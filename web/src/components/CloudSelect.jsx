import React, { useEffect, useMemo, useState } from 'react'
import { COLORS, FONT, KIND_ICONS, accountColor, formatBytes } from '../theme'
import { api } from '../api'
import { Banner, Button, Modal, Spinner } from './ui'

/* Choosing where a file's shards go, and how it is cut.

   A file is cut into shards and each one goes to a different account, so "which
   clouds" is a choice of several — the vault's default, or something else for
   one upload. Both are made with the same list of rows, which is what lives
   here.

   How many clouds are picked settles how many shards there are. How many of
   them it takes to rebuild the file — k — is a second choice, and the one that
   actually says what the file is for. It moves three things at once, and not in
   the same direction:

     storage      n/k, what the file costs once every shard is stored
     tolerance    n − k, how many clouds can go dark before it is lost
     collusion    k, how many accounts must be broken into *together*
                  before what they hold is enough to rebuild anything

   Lowering k buys tolerance with storage and with collusion resistance both.
   2-of-5 survives three clouds going away where 2-of-3 survives one, but two
   accounts still rebuild the file and each of them now holds half of it. That
   is the trade the picker has to show rather than hide. */

/* The default family, mirroring archive.Scheme on the server: clouds come in
   groups of three, and two thirds of the shards rebuild the file. It is what a
   count of clouds names when nobody chooses otherwise — three is 2-of-3, six is
   4-of-6, thirty is 20-of-30 — and it holds storage at 1.5× at every width. */
export const CLOUDS_PER_GROUP = 3
const DATA_PER_GROUP = 2

/* A shard's number is one byte in its header, so a spread stops at 255 shards. */
export const MAX_CLOUDS = 255

export const PARTS_PER_FILE = CLOUDS_PER_GROUP

/* Two shards to rebuild from is the floor, and the server holds it too: with
   one, an account would hold the whole file and splitting would not be
   splitting. */
export const MIN_DATA = 2
const MIN_ACCOUNTS = MIN_DATA

/* The scheme a count of clouds names by itself, or null when it names none.
   Fewer than a full group is the default code with whatever shards there is
   room for, which is what a vault with two clouds connected gets. */
export function schemeFor(count) {
  if (count > 0 && count < CLOUDS_PER_GROUP) return schemeForGroups(1)
  if (count <= 0 || count > MAX_CLOUDS || count % CLOUDS_PER_GROUP !== 0) return null
  return schemeForGroups(count / CLOUDS_PER_GROUP)
}

export function schemeForGroups(groups) {
  return { data: DATA_PER_GROUP * groups, total: CLOUDS_PER_GROUP * groups }
}

/* The scheme to start from for any count of clouds, family or not: the count's
   own scheme where it has one, and otherwise the k that holds 1.5× as closely
   as the count allows. It is a starting point the threshold picker then moves. */
export function defaultSchemeFor(count) {
  const named = schemeFor(count)
  if (named && named.total === count) return named
  if (count < MIN_ACCOUNTS) return null
  return { data: Math.max(MIN_DATA, Math.floor((count * DATA_PER_GROUP) / CLOUDS_PER_GROUP)), total: count }
}

/* Every threshold a given number of clouds can be cut at, widest margin first.
   k runs from two — below which one account holds the file — up to n, where
   there is no margin at all. */
export function thresholdsFor(count) {
  const out = []
  for (let k = MIN_DATA; k <= count; k++) out.push(k)
  return out
}

/* How a scheme is written wherever a person reads it, and what the server reads
   back off the wire. */
export function schemeName(scheme) {
  return scheme ? `${scheme.data}-of-${scheme.total}` : ''
}

/* How many clouds can go dark with the file still readable: n − k. */
export function tolerance(scheme) {
  return scheme ? scheme.total - scheme.data : 0
}

/* What the file costs once every shard is stored: n/k. */
export function storage(scheme) {
  return scheme && scheme.data > 0 ? scheme.total / scheme.data : 0
}

/* Rounded the way it is written next to the other two numbers. */
export function storageName(scheme) {
  const times = storage(scheme)
  if (!times) return ''
  return `${times.toFixed(times % 1 === 0 ? 0 : 2).replace(/0$/, '')}×`
}

/* Whether a scheme is one the server will write. */
export function validScheme(scheme) {
  return !!scheme
    && scheme.data >= MIN_DATA
    && scheme.total >= scheme.data
    && scheme.total <= MAX_CLOUDS
}

/* The widest spread a set of clouds can fill, for a dialog that can only offer
   the default family — the vault-wide default, which names accounts and no
   scheme, so the count has to name one by itself. */
