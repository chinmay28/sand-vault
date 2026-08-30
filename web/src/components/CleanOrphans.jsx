import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { COLORS, FONT, accountColor, formatBytes } from '../theme'
import { api } from '../api'
import { useOrphanEraseProgress } from '../hooks'
import { Banner, Button, Modal, Spinner } from './ui'
import ReattachShards from './ReattachShards'
import CleanLeftovers from './CleanLeftovers'

/* Parts left on a cloud that no file points at any more.

   They come from a delete that could not finish. Erasing a file erases its
   parts from the accounts holding them — the ones connected at the time. A
   cloud that is disconnected while files are deleted keeps its share of them,
   and connecting it back gives that account a brand new internal ID, so the
   vault has no way of knowing it is the account those parts were erased from.
   Nothing ever goes back for them: unreadable, unreferenced, and still counting
   against the quota.

   This is the panel that says so. It is deliberately per account and per
   archive rather than one number with one button, because the only honest thing
   that can be said about an abandoned archive is how big it is and where it is
   — what it used to be lived in the index that stopped mentioning it. So the
   figure somebody is agreeing to is the room they get back, and the rows are
   there to be un-ticked by anyone who would rather not.

   Everything the scan refuses to offer is shown all the same, greyed out and
   with its reason beside it. An account another vault has been writing to is
   the case that matters: its parts look exactly like orphans and are not. */
