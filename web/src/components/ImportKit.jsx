import React, { useEffect, useState } from 'react'
import { COLORS, FONT, KIND_ICONS, accountColor, formatBytes, formatDate } from '../theme'
import { api } from '../api'
import { Banner, Button, Input, Modal, PasswordInput, Spinner } from './ui'
import { FilePicker } from './RecoveryKit'
import EditAccount from './EditAccount'

/* The third door on the lock screen: I have a recovery kit.

   Shown only when no vault exists, which is the state a reinstalled machine is
   in and the only state this can run in anyway. `inspect` runs the moment a
   file is chosen, so the field that follows is labelled Recovery code or Vault
   password according to what the kit actually wants rather than a guess. */

export default function ImportKit({ onClose, onImported }) {
  const [file, setFile] = useState(null)
  const [envelope, setEnvelope] = useState(null)
  const [secret, setSecret] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const [codeHint, setCodeHint] = useState(null)
  const [report, setReport] = useState(null)
  /* Held rather than handed straight up. Signing in the moment the import
     returns unmounts the lock screen and takes the report with it — and the
     report is where the accounts that need a sign-in or a folder become
     buttons, which is the whole reason a partial import is not an error. */
  const [signIn, setSignIn] = useState(null)

  const pick = async (chosen) => {
    setFile(chosen)
    setEnvelope(null)
    setError(null)
    try {
      setEnvelope(await api.inspectKit(chosen))
    } catch (err) {
      setError(err.message)
      setFile(null)
    }
  }

  const wantsCode = envelope?.secret !== 'password'

  /* The check symbol's whole point: a typo is settled here, in microseconds,
     rather than after a second and a half of Argon2id followed by the message
     that makes a person conclude their backup is dead.

     It cannot say *where* the typo is — five bits over the whole code catch
     essentially every slip and localise none of them — so it says the one true
     thing instead. */
  const onSecret = async (value) => {
    setSecret(value)
    setCodeHint(null)
    if (!wantsCode) return
    const bare = value.replace(/[^0-9a-zA-Z]/g, '')
    if (bare.length >= 25) setCodeHint(await checkCode(bare))
  }

  const run = async () => {
    if (password !== confirm) {
      setError('The two passwords do not match.')
      return
    }
    setBusy(true)
    setError(null)
    try {
      const resp = await api.importKit({ file, secret, password })
      setReport(resp.report)
      setSignIn(resp.status)
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  if (report) {
    return <ImportReport report={report} onClose={() => onImported?.(signIn)} />
  }

  return (
    <Modal
      title="Import a recovery kit"
      subtitle="Nothing is changed until you have typed the code and confirmed"
      onClose={() => !busy && onClose()}
      width={520}
    >
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

      <FilePicker file={file} onPick={pick} />

      {envelope && (
        <>
          <div style={{
            padding: '13px 14px', marginBottom: '18px',
            background: COLORS.surfaceRaised, border: `1px solid ${COLORS.border}`, borderRadius: '8px',
          }}>
            <div style={{
              fontFamily: FONT.mono, fontSize: '10px', fontWeight: 600, letterSpacing: '1.5px',
              textTransform: 'uppercase', color: COLORS.textMuted, marginBottom: '10px',
            }}>What this kit says it is</div>
            <Fact label="Made" value={formatDate(envelope.created_at)} />
            <Fact label="Kit" value={(envelope.kit_id || '').slice(0, 8)} />
            <Fact label="Opened by" value={wantsCode ? 'a recovery code' : 'a vault password'} />
          </div>

          {/* Shown rather than masked, which is the opposite of what a password
              field should do and the right thing for this. A code is copied off
              a slip of paper and checked back against it; hiding it behind dots
              turns the one moment it is ever typed into a guessing game, and
              there is nothing to shoulder-surf that the archive beside it does
              not already give away. */}
          {wantsCode ? (
            <Input
              label="Recovery code"
              value={secret}
              autoFocus
              spellCheck={false}
              autoCapitalize="characters"
              autoComplete="off"
              placeholder="XXXXX-XXXXX-XXXXX-XXXXX-XXXXX"
              help={codeHint?.message}
              onChange={(e) => onSecret(e.target.value)}
              style={{
                letterSpacing: '1.5px',
                fontSize: '14px',
                borderColor: codeHint ? (codeHint.ok ? COLORS.success : COLORS.error) : undefined,
              }}
            />
          ) : (
            <PasswordInput
              label="Vault password for this kit"
              value={secret}
              autoFocus
              placeholder="The password in use when it was made"
              onChange={(e) => onSecret(e.target.value)}
            />
          )}

          <PasswordInput
            label="A password for the recovered vault"
            value={password}
            autoComplete="new-password"
            placeholder="Choose a strong passphrase"
            help="What you will type to unlock this machine from now on. It does not have to be the one you used before."
            onChange={(e) => setPassword(e.target.value)}
          />
          <PasswordInput
            label="Confirm password"
            value={confirm}
            autoComplete="new-password"
            placeholder="Type it again"
            onChange={(e) => setConfirm(e.target.value)}
          />

          <Button
            variant="primary"
            disabled={busy || !secret || !password || (codeHint && !codeHint.ok)}
            onClick={run}
            style={{ width: '100%', justifyContent: 'center' }}
          >
            {busy ? <Spinner size={13} color={COLORS.bg} /> : null}
            {busy ? 'RECOVERING…' : 'RECOVER THIS VAULT'}
          </Button>

          {busy && (
            <p style={{
              margin: '14px 0 0', fontFamily: FONT.sans, fontSize: '11px',
              lineHeight: 1.55, color: COLORS.textMuted, textAlign: 'center',
            }}>
              Reconnecting your clouds and asking each what it holds. An account that will not
              connect does not stop this — you will be told which one.
            </p>
          )}
        </>
      )}
    </Modal>
  )
}

function Fact({ label, value }) {
  return (
    <div style={{
      display: 'flex', justifyContent: 'space-between', gap: '10px',
      fontSize: '11.5px', fontFamily: FONT.sans, marginBottom: '6px',
    }}>
      <span style={{ color: COLORS.textMuted }}>{label}</span>
      <span style={{ fontFamily: FONT.mono, color: COLORS.text }}>{value}</span>
    </div>
  )
}

/* Crockford base32, and the same five-bit check symbol the server computes.

   Duplicated here on purpose, and it is the whole reason the symbol exists: a
   typo has to be answered in the field, in microseconds, rather than after a
   second and a half of Argon2id followed by "wrong code" — which is the message
   that makes a person conclude their backup is dead.

   It cannot say *where* the typo is. Five bits over the whole code catch
   essentially every single-character slip and every adjacent transposition, and
   localise none of them, so it says the one true thing instead. A character
   that is not in the alphabet at all *is* localisable, and is named. */
const ALPHABET = '0123456789ABCDEFGHJKMNPQRSTVWXYZ'

async function checkCode(raw) {
  const folded = raw.toUpperCase().replace(/[IL]/g, '1').replace(/O/g, '0')
  if (folded.length !== 25) {
    return { ok: false, message: `A recovery code is 25 characters — this one has ${folded.length}.` }
  }
  for (const c of folded) {
    if (!ALPHABET.includes(c)) {
      return { ok: false, message: `"${c}" is not a character a recovery code uses.` }
    }
  }

  const bytes = decodeCrockford(folded.slice(0, 24))
  const expected = await checkSymbol(bytes)
  if (expected === null) {
    /* No SubtleCrypto — the page is being served over plain HTTP to something
       other than localhost. The shape is all this can vouch for, so it says
       only that and lets the server be the authority. */
    return { ok: true, message: 'That is the right shape for a code.' }
  }
  if (expected !== folded[24]) {
    return { ok: false, message: 'That code has a typo in it — check it against what you wrote down.' }
  }
  return { ok: true, message: 'Checks out. This is a well-formed code.' }
}

/* 24 symbols of five bits each, which is exactly 15 bytes with nothing left
   over — the reason the code is 24 symbols and not 23 or 25. */
function decodeCrockford(symbols) {
  const out = new Uint8Array(15)
  let acc = 0
  let bits = 0
  let at = 0
  for (const c of symbols) {
    acc = (acc << 5) | ALPHABET.indexOf(c)
    bits += 5
    if (bits >= 8) {
      bits -= 8
      out[at++] = (acc >> bits) & 0xff
    }
  }
  return out
}

/* The top five bits of the SHA-256 of those 15 bytes, as a Crockford symbol.
   Returns null where the browser will not do SHA-256 for us. */
async function checkSymbol(bytes) {
  if (!globalThis.crypto?.subtle) return null
  try {
    const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', bytes))
    return ALPHABET[digest[0] >> 3]
  } catch {
    return null
  }
}

/* The report leads on the shortfall, the same way the recovery report does,
   because a list of what worked is not what the person reading it needs. */
function ImportReport({ report: initial, onClose }) {
  /* The counts move as accounts are repaired, so the report is state rather
     than a prop. An account brought back after the import holds parts the
     index gave up on, and saying "312 files cannot be opened" underneath a row
     that has just gone green would be the report contradicting itself. */
  const [report, setReport] = useState(initial)
  const complete = report.lost === 0

  /* The repair dialog edits a real account, not the summary row the report is
     drawn from — so the accounts are fetched once here. The import has already
     signed this session in, which is what makes them readable at all. Rows
     whose account has not arrived yet keep their button disabled rather than
     opening a dialog on nothing. */
  const [configs, setConfigs] = useState(null)
  const [all, setAll] = useState([])
  const [accountsError, setAccountsError] = useState(null)
  const loadAccounts = () => api.providers()
    .then((resp) => {
      const list = resp.providers || []
      setAll(list)
      setConfigs(Object.fromEntries(list.map((p) => [p.id, p])))
      setAccountsError(null)
    })
    .catch((err) => {
      // Without this the repair buttons stay disabled forever with nothing
      // said, which reads as "there is nothing to be done here".
      setConfigs({})
      setAccountsError(err.message)
    })
  useEffect(() => { loadAccounts() }, [])

  /* What a repaired account is actually worth. Reconcile is the operation
     built for exactly this — "finish a recovery that ran before every account
     was back" — so a repair ends by running it: the accounts are asked what
     they hold, records that now have somewhere to point are re-pointed, and
     the tally comes back honest.

     It is what tells a folder that holds the parts from a folder that merely
     exists. Pointing an account at the wrong directory succeeds — a local
     backend creates what it is given — so "connected" alone would be a green
     tick over an empty folder. */
  const reconcile = () => api.resumeRecovery({ dryRun: false })
    .then((resp) => {
      const r = resp.report || {}
      setReport((was) => ({
        ...was,
        files: r.files ?? was.files,
        recoverable: r.recoverable ?? was.recoverable,
        bytes: r.bytes ?? was.bytes,
        recoverable_bytes: r.recoverable_bytes ?? was.recoverable_bytes,
        lost: r.lost ?? was.lost,
        missing: r.missing ?? was.missing,
        blocking: r.missing_accounts ?? was.blocking,
      }))
      return r
    })
    .catch(() => null)

  return (
    <Modal
      title="Recovered"
      subtitle={complete
        ? 'Your files are browsable now.'
        : 'Your files are browsable now. One thing below would bring back the rest.'}
      onClose={onClose}
      width={560}
    >
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(2, minmax(0, 1fr))', gap: '10px', marginBottom: '16px' }}>
        <Stat value={report.recoverable.toLocaleString()} label={`of ${report.files.toLocaleString()} files openable`} />
        <Stat value={formatBytes(report.recoverable_bytes)} label={`of ${formatBytes(report.bytes)}, by weight`} />
      </div>

      {report.index_source !== 'kit' && (
        <Banner tone="success">
          The index came off <strong>{report.index_source_name}</strong>, dated{' '}
          <strong>{formatDate(report.index_at)}</strong> — newer than your kit, so anything
          added since it was made is in the tree too.
        </Banner>
      )}
      {report.password_changed && (
        <Banner tone="warn">
          The copies of the index on your accounts do not open under this kit&apos;s key, so the
          kit&apos;s own index was used. Usually that means your vault password changed after the
          kit was made — anything added between the two is not in this tree.
        </Banner>
      )}

      <div style={{
        fontFamily: FONT.mono, fontSize: '10px', fontWeight: 600, letterSpacing: '1.5px',
        textTransform: 'uppercase', color: COLORS.textMuted, margin: '4px 0 9px',
      }}>Your accounts</div>

      {accountsError && (
        <Banner tone="warn">
          The accounts could not be read back, so the repairs below cannot be opened yet:
          {' '}{accountsError}
        </Banner>
      )}

      <div style={{ display: 'flex', flexDirection: 'column', gap: '6px', marginBottom: '18px' }}>
        {report.accounts.map((a) => (
          <AccountRow
            key={a.id}
            account={a}
            config={configs?.[a.id]}
            providers={all}
            loading={configs === null}
            onRepaired={loadAccounts}
            onReconcile={reconcile}
          />
        ))}
        {report.blocking?.filter((b) => !report.accounts.some((a) => a.id === b.id)).map((b) => (
          <UnknownAccountRow key={b.id} account={b} />
        ))}
      </div>

      {!complete && (
        <Banner tone="warn">
          <strong>{report.lost.toLocaleString()} file(s)</strong> cannot be opened yet.
          {report.blocking?.length > 0 && (
            <> Connect {report.blocking.filter((b) => b.blocking).map((b) => b.name).join(', ')} and
            they come back — nothing else is missing.</>
          )}
        </Banner>
      )}

      <div style={{ display: 'flex', gap: '10px', marginBottom: '18px' }}>
        {report.sub_vaults > 0 && (
          <Tile
            value={`${report.sub_vaults} sub vault(s)`}
            hint="back, and still shut — each opens with its own password"
          />
        )}
        <Tile
          value={`${report.repointed} part(s) re-pointed`}
          hint={report.repointed === 0
            ? 'the kit kept every account’s identity, so nothing needed matching'
            : 'these had moved since the kit was made'}
        />
      </div>

      {report.orphans > 0 && (
        <Banner tone="info">
          {report.orphans.toLocaleString()} object(s) on your accounts are not named by this
          index. Nothing was deleted — the index is simply older than the storage.
        </Banner>
      )}

      {report.warnings?.length > 0 && (
        <div style={{ marginBottom: '16px' }}>
          {report.warnings.map((w, i) => <Banner key={i} tone="warn">{w}</Banner>)}
        </div>
      )}

      <Button variant="primary" onClick={onClose} style={{ width: '100%', justifyContent: 'center' }}>
        OPEN THE VAULT
      </Button>
    </Modal>
  )
}

/* One account, and — where it needs one — the single button that fixes it.

   A failure here is never fatal to the import: a year-old sign-in is the
   expected case, not the edge case. Both repairs keep the account's id, which
   is what keeps the index correct across them.

   The repair itself is the accounts panel's own Edit dialog, opened on its
   "How it connects" half. That half already knows how to put a broken account
   right — a trip through the provider's consent screen reusing the OAuth app
   the kit carried, or the settings form for a backend with no consent screen —
   and whatever it is given is built into a live backend and pinged before it
   is stored, so credentials the provider rejects are refused rather than saved.

   How much that ping proves depends on the backend. A rejected key is a real
   refusal; a *folder*, though, is created if it is not there, so pointing an
   account at the wrong directory succeeds and comes back empty. That is why a
   saved dialog is not treated as the end of it: the row re-tests the account
   and then re-counts the whole vault, and the figures it puts back on the
   report are the only honest answer to whether the repair helped. */
function AccountRow({ account, config, providers, loading, onRepaired, onReconcile }) {
  const [editing, setEditing] = useState(false)
  const [checking, setChecking] = useState(false)
  const [fixed, setFixed] = useState(false)
  const [note, setNote] = useState(null)
  const [error, setError] = useState(null)

  const colour = accountColor(account.id)
  const glyph = KIND_ICONS[account.kind] || '☁'
  const settled = fixed || account.status === 'connected'

  /* The dialog says the settings were accepted, which is not the same claim as
     "this account is working now" — so the row asks the account itself, and
     believes the answer rather than the 200 that carried it: a failed test is
     a normal reply here, not an HTTP error. */
  const recheck = async () => {
    setChecking(true)
    setError(null)
    setNote(null)
    try {
      const result = await api.testProvider(account.id)
      if (!result?.online) {
        setError(result?.error || 'that account still does not answer')
        return
      }
      setFixed(true)

      /* An account that comes back after the import is still holding the index
         of the vault that died — the push that claimed the others could not
         reach it. Left alone it would refuse this vault's index forever, and
         the next recovery would find a stale one sitting there. */
      const claim = await api.claimBackups().catch((err) => ({ error: err.message }))
      if (claim?.error) {
        setNote(`It is connected, but this vault's index could not be copied to it: ${claim.error}`)
      } else if (claim && claim.claimed === false) {
        setNote('It is connected. Copies of the index are switched off for this vault, so it is '
          + 'still holding the one the old vault left there.')
      } else if (claim?.warnings?.length) {
        setNote(claim.warnings[0])
      }

      /* Records that now have somewhere to point are pointed there, and the
         report's tally comes back honest. This is what a repair is *for*, and
         Reconcile is the operation built for it — "finish a recovery that ran
         before every account was back". */
      await onReconcile?.()

      /* Deliberately no "but is it the right folder?" warning here. A folder
         backend creates what it is pointed at, so a wrong one answers exactly
         as a right one does, and the figures that would tell them apart —
         what is actually on the account — are not ones this can get cheaply
         and correctly for every backend. A warning that fires on a guess is
         worse than none on the screen a person is reading in a bad hour.

         What is honest is above: Reconcile has just re-counted, so the file
         tally is the current truth. A repair that found nothing leaves it
         where it was, which is the report declining to claim an improvement
         rather than inventing a reason. */
    } catch (err) {
      setError(err.message)
    } finally {
      setChecking(false)
      // Whatever happened, the stored settings have moved on — a second
      // attempt must open the dialog on what is there now rather than on the
      // options that were broken.
      onRepaired?.()
    }
  }

  return (
    <div style={{
      display: 'flex', flexDirection: 'column', gap: '8px',
      padding: '10px 11px',
      background: COLORS.surfaceRaised,
      border: `1px solid ${settled ? COLORS.border : COLORS.borderBright}`,
      borderLeft: `2px solid ${colour}`,
      borderRadius: '6px',
    }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: '10px' }}>
        <span aria-hidden="true" style={{
          fontFamily: FONT.mono, fontSize: '13px', color: colour,
          width: '14px', textAlign: 'center', flexShrink: 0,
        }}>{glyph}</span>

        <span style={{ display: 'flex', flexDirection: 'column', gap: '2px', flex: 1, minWidth: 0 }}>
          <span style={{ fontFamily: FONT.mono, fontSize: '11.5px', color: COLORS.text }}>
            {account.name}
          </span>
          {!settled && account.detail && (
            <span style={{
              fontFamily: FONT.sans, fontSize: '10.5px', color: COLORS.textMuted,
              overflowWrap: 'anywhere',
            }}>{account.detail}</span>
          )}
        </span>

        {settled && (
          <span style={{ fontFamily: FONT.sans, fontSize: '11px', color: COLORS.success }}>✓ connected</span>
        )}
        {!settled && checking && <Spinner size={13} />}
        {!settled && !checking && (account.repair === 'retry' || !account.repair) && (
          /* No repair named means the account could not even be restored into
             the vault, so there is nothing to open a dialog on — but there is
             still something to try, and a row with neither a button nor a word
             on it reads as one nobody has to do anything about. */
          <Button size="sm" onClick={recheck}>TRY AGAIN</Button>
        )}
        {!settled && !checking && account.repair && account.repair !== 'retry' && (
          <Button
            size="sm"
            variant={account.repair === 'sign_in' ? 'primary' : 'default'}
            disabled={loading || !config}
            onClick={() => setEditing(true)}
          >
            {loading ? '…' : REPAIR_LABEL[account.repair]}
          </Button>
        )}
      </div>

      {error && (
        <span style={{ fontFamily: FONT.sans, fontSize: '11px', color: COLORS.error }}>{error}</span>
      )}
      {note && (
        <span style={{ fontFamily: FONT.sans, fontSize: '11px', color: COLORS.warn, lineHeight: 1.45 }}>
          {note}
        </span>
      )}

      {editing && config && (
        <EditAccount
          provider={config}
          providers={providers}
          initialTab="connection"
          onClose={() => setEditing(false)}
          onChanged={() => { setEditing(false); recheck() }}
        />
      )}
    </div>
  )
}

/* What the one button says. The status is what is wrong; this is what the
   person is about to do about it, and they are different sentences. */
const REPAIR_LABEL = {
  sign_in: 'SIGN IN AGAIN',
  settings: 'FIX SETTINGS',
  path: 'FIND IT',
}

/* An account the newer index names that the kit never knew about — connected
   after the kit was made, so it holds no credentials for it. This is the honest
   ceiling of an old kit, and the one thing somebody has to do by hand. */
function UnknownAccountRow({ account }) {
  return (
    <div style={{
      display: 'flex', alignItems: 'center', gap: '10px', padding: '10px 11px',
      background: `${COLORS.warn}0f`,
      border: `1px solid ${COLORS.warn}55`,
      borderLeft: `2px solid ${COLORS.warn}`,
      borderRadius: '6px',
    }}>
      <span aria-hidden="true" style={{
        fontFamily: FONT.mono, fontSize: '13px', color: COLORS.warn,
        width: '14px', textAlign: 'center', flexShrink: 0,
      }}>{KIND_ICONS[account.kind] || '☁'}</span>
      <span style={{ display: 'flex', flexDirection: 'column', gap: '2px', flex: 1, minWidth: 0 }}>
        <span style={{ fontFamily: FONT.mono, fontSize: '11.5px', color: COLORS.text }}>{account.name}</span>
        <span style={{ fontFamily: FONT.sans, fontSize: '10.5px', color: COLORS.warn, lineHeight: 1.4 }}>
          connected after your kit was made, so it holds no credentials for it — it has parts
          of {account.files.toLocaleString()} file(s)
        </span>
      </span>
    </div>
  )
}

function Stat({ value, label }) {
  return (
    <div style={{
      padding: '14px', background: COLORS.surfaceRaised,
      border: `1px solid ${COLORS.border}`, borderRadius: '8px',
    }}>
      <div style={{ fontFamily: FONT.mono, fontSize: '22px', fontWeight: 700, color: COLORS.text, lineHeight: 1.1 }}>
        {value}
      </div>
      <div style={{ marginTop: '5px', fontFamily: FONT.sans, fontSize: '11px', color: COLORS.textMuted, lineHeight: 1.4 }}>
        {label}
      </div>
    </div>
  )
}

function Tile({ value, hint }) {
  return (
    <div style={{
      flex: 1, padding: '11px 12px', background: COLORS.surfaceRaised,
      border: `1px solid ${COLORS.border}`, borderRadius: '6px',
    }}>
      <div style={{ fontFamily: FONT.mono, fontSize: '12px', color: COLORS.text }}>{value}</div>
      <div style={{ marginTop: '3px', fontFamily: FONT.sans, fontSize: '10.5px', color: COLORS.textMuted, lineHeight: 1.4 }}>
        {hint}
      </div>
    </div>
  )
}

/* The door itself, shown on the lock screen only when no vault exists. */
export function ImportKitDoor({ onClick }) {
  const [hover, setHover] = useState(false)

  return (
    <>
      <div style={{ display: 'flex', alignItems: 'center', gap: '12px', margin: '18px 0' }}>
        <span style={{ flex: 1, height: '1px', background: COLORS.border }} />
        <span style={{
          fontFamily: FONT.mono, fontSize: '10px', letterSpacing: '1.5px',
          textTransform: 'uppercase', color: COLORS.textMuted,
        }}>or</span>
        <span style={{ flex: 1, height: '1px', background: COLORS.border }} />
      </div>

      <button
        type="button"
        onClick={onClick}
        onPointerEnter={(e) => { if (e.pointerType === 'mouse') setHover(true) }}
        onPointerLeave={() => setHover(false)}
        style={{
          display: 'flex', alignItems: 'center', gap: '13px', width: '100%',
          padding: '14px 16px',
          background: hover ? COLORS.surfaceHover : COLORS.surface,
          border: `1px solid ${hover ? COLORS.accent : COLORS.borderBright}`,
          borderRadius: '10px', cursor: 'pointer', textAlign: 'left',
          transition: 'background 0.15s ease, border-color 0.15s ease',
        }}
      >
        <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke={COLORS.accent}
          strokeWidth="1.9" strokeLinecap="round" strokeLinejoin="round"
          style={{ flexShrink: 0 }} aria-hidden="true">
          <path d="M4 8h16a1 1 0 0 1 1 1v10a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V9a1 1 0 0 1 1-1z" />
          <path d="M8 8V6a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" />
          <path d="M10 13h4" />
        </svg>
        <span style={{ display: 'flex', flexDirection: 'column', gap: '4px', flex: 1, minWidth: 0 }}>
          <span style={{
            fontFamily: FONT.mono, fontSize: '12px', fontWeight: 600,
            letterSpacing: '0.5px', color: COLORS.text,
          }}>I have a recovery kit</span>
          <span style={{ fontFamily: FONT.sans, fontSize: '11px', lineHeight: 1.5, color: COLORS.textMuted }}>
            Reconnects every cloud and brings back the tree you had
          </span>
        </span>
        <span aria-hidden="true" style={{ fontFamily: FONT.mono, fontSize: '13px', color: COLORS.textMuted }}>›</span>
      </button>

      <p style={{
        margin: '14px 0 0', textAlign: 'center',
        fontFamily: FONT.sans, fontSize: '11px', lineHeight: 1.55, color: COLORS.textMuted,
      }}>
        No kit, but your clouds are still there? Create a vault, connect one, and SAND will
        notice what it is holding.
      </p>
    </>
  )
}