export function maxSelectable(count) {
  if (count < CLOUDS_PER_GROUP) return count
  return Math.min(Math.floor(count / CLOUDS_PER_GROUP) * CLOUDS_PER_GROUP, MAX_CLOUDS)
}

/* Whether a count of clouds is one the server will take with no scheme named.
   The counts this rules out are the ones between groups: a fourth cloud has no
   shard of its own to hold without a fifth and a sixth beside it — unless the
   upload says what four clouds should mean. */
export function validSpread(count) {
  return count === 0 || schemeFor(count) !== null
}

/* Whether a selection is one that can actually be stored with no scheme named. */
export function usableSpread(count) {
  return count >= MIN_ACCOUNTS && validSpread(count)
}

/* Whether a selection is one that can be stored once a scheme is named
   alongside it, which is the question the upload and relocate dialogs ask. */
export function usableCut(count, scheme) {
  return count >= MIN_ACCOUNTS && validScheme(scheme) && scheme.total === count
}

/* How many *distinct* shards of a file are stored, which is the number that
   decides whether it can be rebuilt. */
export function storedParts(shards) {
  return new Set((shards || []).map((s) => s.part)).size
}

/* The scheme a stored file was cut with, from what the index recorded. A file
   written before schemes existed carries nothing and is 2-of-3. */
export function fileScheme(file) {
  if (file?.data_shards > 0 && file?.total_shards > 0) {
    return { data: file.data_shards, total: file.total_shards }
  }
  return schemeForGroups(1)
}

/* The nearest spreads above and below a count that names none, for saying what
   the two ways out of it are. */
export function nextScheme(count) {
  const up = Math.min((Math.floor(count / CLOUDS_PER_GROUP) + 1) * CLOUDS_PER_GROUP, MAX_CLOUDS)
  return schemeFor(up) || schemeForGroups(1)
}

export function previousScheme(count) {
  const down = Math.max(Math.floor(count / CLOUDS_PER_GROUP) * CLOUDS_PER_GROUP, CLOUDS_PER_GROUP)
  return schemeFor(down) || schemeForGroups(1)
}

/* The clouds an upload starts on: the vault's default, and with none set three
   picked at random — which is exactly what the picker then lets the user
   change. A default is taken as it stands rather than made up to three, the
   same way the server takes it, so a default of two clouds does not quietly
   become three and a default of six does not quietly become three either.

   Reachable accounts are drawn first, so a random pick does not send parts at
   an account that is not answering while a working one sits idle. */
export function initialSelection(providers, defaults = []) {
  const connected = new Set(providers.map((p) => p.id))
  const preferred = (defaults || []).filter((id) => connected.has(id))
  // Trimmed only if a disconnected cloud has left the default a shape a file
  // cannot be laid out over — five of a saved six becomes three, not five.
  if (preferred.length > 0) return preferred.slice(0, maxSelectable(preferred.length))

  const pool = [...shuffle(providers.filter((p) => p.online)), ...shuffle(providers.filter((p) => !p.online))]
  return pool.slice(0, PARTS_PER_FILE).map((p) => p.id)
}

function shuffle(items) {
  const out = [...items]
  for (let i = out.length - 1; i > 0; i--) {
    const j = Math.floor(Math.random() * (i + 1));
    [out[i], out[j]] = [out[j], out[i]]
  }
  return out
}

/* The rows themselves.

   `cap` is how many clouds may be picked. A dialog that can name a scheme
   alongside the clouds passes every connected account, because any count of
   them is a spread once the code is spelled out. The vault-wide default cannot
   name one — it stores accounts and nothing else — so it leaves the cap at the
   widest scheme the connected clouds can fill by themselves. */