export default function CleanOrphans({ scan: initialScan, zIndex = 100, onClose, onSwept }) {
  const [scan, setScan] = useState(initialScan)
  const [excluded, setExcluded] = useState(() => new Set())
  const [busy, setBusy] = useState(false)
  const [sweeping, setSweeping] = useState(false)
  const [error, setError] = useState(null)
  const [report, setReport] = useState(null)

  /* The sweep is one POST that answers only at the end — for a vault where
     somebody has been deleting films, minutes of it. The server counts the
     objects beside the running request, and this is the asking end, polling
     only while the sweep is in flight. Null until the first answer; a total
     of 0 means the sweep is still listing every account to decide what goes,
     which it does before its first delete so that what is erased is what is
     abandoned now. */
  const at = useOrphanEraseProgress(sweeping)

  const offered = useMemo(() => (scan?.items || []).filter((item) => item.deletable), [scan])
  const withheld = useMemo(() => (scan?.items || []).filter((item) => !item.deletable), [scan])
  const key = (item) => `${item.provider_id}:${item.archive_id}`

  const chosen = offered.filter((item) => !excluded.has(key(item)))
  const chosenBytes = chosen.reduce((sum, item) => sum + (item.bytes || 0), 0)
  const chosenObjects = chosen.reduce((sum, item) => sum + (item.objects || 0), 0)

  const toggle = (item) => {
    setExcluded((current) => {
      const next = new Set(current)
      if (next.has(key(item))) next.delete(key(item))
      else next.add(key(item))
      return next
    })
  }

  const sweep = async () => {
    setError(null)
    setBusy(true)
    setSweeping(true)
    try {
      /* Nothing unticked means "all of it", and is sent as an empty list rather
         than as the rows on screen — the list is capped for reading, and naming
         only what fitted would quietly leave the rest behind. Untick anything
         and the request becomes exactly what is ticked. */
      const targets = excluded.size === 0
        ? []
        : chosen.map((item) => ({ provider_id: item.provider_id, archive_id: item.archive_id }))
      const result = await api.sweepOrphans({ targets })
      setReport(result)
      onSwept?.()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
      setSweeping(false)
    }
  }

  const rescan = async () => {
    setError(null)
    setBusy(true)
    try {
      const fresh = await api.orphanScan()
      setScan(fresh)
      setExcluded(new Set())
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  if (report) {
    return (
      <Modal title="Swept" onClose={onClose} width={560} zIndex={zIndex}>
        <Banner tone="success">
          {report.deleted} object{report.deleted === 1 ? '' : 's'} erased across{' '}
          {report.archives} archive{report.archives === 1 ? '' : 's'}, freeing{' '}
          {formatBytes(report.bytes)} on your clouds.
        </Banner>

        {report.skipped?.length > 0 && (
          <Banner tone="info">
            Some of what was named had stopped being abandoned by the time the sweep ran, and was
            left where it is:
            <span style={preText}>{report.skipped.join('\n')}</span>
          </Banner>
        )}
        {report.warnings?.length > 0 && (
          <Banner tone="warn">
            <span style={preText}>{report.warnings.join('\n')}</span>
          </Banner>
        )}

        <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
          <Button variant="primary" onClick={onClose}>Done</Button>
        </div>
      </Modal>
    )
  }

  return (
    <Modal
      title="Parts nothing points at"
      subtitle="Storage your clouds are holding for files this vault no longer has."
      onClose={busy ? undefined : onClose}
      width={620}
      zIndex={zIndex}
    >
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

      <p style={paragraph}>
        Deleting a file erases its parts from the clouds holding them — the ones connected at the
        time. Disconnect a cloud, delete files without it, and its share of them stays put; connect
        it back and it arrives as a new account, so nothing ever goes looking again. These are what
        was left: encrypted, unreadable, referred to by nothing, and still taking up room.
      </p>

      <AccountBreakdown accounts={scan?.accounts || []} />

      {scan?.blocked?.length > 0 && (
        <Banner tone="warn">
          Nothing here is being offered for deletion:
          <span style={preText}>{scan.blocked.map((reason) => `• ${reason}`).join('\n')}</span>
        </Banner>
      )}

      {offered.length > 0 && (
        <>
          <SectionLabel>
            {offered.length} abandoned archive{offered.length === 1 ? '' : 's'}
            {scan?.items_truncated > 0 ? ` (${scan.items_truncated} more not listed)` : ''}
          </SectionLabel>
          <p style={{ ...paragraph, marginBottom: '10px' }}>
            One row per archive that was written and forgotten. There is nothing to say about what
            each one was — the file name, its folder and its date all lived in the index that
            stopped mentioning it. Untick anything you would rather keep.
          </p>
          <div style={listStyle}>
            {offered.map((item) => (
              <ArchiveRow
                key={key(item)}
                item={item}
                checked={!excluded.has(key(item))}
                onToggle={() => toggle(item)}
                disabled={busy}
              />
            ))}
          </div>
        </>
      )}

      {withheld.length > 0 && (
        <>
          <SectionLabel>Left alone</SectionLabel>
          <div style={listStyle}>
            {withheld.map((item) => (
              <ArchiveRow key={key(item)} item={item} checked={false} disabled />
            ))}
          </div>
        </>
      )}

      {scan?.warnings?.length > 0 && (
        <Banner tone="warn">
          <span style={preText}>{scan.warnings.join('\n')}</span>
        </Banner>
      )}

      {scan?.items_truncated > 0 && (
        <Banner tone="info">
          {scan.items_truncated} further archive{scan.items_truncated === 1 ? '' : 's'} were found
          and are not listed above — the list is capped for reading, not for counting.
          {excluded.size === 0
            ? ' Sweeping takes those too.'
            : ' Because you have unticked something, only the ticked rows above will be swept.'}
        </Banner>
      )}

      {/* What the button used to hide: how far the erasing has got, against
          how much there is. The first stretch has no denominator — the sweep
          lists every account again before its first delete, so that what it
          erases is what is abandoned now rather than when the scan ran — and
          saying so beats a bar pretending to know. */}
      {sweeping && (
        <div style={{ marginBottom: '16px' }}>
          <div style={{
            display: 'flex', alignItems: 'center', gap: '8px', marginBottom: '8px',
            fontFamily: FONT.mono, fontSize: '11.5px', color: COLORS.textDim,
          }}>
            <Spinner size={11} />
            <span>
              {at?.total > 0
                ? `Erased ${at.done} of ${at.total} objects`
                : 'Checking every cloud once more, so what goes is what is abandoned now…'}
            </span>
          </div>
          <div style={{ height: '3px', background: COLORS.border, borderRadius: '2px', overflow: 'hidden' }}>
            <div style={{
              height: '100%',
              width: `${at?.total > 0 ? Math.max(4, Math.min(100, (at.done / at.total) * 100)) : 4}%`,
              background: COLORS.accent,
              transition: 'width 0.2s ease',
            }} />
          </div>
        </div>
      )}

      <div style={{ display: 'flex', justifyContent: 'space-between', gap: '8px', flexWrap: 'wrap' }}>
        <Button variant="ghost" onClick={rescan} disabled={busy}>Scan again</Button>
        <div style={{ display: 'flex', gap: '8px', flexWrap: 'wrap' }}>
          <Button variant="ghost" onClick={onClose} disabled={busy}>Not now</Button>
          <Button
            variant="primary"
            onClick={sweep}
            disabled={busy || chosen.length === 0 || scan?.blocked?.length > 0}
          >
            {sweeping ? <Spinner size={12} color={COLORS.bg} /> : null}
            {sweeping
              ? (at?.total > 0 ? `Erasing ${at.done} of ${at.total}…` : 'Erasing…')
              : `Erase ${chosenObjects} object${chosenObjects === 1 ? '' : 's'} · ${formatBytes(chosenBytes)}`}
          </Button>
        </div>
      </div>
    </Modal>
  )
}

/* The same two panels, opened on purpose rather than when the app brings them up.

   Everything above arrives as news. The app scans when the set of connected
   clouds changes and puts what it found in a banner over the file list, which
   is the right shape for news and the wrong shape for a door: a banner can be
   waved away, and it is only ever there when there is something to say. After
   either of those, these panels could not be reached at all — somebody who
   dismissed the notice, or who simply wants to know whether anything is adrift
   before the app volunteers it, had nothing to click.

   So this is the door, and it hangs off the vault settings list. It runs its
   own scan when it opens instead of reusing whatever the app last saw, because
   the whole point of asking on purpose is to be told what is true now. That
   scan is a listing per account and is slow enough to be worth not running
   until it is asked for, which is why the line in the settings list reports no
   figure of its own — it is a question, not a reading.

   The listing answers three separate questions at once (§3.7.1, §3.7.2 and
   §3.7.3), so what is found is named here and handed to whichever panel already
   knows what to do about it. Nothing is decided in this component.

   The third of them is not about a cloud at all: SAND writes its working files
   into the folder the vault file lives in, and an upload spool left by a
   process that was killed is the whole file that was being uploaded. It rides
   along with this scan because it is the same question — something is being
   held that no file in this vault needs — and because it costs a directory
   read, which is nothing beside the listings. */
export function StrayParts({ zIndex = 100, onClose, onChanged }) {
  const [scan, setScan] = useState(null)
  const [busy, setBusy] = useState(true)
  const [error, setError] = useState(null)
  const [open, setOpen] = useState(null)

  /* Whether the panel that was open changed anything. Closing one that did
     puts this back in front of a scan that is now wrong, so the scan is run
     again; closing one that was only looked at leaves the figures alone rather
     than spending another listing to confirm them. */
  const acted = useRef(false)

  const rescan = useCallback(async () => {
    setError(null)
    setBusy(true)
    try {
      setScan(await api.orphanScan())
    } catch (err) {
      setScan(null)
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }, [])

  useEffect(() => { rescan() }, [rescan])

  const back = () => {
    setOpen(null)
    if (!acted.current) return
    acted.current = false
    rescan()
  }

  const done = () => {
    acted.current = true
    onChanged?.()
  }

  if (open === 'sweep' && scan) {
    return <CleanOrphans scan={scan} zIndex={zIndex} onClose={back} onSwept={done} />
  }

  if (open === 'reattach' && scan) {
    return <ReattachShards scan={scan} zIndex={zIndex} onClose={back} onDone={done} />
  }

  if (open === 'leftovers' && scan?.leftovers) {
    return <CleanLeftovers scan={scan.leftovers} zIndex={zIndex} onClose={back} onSwept={done} />
  }

  /* An account that would not answer is not a small caveat here. A part is
     abandoned only if every account agrees it is, so one silent cloud makes
     "nothing adrift" a thing this cannot say. */
  const unheard = (scan?.accounts || []).filter((account) => account.error)
  const leftovers = scan?.leftovers
  const nothing = scan && !scan.found && scan.reattachable === 0 && !leftovers?.found

  return (
    <Modal
      title="Stray parts"
      subtitle="What your clouds — and this machine — are holding that no file in this vault needs"
      onClose={onClose}
      width={520}
      zIndex={zIndex}
    >
      {error && <Banner tone="error">{error}</Banner>}

      {busy && (
        <div style={{
          display: 'flex', alignItems: 'center', gap: '10px',
          padding: '18px 2px', fontFamily: FONT.sans, fontSize: '12px', color: COLORS.textMuted,
        }}>
          <Spinner size={14} />
          Asking every connected cloud what it is holding, and this machine what SAND has left in
          the vault’s own folder. The clouds are a full listing per account, so it takes about as
          long as the slowest of them.
        </div>
      )}

      {!busy && scan && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: '8px', marginBottom: '16px' }}>
          {/* The repair first, and the sweep second, on the two occasions both
              are on offer: one adds a spare part back to a file that is short
              of one, the other erases something. Putting the deletion at the
              top of a list somebody opened out of curiosity is the wrong way
              round. */}
          {scan.reattachable > 0 && (
            <Finding
              icon="🧩"
              title={`${scan.reattachable} part${scan.reattachable === 1 ? '' : 's'} of `
                + `${scan.stray_files} file${scan.stray_files === 1 ? '' : 's'} `
                + `${scan.reattachable === 1 ? 'is' : 'are'} on your clouds unrecorded`}
              body={'A disconnected cloud takes its records with it, and reconnecting the storage '
                + 'never brings them back on its own. Putting them back moves no data at all.'}
              action="Put them back"
              onClick={() => setOpen('reattach')}
            />
          )}

          {scan.found && (
            <Finding
              icon="🧹"
              title={`${formatBytes(scan.bytes)} across ${scan.objects} `
                + `part${scan.objects === 1 ? '' : 's'} belongs to no file in this vault`}
              body={'What each one used to be is not knowable — that lived in the index that '
                + 'stopped naming it. All that can be said is how much room it is taking and '
                + 'which cloud is holding it.'}
              action="Take a look"
              onClick={() => setOpen('sweep')}
            />
          )}

          {/* Last of the three, and the only one that is not about a cloud.
              It is said after them rather than before because it is SAND's own
              mess rather than anything to do with the files — but it is very
              often the biggest number on this screen, because one interrupted
              upload leaves the whole file it was sending. */}
          {leftovers?.found && (
            <Finding
              icon="🧽"
              title={`${formatBytes(leftovers.bytes)} of working files sit in this vault’s own `
                + 'folder on this machine'}
              body={'An upload is spooled to disk in full before it is sent, and a process that '
                + 'was killed never gets to delete its copy. These are SAND’s scratch files, not '
                + 'your files: erasing them frees room here and changes nothing in the vault.'}
              action="Tidy up"
              onClick={() => setOpen('leftovers')}
            />
          )}

          {nothing && unheard.length === 0 && (
            <Banner tone="success">
              Nothing adrift. Every part your clouds are holding belongs to a file this vault
              still has, and its own folder on this machine is clear of working files.
            </Banner>
          )}

          {nothing && unheard.length > 0 && (
            <Banner tone="warn">
              Nothing adrift on the clouds that answered, and nothing left lying about on this
              machine — but these clouds did not answer, and a part counts as abandoned only when
              every account agrees it is:
              <span style={preText}>
                {unheard.map((account) => `• ${account.name} — ${account.error}`).join('\n')}
              </span>
            </Banner>
          )}

          {!nothing && unheard.length > 0 && (
            <Banner tone="warn">
              Some accounts could not be listed, so this is what the rest are holding rather than
              the whole answer:
              <span style={preText}>
                {unheard.map((account) => `• ${account.name} — ${account.error}`).join('\n')}
              </span>
            </Banner>
          )}

          {scan.blocked?.length > 0 && (
            <Banner tone="info">
              Nothing will be offered for deletion while this holds:
              <span style={preText}>{scan.blocked.map((reason) => `• ${reason}`).join('\n')}</span>
            </Banner>
          )}
        </div>
      )}

      <div style={{ display: 'flex', justifyContent: 'space-between', gap: '8px', flexWrap: 'wrap' }}>
        <Button variant="ghost" onClick={rescan} disabled={busy}>Scan again</Button>
        <Button variant="ghost" onClick={onClose}>Done</Button>
      </div>
    </Modal>
  )
}

/* One thing the listing turned up, and the button for the panel that deals
   with it.

   It leads on the same fact its banner over the file list leads on, and for the
   same reason: room held for nothing, in the sweep's case, and files short of a
   spare in the repair's. This is the same news reached a different way, and a
   reader who has seen both should not have to work out that they are the same
   news. */
function Finding({ icon, title, body, action, onClick }) {
  return (
    <div style={{
      display: 'flex',
      alignItems: 'flex-start',
      gap: '12px',
      padding: '12px',
      borderRadius: '8px',
      background: COLORS.surfaceRaised,
      border: `1px solid ${COLORS.border}`,
    }}>
      <span aria-hidden="true" style={{ fontSize: '16px', lineHeight: 1.2, flexShrink: 0 }}>
        {icon}
      </span>
      <div style={{ minWidth: 0, flex: 1, display: 'flex', flexDirection: 'column', gap: '4px' }}>
        <span style={{
          fontFamily: FONT.mono, fontSize: '12px', fontWeight: 600, color: COLORS.text,
        }}>{title}</span>
        <span style={{
          fontFamily: FONT.sans, fontSize: '11px', lineHeight: 1.5, color: COLORS.textMuted,
        }}>{body}</span>
        <div style={{ marginTop: '6px' }}>
          <Button size="sm" variant="primary" onClick={onClick}>{action}</Button>
        </div>
      </div>
    </div>
  )
}