export function CloudChoice({ providers, selected, onChange, cap: capProp }) {
  const cap = Math.min(capProp ?? maxSelectable(providers.length), MAX_CLOUDS)
  const full = selected.length >= cap

  const toggle = (id) => {
    if (selected.includes(id)) onChange(selected.filter((s) => s !== id))
    else if (!full) onChange([...selected, id])
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '6px' }}>
      {providers.map((provider) => {
        const chosen = selected.includes(provider.id)
        // One shard per cloud, numbered in the order they were picked — which
        // is how the server numbers them too. Six clouds read 1 to 6, and the
        // first four to answer a read are the four the file comes back from.
        const part = chosen ? selected.indexOf(provider.id) + 1 : null
        return (
          <button
            key={provider.id}
            type="button"
            role="checkbox"
            aria-checked={chosen}
            title={chosen ? `Shard ${part} of ${selected.length}` : undefined}
            onClick={() => toggle(provider.id)}
            disabled={!chosen && full}
            style={{
              display: 'flex',
              alignItems: 'center',
              gap: '10px',
              width: '100%',
              padding: '10px 12px',
              textAlign: 'left',
              background: chosen ? COLORS.surfaceRaised : COLORS.bg,
              border: `1px solid ${chosen ? COLORS.accent : COLORS.border}`,
              borderLeft: `3px solid ${accountColor(provider.id)}`,
              borderRadius: '6px',
              color: COLORS.text,
              cursor: !chosen && full ? 'not-allowed' : 'pointer',
              opacity: !chosen && full ? 0.45 : 1,
            }}
          >
            {/* The badge doubles as the shard number, so the row says both
                "chosen" and "this is where shard 2 goes". */}
            <span style={{
              width: '20px',
              height: '20px',
              flexShrink: 0,
              borderRadius: '4px',
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'center',
              fontFamily: FONT.mono,
              fontSize: '10px',
              fontWeight: 700,
              color: chosen ? COLORS.bg : COLORS.textMuted,
              background: chosen ? accountColor(provider.id) : 'transparent',
              border: chosen ? 'none' : `1px dashed ${COLORS.border}`,
            }}>{part || ''}</span>

            <span style={{ fontSize: '13px', flexShrink: 0 }}>{KIND_ICONS[provider.kind] || '☁'}</span>

            <span style={{ flex: 1, minWidth: 0 }}>
              <span style={{
                display: 'block',
                fontFamily: FONT.mono,
                fontSize: '12px',
                overflow: 'hidden',
                textOverflow: 'ellipsis',
                whiteSpace: 'nowrap',
              }}>{provider.name}</span>
              <span style={{
                display: 'block',
                fontFamily: FONT.mono,
                fontSize: '10px',
                color: COLORS.textMuted,
                marginTop: '2px',
              }}>
                {provider.kind} · {provider.shards} shard{provider.shards === 1 ? '' : 's'} · {formatBytes(provider.stored)}
              </span>
            </span>

            <span
              title={provider.online ? 'Reachable' : provider.error || 'Unreachable'}
              style={{
                width: '7px',
                height: '7px',
                borderRadius: '50%',
                flexShrink: 0,
                background: provider.online ? COLORS.success : COLORS.error,
              }}
            />
          </button>
        )
      })}

      {/* A row that cannot be clicked has to say why, or it reads as broken. */}
      {full && providers.length > cap && (
        <p style={{
          margin: '2px 0 0',
          fontFamily: FONT.mono,
          fontSize: '10px',
          color: COLORS.textMuted,
        }}>
          {schemeName(schemeFor(cap))} is as wide as {providers.length} clouds goes.
          Unpick one to swap it for another.
        </p>
      )}
    </div>
  )
}

/* How many of the chosen clouds it should take to rebuild the file.

   The clouds above settle n. This settles k, and with it all three numbers
   underneath — which is why they are printed beside the control rather than
   left for the banner to explain after the fact. Every threshold from two up to
   n is offered: two is the most durable and the easiest to collude against, n
   is the reverse and keeps no spare at all. */
export function ThresholdChoice({ scheme, onChange }) {
  if (!scheme || scheme.total < MIN_ACCOUNTS) return null

  const options = thresholdsFor(scheme.total)
  const suggested = defaultSchemeFor(scheme.total)

  return (
    <div style={{ marginTop: '14px' }}>
      <label style={{
        display: 'flex',
        alignItems: 'center',
        gap: '8px',
        fontFamily: FONT.mono,
        fontSize: '11px',
        color: COLORS.textDim,
      }}>
        <span style={{ flexShrink: 0 }}>Rebuild from</span>
        <select
          value={scheme.data}
          onChange={(e) => onChange({ ...scheme, data: Number(e.target.value) })}
          style={{
            fontFamily: FONT.mono,
            fontSize: '11px',
            padding: '4px 6px',
            background: COLORS.bg,
            color: COLORS.text,
            border: `1px solid ${COLORS.border}`,
            borderRadius: '4px',
          }}
        >
          {options.map((k) => (
            <option key={k} value={k}>
              {k} of {scheme.total}{suggested && k === suggested.data ? ' (usual)' : ''}
            </option>
          ))}
        </select>
        <span style={{ flexShrink: 0 }}>clouds</span>
      </label>

      {/* The three numbers the choice moves, side by side, because reading any
          one of them alone is how a 2-of-5 gets picked for the wrong reason. */}
      <div style={{
        display: 'flex',
        gap: '14px',
        flexWrap: 'wrap',
        margin: '8px 0 0',
        fontFamily: FONT.mono,
        fontSize: '10px',
        color: COLORS.textMuted,
      }}>
        <span>stores {storageName(scheme)}</span>
        <span>survives {tolerance(scheme)} loss{tolerance(scheme) === 1 ? '' : 'es'}</span>
        <span>{scheme.data} must collude</span>
      </div>
    </div>
  )
}