/* What each account is carrying, and how much of it is dead weight. The
   denominator matters as much as the figure: 40 MB abandoned means one thing on
   an account holding 60 MB and another on one holding 400 GB. */
function AccountBreakdown({ accounts }) {
  const carrying = accounts.filter((account) => account.orphans > 0 || account.error)
  if (carrying.length === 0) return null

  return (
    <div style={{ ...listStyle, marginBottom: '16px' }}>
      {carrying.map((account) => (
        <div key={account.provider_id} style={rowStyle}>
          <span style={{
            width: '3px',
            alignSelf: 'stretch',
            borderRadius: '2px',
            background: accountColor(account.provider_id),
            flexShrink: 0,
          }} />
          <div style={{ minWidth: 0, flex: 1 }}>
            <div style={{ fontSize: '12px', color: COLORS.text }}>{account.name}</div>
            <div style={{ fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textMuted }}>
              {account.error
                ? `could not be listed — ${account.error}`
                : `${account.orphans} of ${account.objects} part objects, `
                  + `${formatBytes(account.orphan_bytes)} of ${formatBytes(account.bytes)}`}
            </div>
          </div>
          {account.foreign && (
            <span style={tagStyle}>another vault&apos;s</span>
          )}
        </div>
      ))}
    </div>
  )
}