/* What a selection means, said before the upload rather than after it.

   `moving` switches the two sentences that differ between putting a file
   somewhere for the first time and picking it up off one cloud and setting it
   down on another — the second of which can also erase a spare shard, because
   the chosen clouds may not have room for all of them. */
function SelectionNote({ providers, selected, scheme, moving = false }) {
  if (selected.length < MIN_ACCOUNTS) {
    return (
      <Banner tone="error">
        Choose at least {MIN_ACCOUNTS} clouds. It takes at least two shards to rebuild a file, so
        one cloud on its own could not — and would be the only thing standing between you and
        losing it.
      </Banner>
    )
  }
  if (!validScheme(scheme)) {
    return (
      <Banner tone="error">
        {selected.length} clouds cannot be cut at {scheme ? scheme.data : '?'} — a file has to take
        at least {MIN_DATA} shards to rebuild, and no more than the {selected.length} it is cut
        into.
      </Banner>
    )
  }

  const offline = providers.filter((p) => selected.includes(p.id) && !p.online)
  if (offline.length) {
    return (
      <Banner tone="warn">
        {offline.map((p) => p.name).join(', ')} {offline.length === 1 ? 'is' : 'are'} not answering.
        {moving
          ? ' Parts bound for it will stay where they are, and nothing is lost — run the move again once it is back.'
          : ' The upload will go ahead if the others accept their parts, and that part will be missing.'}
      </Banner>
    )
  }

  // No spare at all: every cloud has to answer for the file to come back, which
  // is a real choice but not one to make by accident.
  if (tolerance(scheme) === 0) {
    return (
      <Banner tone="warn">
        {schemeName(scheme)} keeps no spare: all {scheme.total} clouds have to answer to rebuild
        the file{moving ? ', and any shard the chosen clouds have no room for is erased' : ''}. It
        is the cheapest at {storageName(scheme)} and the hardest to collude against, and one cloud
        going away loses the file.
      </Banner>
    )
  }

  // A low threshold is durable and cheap to collude against at once, and the
  // second half of that is the part a storage figure does not say.
  if (scheme.data < DATA_PER_GROUP * scheme.total / CLOUDS_PER_GROUP) {
    return (
      <Banner tone="warn">
        {schemeName(scheme)}: any {scheme.data} of {scheme.total} clouds rebuild the file, so it
        survives {tolerance(scheme)} of them going away — but {scheme.data} of them together are
        also enough for someone else to rebuild it, and each one holds a{' '}
        {scheme.data === 2 ? 'half' : `1/${scheme.data}`} of it. It stores {storageName(scheme)}.
      </Banner>
    )
  }

  return (
    <Banner tone="success">
      {schemeName(scheme)}: one shard on each of {scheme.total} clouds, any {scheme.data} of
      which rebuild the file. {tolerance(scheme)} of them could go away before it was at risk,
      and an attacker would need {scheme.data} of them together before what they hold is enough
      to rebuild anything. It stores {storageName(scheme)}.
    </Banner>
  )
}

/* The dialog every upload passes through: which files are going up, which three
   clouds they are being scattered over, and the chance to change that before a
   single byte leaves the machine. */