function ArchiveRow({ item, checked, onToggle, disabled }) {
  return (
    <label style={{ ...rowStyle, cursor: disabled ? 'default' : 'pointer', opacity: disabled ? 0.6 : 1 }}>
      <input
        type="checkbox"
        checked={checked}
        onChange={onToggle}
        disabled={disabled}
        style={{ accentColor: COLORS.accent, flexShrink: 0 }}
      />
      <span style={{
        width: '3px',
        alignSelf: 'stretch',
        borderRadius: '2px',
        background: accountColor(item.provider_id),
        flexShrink: 0,
      }} />
      <div style={{ minWidth: 0, flex: 1 }}>
        <div style={{
          fontFamily: FONT.mono,
          fontSize: '11px',
          color: COLORS.text,
          overflow: 'hidden',
          textOverflow: 'ellipsis',
          whiteSpace: 'nowrap',
        }}>{item.archive_id}</div>
        <div style={{ fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textMuted }}>
          {item.provider_name} · {item.objects} object{item.objects === 1 ? '' : 's'} ·{' '}
          {formatBytes(item.bytes)}
          {item.reason ? ` · ${item.reason}` : ''}
        </div>
      </div>
    </label>
  )
}

function SectionLabel({ children }) {
  return (
    <div style={{
      fontFamily: FONT.mono,
      fontSize: '9px',
      fontWeight: 700,
      letterSpacing: '1.2px',
      textTransform: 'uppercase',
      color: COLORS.textMuted,
      marginBottom: '6px',
    }}>{children}</div>
  )
}

const paragraph = {
  margin: '0 0 16px',
  fontFamily: FONT.sans,
  fontSize: '12px',
  lineHeight: 1.6,
  color: COLORS.textMuted,
}

const listStyle = {
  display: 'flex',
  flexDirection: 'column',
  gap: '4px',
  maxHeight: '240px',
  overflowY: 'auto',
  marginBottom: '16px',
}

const rowStyle = {
  display: 'flex',
  alignItems: 'center',
  gap: '10px',
  padding: '8px 10px',
  borderRadius: '6px',
  background: COLORS.surface,
  border: `1px solid ${COLORS.border}`,
}

const tagStyle = {
  fontFamily: FONT.mono,
  fontSize: '9px',
  letterSpacing: '0.6px',
  textTransform: 'uppercase',
  color: COLORS.textMuted,
  border: `1px solid ${COLORS.border}`,
  borderRadius: '4px',
  padding: '2px 6px',
  flexShrink: 0,
}

const preText = {
  display: 'block',
  marginTop: '6px',
  fontFamily: FONT.mono,
  fontSize: '11px',
  whiteSpace: 'pre-wrap',
}