export function UploadDestination({ files, path, providers, defaults, onUpload, onClose, onChanged }) {
  const [selected, setSelected] = useState(() => initialSelection(providers, defaults))
  const [threshold, setThreshold] = useState(null)
  const [remember, setRemember] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  /* The clouds settle n; k is the suggested one until somebody moves it, and a
     k left over from a wider selection is pulled back inside the new one rather
     than left naming a scheme that no longer exists. */
  const scheme = useMemo(() => {
    const suggested = defaultSchemeFor(selected.length)
    if (!suggested) return null
    if (threshold === null) return suggested
    return { data: Math.min(Math.max(threshold, MIN_DATA), selected.length), total: selected.length }
  }, [selected.length, threshold])

  // Only a scheme the count would not have named by itself has to be sent; the
  // rest leave the server's own default in charge, as before.
  const named = scheme && (!schemeFor(scheme.total) || schemeFor(scheme.total).data !== scheme.data)

  const usingDefault = useMemo(() => {
    const set = new Set(defaults || [])
    return set.size > 0 && selected.length === set.size && selected.every((id) => set.has(id))
  }, [defaults, selected])

  const names = files.map((f) => f.name)
  const total = files.reduce((sum, f) => sum + f.size, 0)

  const submit = async () => {
    setBusy(true)
    setError(null)
    try {
      // Saved first: an upload that fails should not also lose the choice the
      // user just made about where their files live from now on.
      if (remember) {
        await api.setDefaultAccounts(selected)
        onChanged?.()
      }
      onUpload(selected, named ? schemeName(scheme) : '')
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }

  return (
    <Modal
      title={names.length === 1 ? `Upload ${names[0]}` : `Upload ${names.length} files`}
      subtitle={`${formatBytes(total)} into ${path} — cut ${
        schemeName(scheme) || 'across the chosen clouds'
      }, one encrypted shard per cloud`}
      onClose={busy ? undefined : onClose}
      width={460}
    >
      {error && <Banner tone="error">{error}</Banner>}

      <p style={{
        margin: '0 0 12px',
        fontFamily: FONT.sans,
        fontSize: '12px',
        color: COLORS.textMuted,
        lineHeight: 1.6,
      }}>
        {usingDefault
          ? 'Your default clouds are selected. Change them for this upload if you want.'
          : (defaults?.length
            ? 'Chosen for this upload, in place of your default clouds.'
            : `No default is set, so ${PARTS_PER_FILE} clouds were picked at random. Change them if you want.`)}
      </p>

      <CloudChoice
        providers={providers}
        selected={selected}
        onChange={setSelected}
        cap={providers.length}
      />

      <ThresholdChoice scheme={scheme} onChange={(next) => setThreshold(next.data)} />

      <div style={{ marginTop: '14px' }}>
        <SelectionNote providers={providers} selected={selected} scheme={scheme} />
      </div>

      <label style={{
        display: 'flex',
        alignItems: 'center',
        gap: '8px',
        margin: '10px 0 16px',
        fontFamily: FONT.mono,
        fontSize: '11px',
        color: COLORS.textDim,
        cursor: 'pointer',
      }}>
        <input
          type="checkbox"
          checked={remember}
          onChange={(e) => setRemember(e.target.checked)}
          disabled={!usableSpread(selected.length)}
          title={usableSpread(selected.length)
            ? undefined
            : `A vault-wide default names clouds and no scheme, so it has to be a count that names one by itself — ${CLOUDS_PER_GROUP}, ${2 * CLOUDS_PER_GROUP}, ${3 * CLOUDS_PER_GROUP}…`}
          style={{ accentColor: COLORS.accent, width: '15px', height: '15px', minHeight: 0 }}
        />
        Make this the default for the whole vault
      </label>

      <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end' }}>
        <Button type="button" variant="ghost" onClick={onClose} disabled={busy}>Cancel</Button>
        <Button
          type="button"
          variant="primary"
          onClick={submit}
          disabled={busy || !usableCut(selected.length, scheme)}
        >
          {busy ? <Spinner size={10} /> : null}
          ↑ Upload to {selected.length} cloud{selected.length === 1 ? '' : 's'}
        </Button>
      </div>
    </Modal>
  )
}

/* The same choice, made once for the whole vault. */
export function DefaultClouds({ providers, defaults, onClose, onChanged, zIndex }) {
  const [selected, setSelected] = useState(() => (defaults || []).filter(
    (id) => providers.some((p) => p.id === id)))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  const save = async (accounts) => {
    setBusy(true)
    setError(null)
    try {
      await api.setDefaultAccounts(accounts)
      onChanged()
      onClose()
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }

  return (
    <Modal
      title="Default clouds"
      subtitle="Where uploads go unless they choose otherwise. Every upload can still pick its own."
      onClose={busy ? undefined : onClose}
      width={460}
      zIndex={zIndex}
    >
      {error && <Banner tone="error">{error}</Banner>}

      <CloudChoice providers={providers} selected={selected} onChange={setSelected} />

      {selected.length > 0 && !usableSpread(selected.length) && (
        <div style={{ marginTop: '14px' }}>
          <Banner tone="error">
            {selected.length < MIN_ACCOUNTS
              ? `Choose at least ${MIN_ACCOUNTS} clouds, or none at all to let every upload pick its own.`
              : `Choose clouds in groups of ${CLOUDS_PER_GROUP} — ${
                previousScheme(selected.length).total} or ${nextScheme(selected.length).total}, not ${
                selected.length}.`}
          </Banner>
        </div>
      )}

      <p style={{
        margin: '14px 0 16px',
        fontFamily: FONT.sans,
        fontSize: '12px',
        color: COLORS.textMuted,
        lineHeight: 1.6,
      }}>
        With no default, each file picks {PARTS_PER_FILE} clouds of its own at random — which is
        what spreads a vault evenly over more than {PARTS_PER_FILE} accounts. Saving a default of
        6, 9 or more — any group of {CLOUDS_PER_GROUP} — cuts every upload{' '}
        {schemeName(schemeForGroups(2))}, {schemeName(schemeForGroups(3))} and so on, for the same
        storage and a group's worth of extra margin each time.
      </p>

      <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end', flexWrap: 'wrap' }}>
        <Button type="button" variant="ghost" onClick={() => save([])} disabled={busy}>
          Pick per upload
        </Button>
        <Button type="button" variant="ghost" onClick={onClose} disabled={busy}>Cancel</Button>
        <Button
          type="button"
          variant="primary"
          onClick={() => save(selected)}
          disabled={busy || !usableSpread(selected.length)}
        >
          {busy ? <Spinner size={10} /> : null}Save default
        </Button>
      </div>
    </Modal>
  )
}

/* How long to sit on a change of selection before asking the server what it
   would cost. Clicking through four clouds should be one question, not four. */
const PREVIEW_DEBOUNCE_MS = 220

/* Moving something that is already stored onto a different set of clouds.

   The same three rows as an upload, with one thing added that only makes sense
   here: what the change would actually cost. A part already sitting on a cloud
   that is staying does not move, so swapping one of three is one part on the
   wire rather than a whole file — and that is worth saying before the button is
   pressed rather than after. The server answers it out of the encrypted index
   alone, without contacting any account, so the estimate is free and exact.

   `target` is one file or folder; `targets` is a list of them, which is what a
   selection of rows comes to. Both are the same dialog: the estimates are
   priced together and read as one number, because "what would this cost" is
   one question however many things were picked. */
export function RelocateClouds({ target, targets, title, subtitle, current, providers, onClose, onDone }) {
  const [selected, setSelected] = useState(() => (current || []).filter(
    (id) => providers.some((p) => p.id === id)))
  const [threshold, setThreshold] = useState(null)
  const [plan, setPlan] = useState(null)
  const [planning, setPlanning] = useState(false)
  const [busy, setBusy] = useState(false)
  const [moved, setMoved] = useState(0)
  const [error, setError] = useState(null)
  const [report, setReport] = useState(null)

  const scope = (targets && targets.length ? targets : [target]).filter(Boolean)

  const scheme = useMemo(() => {
    const suggested = defaultSchemeFor(selected.length)
    if (!suggested) return null
    if (threshold === null) return suggested
    return { data: Math.min(Math.max(threshold, MIN_DATA), selected.length), total: selected.length }
  }, [selected.length, threshold])

  const named = scheme && (!schemeFor(scheme.total) || schemeFor(scheme.total).data !== scheme.data)
  const cut = named ? schemeName(scheme) : ''
  const enough = usableCut(selected.length, scheme)
  const key = `${selected.join(',')}|${cut}`
  const scopeKey = scope.map((t) => t.id || t.path).join('\n')

  useEffect(() => {
    if (!enough || report) {
      setPlan(null)
      return undefined
    }

    /* Every change cancels the question the last one asked, so a slow answer
       can never arrive after — and describe — a selection that has moved on. */
    const controller = new AbortController()
    setPlanning(true)
    const timer = setTimeout(() => {
      /* Together rather than one after another: each estimate is read out of
         the index with no account contacted, so a selection of thirty is
         thirty cheap answers and no reason to wait for them in turn. */
      Promise.all(scope.map((t) => api.relocate({
        ...t, accounts: selected, scheme: cut, preview: true, signal: controller.signal,
      })))
        .then((plans) => { setPlan(mergePlans(plans)); setError(null) })
        .catch((err) => {
          if (err.name === 'AbortError') return
          setPlan(null)
          setError(err.message)
        })
        .finally(() => { if (!controller.signal.aborted) setPlanning(false) })
    }, PREVIEW_DEBOUNCE_MS)

    return () => { clearTimeout(timer); controller.abort() }
    // The selection and the scope are compared by value: a new array holding
    // the same ids is the same question.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, scopeKey, enough, report])

  const submit = async () => {
    setBusy(true)
    setMoved(0)
    setError(null)
    try {
      /* One at a time, unlike the estimates: this one really does copy parts
         between accounts, and running the whole selection at once is how a
         provider starts refusing. Each file commits on its own, so a failure
         partway leaves everything before it moved and everything after it
         where it was — which is exactly what running it again then finishes. */
      const reports = []
      for (const t of scope) {
        reports.push(await api.relocate({ ...t, accounts: selected, scheme: cut }))
        setMoved(reports.length)
      }
      setReport(mergeReports(reports))
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  const close = () => {
    // The listing's part badges are drawn from the index, which has changed.
    if (report) onDone?.()
    onClose()
  }

  return (
    <Modal
      title={title}
      subtitle={subtitle}
      onClose={busy ? undefined : close}
      width={480}
    >
      {error && <Banner tone="error">{error}</Banner>}

      {report ? (
        <RelocationOutcome report={report} onClose={close} />
      ) : (
        <>
          <p style={{
            margin: '0 0 12px',
            fontFamily: FONT.sans,
            fontSize: '12px',
            color: COLORS.textMuted,
            lineHeight: 1.6,
          }}>
            Shards already on a cloud you keep stay exactly where they are — only the rest are
            carried across, still encrypted, without ever being rebuilt. Changing the code a file
            is cut with is different — a wider spread, or a different threshold — because no
            shard of the old file is a shard of the new one, so it is gathered and written out
            again. The estimate below says which is happening.
          </p>

          <CloudChoice
            providers={providers}
            selected={selected}
            onChange={setSelected}
            cap={providers.length}
          />

          <ThresholdChoice scheme={scheme} onChange={(next) => setThreshold(next.data)} />

          <div style={{ marginTop: '14px' }}>
            <SelectionNote providers={providers} selected={selected} scheme={scheme} moving />
          </div>

          {enough && <RelocationCost plan={plan} planning={planning} />}

          <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end', marginTop: '16px' }}>
            <Button type="button" variant="ghost" onClick={close} disabled={busy}>Cancel</Button>
            <Button
              type="button"
              variant="primary"
              onClick={submit}
              disabled={busy || !enough || (plan !== null && !worthDoing(plan))}
            >
              {busy ? (
                <>
                  <Spinner size={10} color={COLORS.bg} />
                  {scope.length > 1 ? ` Moving ${moved + 1} of ${scope.length}…` : ' Moving…'}
                </>
              ) : '⇄ Move the shards'}
            </Button>
          </div>
        </>
      )}
    </Modal>
  )
}

/* Whether a plan has anything in it worth pressing the button for. A change of
   scheme is work — the file comes down and goes back up — even though it moves
   no single shard, so counting only shards moved and erased would grey out the
   one plan that costs the most. */
function worthDoing(plan) {
  return plan.moves > 0 || plan.drops > 0 || plan.recoded > 0
}

/* Several estimates read as one. Every field is a count of files, of parts or
   of bytes, so they add — and the whole point of pricing a selection together
   is a single "12 parts to move, 400 MB" rather than a column of them. */
function mergePlans(plans) {
  return plans.reduce((all, plan) => ({
    moves: all.moves + (plan.moves || 0),
    recoded: all.recoded + (plan.recoded || 0),
    recode_bytes: all.recode_bytes + (plan.recode_bytes || 0),
    bytes: all.bytes + (plan.bytes || 0),
    total: all.total + (plan.total || 0),
    unchanged: all.unchanged + (plan.unchanged || 0),
    drops: all.drops + (plan.drops || 0),
    warnings: [...all.warnings, ...(plan.warnings || [])],
  }), { moves: 0, recoded: 0, recode_bytes: 0, bytes: 0, total: 0, unchanged: 0, drops: 0, warnings: [] })
}

function mergeReports(reports) {
  return reports.reduce((all, report) => ({
    relocated: all.relocated + (report.relocated || 0),
    recoded: all.recoded + (report.recoded || 0),
    parts_moved: all.parts_moved + (report.parts_moved || 0),
    parts_dropped: all.parts_dropped + (report.parts_dropped || 0),
    bytes: all.bytes + (report.bytes || 0),
    total: all.total + (report.total || 0),
    unchanged: all.unchanged + (report.unchanged || 0),
    partial: all.partial + (report.partial || 0),
    failed: all.failed + (report.failed || 0),
    warnings: [...all.warnings, ...(report.warnings || [])],
  }), {
    relocated: 0, recoded: 0, parts_moved: 0, parts_dropped: 0, bytes: 0,
    total: 0, unchanged: 0, partial: 0, failed: 0, warnings: [],
  })
}

/* What the chosen clouds would cost, from the index alone. */
function RelocationCost({ plan, planning }) {
  if (!plan) {
    return (
      <p style={{
        margin: '12px 0 0', display: 'flex', alignItems: 'center', gap: '7px',
        fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textMuted,
      }}>
        {planning ? <><Spinner size={10} /> working out what would move…</> : ' '}
      </p>
    )
  }

  if (!worthDoing(plan)) {
    return (
      <div style={{ marginTop: '12px' }}>
        <Banner tone="success">
          Already there — every part is on one of those clouds. Nothing to move.
        </Banner>
      </div>
    )
  }

  return (
    <div style={{
      marginTop: '12px',
      padding: '11px 13px',
      background: COLORS.bg,
      border: `1px solid ${COLORS.border}`,
      borderRadius: '6px',
      fontFamily: FONT.mono,
      fontSize: '11px',
      color: COLORS.textDim,
      lineHeight: 1.7,
    }}>
      <div style={{ color: COLORS.text }}>
        {/* With nothing to move but a part to erase — narrowing three clouds to
            two — "0 parts to move" is not what the change is, so it leads with
            what actually happens instead. */}
        {/* A change of scheme is not a move: no shard of the old file is a
            shard of the new one, so the file is rebuilt rather than carried.
            Saying "parts to move" would describe the wrong operation and,
            worse, the wrong bill. */}
        {plan.recoded > 0
          ? (
            <>
              {plan.recoded} file{plan.recoded === 1 ? '' : 's'} to rebuild
              {plan.recode_bytes > 0 && <> · {formatBytes(plan.recode_bytes)}</>}
            </>
          )
          : plan.moves > 0
            ? (
              <>
                {plan.moves} shard{plan.moves === 1 ? '' : 's'} to move
                {plan.bytes > 0 && <> · {formatBytes(plan.bytes)}</>}
              </>
            )
            : <>Nothing to move</>}
        {planning && <> …</>}
      </div>
      <div style={{ color: COLORS.textMuted }}>
        {plan.total} file{plan.total === 1 ? '' : 's'} in scope
        {plan.unchanged > 0 && <>, {plan.unchanged} already in place</>}
        {plan.drops > 0 && (
          <span style={{ color: COLORS.warn }}>
            {' '}· {plan.drops} spare shard{plan.drops === 1 ? '' : 's'} erased
          </span>
        )}
      </div>
      {(plan.warnings || []).slice(0, 3).map((w, i) => (
        <div key={i} style={{ color: COLORS.warn }}>{w}</div>
      ))}
    </div>
  )
}

/* What the move actually did. It stays on screen rather than closing on
   success, because a partial move — one cloud not answering — is a normal
   outcome worth reading, and running it again is what finishes it. */
function RelocationOutcome({ report, onClose }) {
  const stuck = report.partial + report.failed

  return (
    <>
      <Banner tone={stuck ? 'warn' : 'success'}>
        {stuck
          ? `${stuck} file(s) did not fully move. Nothing was lost — their parts are still where
             they were. Try again once the accounts are answering.`
          : report.recoded > 0
            ? `Rebuilt ${report.recoded} file(s) under the new scheme${
              report.bytes ? `, ${formatBytes(report.bytes)}` : ''}.`
            : `Moved ${report.parts_moved} shard(s)${report.bytes ? `, ${formatBytes(report.bytes)}` : ''},
               across ${report.relocated} file(s).`}
      </Banner>

      <div style={{
        padding: '11px 13px',
        background: COLORS.bg,
        border: `1px solid ${COLORS.border}`,
        borderRadius: '6px',
        fontFamily: FONT.mono,
        fontSize: '11px',
        color: COLORS.textDim,
        lineHeight: 1.7,
      }}>
        <div>{report.total} file(s) in scope · {report.unchanged} already in place</div>
        {report.parts_dropped > 0 && (
          <div style={{ color: COLORS.warn }}>
            {report.parts_dropped} spare shard(s) erased — the chosen clouds had no room for them.
          </div>
        )}
      </div>

      {(report.warnings || []).length > 0 && (
        <div style={{ marginTop: '10px', maxHeight: '160px', overflowY: 'auto' }}>
          <Banner tone="warn">
            {report.warnings.map((w, i) => <div key={i}>{w}</div>)}
          </Banner>
        </div>
      )}

      <div style={{ display: 'flex', justifyContent: 'flex-end', marginTop: '4px' }}>
        <Button type="button" variant="primary" onClick={onClose}>Done</Button>
      </div>
    </>
  )
}
