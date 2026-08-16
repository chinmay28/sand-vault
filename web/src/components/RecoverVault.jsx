import React, { useCallback, useState } from 'react'
import { COLORS, FONT, KIND_ICONS, formatBytes } from '../theme'
import { api } from '../api'
import { Banner, Button, Modal, PasswordInput, Spinner } from './ui'
import { PARTS_PER_FILE } from './CloudSelect'
import ConnectCloud from './ConnectCloud'

/* The dialog for the day the machine died.

   Every connected account carries an encrypted copy of the index, so a fresh
   install that reconnects those accounts is sitting on everything it needs to
   rebuild the vault — it just does not know it yet. The app notices, and this is
   what it opens.

   It opens on the *first* cloud you reconnect, which is never enough on its
   own: a file is rebuilt from two of its three parts, and one account holds one
   of them. So this is not a form, it is an errand — connect a cloud, see what
   that bought you, connect the next one — and it runs the errand rather than
   describing it. Connecting is a button in here, and every account that lands
   re-checks by itself and goes on to recover the moment there is enough to
   recover with.

   The check before the commit is not a nicety either. A recovery is only as
   complete as the accounts that came back, and finding out which files did not
   *after* adopting the index is far worse than being told first.

   Two shapes, because there are two situations. An empty vault adopts the index
   outright. A vault that already did that, with a cloud still missing, has
   nothing left to adopt and everything left to re-point — so the same dialog
   resumes instead, and asks for no password, the key having been adopted the
   first time round. */
export default function RecoverVault({ scan: initialScan, onClose, onRecovered, onAccountsChanged }) {
  const [scan, setScan] = useState(initialScan)
  const [password, setPassword] = useState('')
  const [source, setSource] = useState(() => preferredSource(initialScan))
  const [busy, setBusy] = useState(null) // 'preview' | 'recover' | 'scanning'
  const [error, setError] = useState(null)
  const [preview, setPreview] = useState(null)
  const [result, setResult] = useState(null)
  const [connecting, setConnecting] = useState(false)

  const resuming = !scan?.available && !!scan?.resumable
  const holders = (scan?.sources || []).filter((s) => s.backup && s.foreign)
  // Accounts that turned out to be carrying something. Two is what it takes to
  // rebuild a file, so below that there is nothing worth attempting yet.
  const carrying = (scan?.sources || []).filter((s) => s.parts > 0).length

  /* One attempt, as a check or for real.
     `mode` is passed explicitly rather than read off `resuming` so that the
     step which connects a cloud can act on the scan it has just fetched. That
     scan is exactly what decides the mode — connecting the last cloud is what
     turns "adopt this index" into "re-point the one we already have" — and the
     value closed over here is a render behind it. */
  const run = useCallback(async (dryRun, mode) => {
    setError(null)
    setBusy(dryRun ? 'preview' : 'recover')
    try {
      const resp = mode === 'resume'
        ? await api.resumeRecovery({ dryRun })
        : await api.recover({ providerId: source, password, dryRun })
      if (dryRun) setPreview(resp)
      else setResult(resp)
      return resp
    } catch (err) {
      setError(err.code === 'WRONG_PASSWORD'
        ? 'That password does not open the backup on this account. It is the password of the vault you lost, which may not be the one this vault uses.'
        : err.message)
      if (!dryRun) setPreview(null)
      return null
    } finally {
      setBusy(null)
    }
  }, [source, password])

  // What the dialog is doing right now, for the buttons that are already on the
  // step that decided it.
  const mode = resuming ? 'resume' : 'recover'

  /* A cloud has just been connected from inside this dialog, which is the whole
     reason the dialog asked. Look again, and carry straight on:

     - Still short of what it takes to rebuild anything, so there is nothing to
       try yet — the dialog asks for the next cloud instead.
     - Enough now, and the vault is already carrying the index: re-point it,
       for real. Nothing is at risk in doing that — it is the same reachability
       question the check asks, answered by writing it down.
     - Enough now, and a password has been given: check first, then recover if
       the check comes back whole. Being handed the cloud that was asked for is
       the assent; making someone press the same button again is ceremony. */
  const afterConnect = useCallback(async () => {
    setConnecting(false)
    onAccountsChanged?.()

    setBusy('scanning')
    let fresh = scan
    try {
      fresh = await api.recoveryScan()
      setScan(fresh)
      if (!source) setSource(preferredSource(fresh))
    } catch (err) {
      setError(err.message)
      return
    } finally {
      setBusy(null)
    }

    const holding = (fresh.sources || []).filter((s) => s.parts > 0).length
    if (holding < MIN_PARTS_TO_RESTORE) return

    if (!fresh.available && fresh.resumable) {
      await run(false, 'resume')
      return
    }
    if (!password) return

    const checked = await run(true, 'recover')
    if (checked && checked.report.lost === 0) await run(false, 'recover')
  }, [scan, source, password, run, onAccountsChanged])

  /* One more cloud, asked for from wherever the shortfall was reported.

     Primary only where it is the way forward — with too few clouds to rebuild
     anything, or a report saying what is still missing. Once a recovery can go
     ahead it steps back to an ordinary button, so there are not two of them
     competing to look like the thing to press. */
  const connectButton = (label = 'Connect another cloud', variant = 'primary') => (
    <Button variant={variant} onClick={() => setConnecting(true)} disabled={!!busy}>
      {busy === 'scanning' ? <Spinner size={12} color={variant === 'primary' ? COLORS.bg : COLORS.accent} /> : null}
      {busy === 'scanning' ? 'Looking…' : `+ ${label}`}
    </Button>
  )

  const cloudDialog = connecting && (
    <ConnectCloud onClose={() => setConnecting(false)} onConnected={afterConnect} />
  )

  /* ---- After the rebuild: what came back, and what did not ---- */
  if (result) {
    const report = result.report
    return (
      <Modal title={report.lost > 0 ? 'Recovery finished, in part' : 'Recovery complete'}
        onClose={busy ? undefined : onClose} width={620}>
        {report.recoverable > 0 ? (
          <Banner tone={report.lost > 0 ? 'warn' : 'success'}>
            {report.recoverable} of {report.files} file{report.files === 1 ? '' : 's'} are back in
            your vault, and openable — {formatBytes(report.recoverable_bytes)} of {formatBytes(report.bytes)}.
          </Banner>
        ) : (
          <Banner tone="error">
            The index came back, but none of its {report.files} file{report.files === 1 ? '' : 's'} has
            enough reachable parts to open.
          </Banner>
        )}

        <RecoveryFigures report={report} />
        <Shortfall report={report} />

        {/* Adopting the lost vault's key is what made these files readable
            again, and it is not where this should end: that key comes from the
            old password, and every copy of the old index backup hands it over.
            Said here, and again in the accounts panel, because the transfer it
            takes to fix is the whole vault twice over and rarely wanted now. */}
        {report.recoverable > 0 && (
          <Banner tone="info">
            These files are on the key of the vault they came from, which its password still
            opens. When you have the time and the bandwidth, re-encrypt them under your own key
            from the accounts panel — it rebuilds every file and erases the parts the old
            password could read.
          </Banner>
        )}

        {report.warnings?.length > 0 && (
          <Banner tone="warn">
            <span style={{ fontFamily: FONT.mono, fontSize: '11px', whiteSpace: 'pre-wrap' }}>
              {report.warnings.join('\n')}
            </span>
          </Banner>
        )}

        <p style={paragraph}>
          {report.lost > 0
            ? 'Nothing has been thrown away: the index knows these files exist and where their parts went. Connect one of the clouds above and this picks up where it left off — no password needed the second time.'
            : 'The accounts now carry a copy of the index under this vault’s password, so this machine can be lost too.'}
        </p>

        <div style={{ display: 'flex', justifyContent: 'flex-end', gap: '8px', flexWrap: 'wrap' }}>
          {report.lost > 0 ? (
            <>
              <Button variant="ghost" onClick={onRecovered} disabled={!!busy}>Leave it for now</Button>
              {connectButton('Connect a missing cloud')}
            </>
          ) : (
            <Button variant="primary" onClick={onRecovered} disabled={!!busy}>Open the vault</Button>
          )}
        </div>

        {cloudDialog}
      </Modal>
    )
  }

  /* ---- Resuming: the index is here, some of it is out of reach ---- */
  if (resuming) {
    return (
      <Modal
        title="Finish the recovery"
        subtitle="An earlier recovery ran before all your clouds were back. These parts are still out of reach."
        onClose={busy ? undefined : onClose}
        width={620}
      >
        {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

        <p style={paragraph}>
          {scan.stranded > 0
            ? `${scan.stranded} file${scan.stranded === 1 ? '' : 's'} in this vault cannot be opened: ${scan.unresolved} of their parts sit on accounts this vault is not connected to. `
            : `${scan.unresolved} part${scan.unresolved === 1 ? '' : 's'} of your files sit on accounts this vault is not connected to. `}
          Connect them here and this finishes by itself — it asks every account what it holds and
          re-points the index at whichever one answers. No password: the key is already here.
        </p>

        {preview && <PreviewReport report={preview.report} />}

        <div style={{ display: 'flex', justifyContent: 'flex-end', gap: '8px', flexWrap: 'wrap' }}>
          <Button variant="ghost" onClick={onClose} disabled={!!busy}>Not now</Button>
          <Button onClick={() => run(true, mode)} disabled={!!busy}>
            {busy === 'preview' ? <Spinner size={12} /> : null}
            {busy === 'preview' ? 'Checking…' : 'Check what is reachable'}
          </Button>
          <Button variant="primary" onClick={() => run(false, mode)} disabled={!!busy}>
            {busy === 'recover' ? <Spinner size={12} color={COLORS.bg} /> : null}
            {busy === 'recover' ? 'Finishing…' : 'Finish recovery'}
          </Button>
        </div>

        <div style={{ display: 'flex', justifyContent: 'flex-end', marginTop: '10px' }}>
          {connectButton('Connect another cloud', 'default')}
        </div>

        {cloudDialog}
      </Modal>
    )
  }

  /* ---- Not enough clouds yet to rebuild anything from ---- */
  const short = carrying < MIN_PARTS_TO_RESTORE

  return (
    <Modal
      title="Sand files detected"
      subtitle={short
        ? 'This cloud is carrying a vault. One cloud is not enough to rebuild a file from — connect the rest.'
        : 'These accounts are still carrying a vault. This one is empty, so it can take it over.'}
      onClose={busy ? undefined : onClose}
      width={620}
    >
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

      <p style={paragraph}>
        {scan.parts > 0
          ? `${scan.parts} stored part${scan.parts === 1 ? '' : 's'} (${formatBytes(scan.bytes)}) are sitting on your connected clouds, along with an encrypted copy of the index that says what they are.`
          : 'An encrypted copy of a vault index is sitting on your connected clouds.'}
        {' '}Your password opens it — nothing else is needed.
      </p>

      <div style={{ marginBottom: '16px' }}>
        {(scan.sources || []).map((s) => (
          <SourceRow
            key={s.provider_id}
            source={s}
            selectable={holders.length > 1 && s.backup && s.foreign}
            selected={s.provider_id === source}
            onSelect={() => setSource(s.provider_id)}
          />
        ))}
      </div>

      {holders.length > 1 && (
        <p style={{ ...paragraph, marginTop: '-6px' }}>
          Every copy is identical, so any of them will do.
        </p>
      )}

      {/* Below two clouds there is nothing to attempt, so the dialog does not
          pretend otherwise: it asks for the next one and stops there. */}
      {short ? (
        <Banner tone="warn">
          A file was split into {PARTS_PER_FILE} parts across {PARTS_PER_FILE} clouds and is
          rebuilt from any {MIN_PARTS_TO_RESTORE} of them, so
          {carrying === 1 ? ' one cloud on its own carries no whole file' : ' nothing here carries a whole file'}.
          Connect the next cloud that held parts of this vault — as many as you can — and the
          recovery starts on its own.
        </Banner>
      ) : (scan.sources || []).length < PARTS_PER_FILE && (
        <Banner tone="info">
          {scan.sources.length} clouds are connected, which is enough to rebuild every file — but
          not to bring back the third part each one was stored with. Connect the last{' '}
          {PARTS_PER_FILE - scan.sources.length === 1
            ? 'cloud'
            : `${PARTS_PER_FILE - scan.sources.length} clouds`} first and nothing comes back
          without its spare; recover now and they are picked up when it turns up.
        </Banner>
      )}

      {!short && (
        <PasswordInput
          label="Password of the vault you lost"
          value={password}
          autoFocus
          autoComplete="off"
          placeholder="The password that wrote this backup"
          disabled={!!busy}
          help="Not this vault's password, unless they happen to be the same one."
          onChange={(e) => { setPassword(e.target.value); setPreview(null) }}
        />
      )}

      {preview && <PreviewReport report={preview.report} at={preview.backup_at} />}

      <div style={{ display: 'flex', justifyContent: 'flex-end', gap: '8px', flexWrap: 'wrap' }}>
        <Button variant="ghost" onClick={onClose} disabled={!!busy}>Not now</Button>
        {short ? connectButton() : (
          <>
            <Button onClick={() => run(true, mode)} disabled={!!busy || !password}>
              {busy === 'preview' ? <Spinner size={12} /> : null}
              {busy === 'preview' ? 'Checking…' : 'Check what is there'}
            </Button>
            <Button variant="primary" onClick={() => run(false, mode)} disabled={!!busy || !password}>
              {busy === 'recover' ? <Spinner size={12} color={COLORS.bg} /> : null}
              {busy === 'recover' ? 'Recovering…' : 'Recover'}
            </Button>
          </>
        )}
      </div>

      {/* Still reachable once there is enough to recover with: more clouds is
          always the better answer, right up until every part is back. */}
      {!short && (scan.sources || []).length < PARTS_PER_FILE && (
        <div style={{ display: 'flex', justifyContent: 'flex-end', marginTop: '10px' }}>
          {connectButton('Connect another cloud', 'default')}
        </div>
      )}

      <p style={{ ...paragraph, margin: '14px 0 0' }}>
        Recovering adopts the lost vault&apos;s encryption key, which is what makes the parts on
        those accounts readable again. It only runs against an empty vault, so nothing here can
        be overwritten.
      </p>

      {cloudDialog}
    </Modal>
  )
}

/* Two of a file's three parts rebuild it, so two accounts is the floor below
   which a recovery has nothing to work with. Mirrors archive.MinPartsToRestore. */
const MIN_PARTS_TO_RESTORE = 2

/* One connected account, and what it turned out to be holding. Doubles as the
   picker when more than one account carries a backup — but only then, because a
   row you cannot choose should not look like a choice. */
function SourceRow({ source, selectable, selected, onSelect }) {
  const holds = source.backup && source.foreign

  let note = 'nothing SAND recognises'
  if (source.error) note = source.error
  else if (holds) note = source.parts > 0
    ? `index backup · ${source.parts} part${source.parts === 1 ? '' : 's'} · ${formatBytes(source.bytes)}`
    : 'index backup, but none of the parts it describes'
  else if (source.backup) note = "this vault's own backup"
  else if (source.parts > 0) note = `${source.parts} part${source.parts === 1 ? '' : 's'} · ${formatBytes(source.bytes)} · no index backup`

  const row = (
    <>
      <span style={{ fontSize: '13px', flexShrink: 0 }}>{KIND_ICONS[source.kind] || '☁'}</span>
      <span style={{ minWidth: 0, flex: 1 }}>
        <span style={{
          display: 'block',
          fontFamily: FONT.mono,
          fontSize: '12px',
          color: COLORS.text,
          overflow: 'hidden',
          textOverflow: 'ellipsis',
          whiteSpace: 'nowrap',
        }}>{source.name}</span>
        <span style={{
          display: 'block',
          marginTop: '3px',
          fontFamily: FONT.mono,
          fontSize: '10px',
          lineHeight: 1.5,
          color: source.error ? COLORS.error : COLORS.textMuted,
          wordBreak: 'break-word',
        }}>{note}</span>
      </span>
      {holds && (
        <span style={{
          flexShrink: 0,
          padding: '1px 5px',
          borderRadius: '3px',
          border: `1px solid ${COLORS.accentDim}`,
          color: COLORS.accentBright,
          fontFamily: FONT.mono,
          fontSize: '8.5px',
          fontWeight: 700,
          letterSpacing: '0.8px',
          textTransform: 'uppercase',
        }}>recoverable</span>
      )}
    </>
  )

  const style = {
    width: '100%',
    display: 'flex',
    alignItems: 'center',
    gap: '9px',
    textAlign: 'left',
    padding: '9px 11px',
    marginBottom: '6px',
    background: COLORS.bg,
    border: `1px solid ${selectable && selected ? COLORS.accent : COLORS.border}`,
    borderRadius: '6px',
  }

  if (!selectable) return <div style={style}>{row}</div>

  return (
    <button type="button" onClick={onSelect} aria-pressed={selected}
      style={{ ...style, cursor: 'pointer' }}>{row}</button>
  )
}

/* The dry run, phrased as what *would* happen. */
function PreviewReport({ report, at }) {
  return (
    <div style={{
      padding: '12px',
      marginBottom: '16px',
      background: COLORS.bg,
      border: `1px solid ${COLORS.border}`,
      borderRadius: '6px',
    }}>
      {/* Only when a backup was read: resuming re-points an index that is
          already here, and has no envelope with a date on it. */}
      {at && (
        <div style={{
          fontFamily: FONT.mono,
          fontSize: '10px',
          fontWeight: 700,
          letterSpacing: '1.2px',
          textTransform: 'uppercase',
          color: COLORS.textMuted,
          marginBottom: '10px',
        }}>
          Backup written {new Date(at).toLocaleString()}
        </div>
      )}

      <RecoveryFigures report={report} />
      <Shortfall report={report} />

      {report.lost === 0 && (
        <Banner tone="success">
          Every file the backup describes has enough parts on the accounts you have
          reconnected. Nothing would be left behind.
        </Banner>
      )}
    </div>
  )
}

/* What came back, as figures rather than a paragraph. */
function RecoveryFigures({ report }) {
  return (
    <div style={{ display: 'flex', flexWrap: 'wrap', gap: '18px', marginBottom: '14px' }}>
      <Figure value={`${report.recoverable}/${report.files}`} label="files openable" />
      <Figure value={formatBytes(report.recoverable_bytes)} label={`of ${formatBytes(report.bytes)}`} />
      <Figure value={report.folders} label={report.folders === 1 ? 'folder' : 'folders'} />
      {report.degraded > 0 && (
        <Figure value={report.degraded} label="with no spare part" tone={COLORS.warn} />
      )}
    </div>
  )
}

function Figure({ value, label, tone }) {
  return (
    <div>
      <div style={{
        fontFamily: FONT.mono,
        fontSize: '17px',
        fontWeight: 600,
        color: tone || COLORS.text,
        lineHeight: 1.2,
        letterSpacing: '-0.5px',
      }}>{value}</div>
      <div style={{
        fontFamily: FONT.mono,
        fontSize: '9px',
        fontWeight: 600,
        letterSpacing: '1.2px',
        textTransform: 'uppercase',
        color: COLORS.textMuted,
        marginTop: '3px',
      }}>{label}</div>
    </div>
  )
}

/* The half of the report that matters most: what did not come back, and the one
   thing that would change it — reconnect these accounts and run it again. */
function Shortfall({ report }) {
  if (!report.lost) return null

  return (
    <>
      <Banner tone="warn">
        {report.lost} of {report.files} file{report.files === 1 ? '' : 's'} did
        not come back — {formatBytes(report.lost_bytes)} of {formatBytes(report.bytes)}.
        A file is rebuilt from any two of its three parts, and these have fewer than two on
        the accounts you have reconnected.
      </Banner>

      {report.missing_accounts?.length > 0 && (
        <div style={{ marginBottom: '14px' }}>
          <SectionLabel>Connect these and recover again</SectionLabel>
          {report.missing_accounts.map((account) => (
            <div key={account.id} style={listRow}>
              <span style={{ fontSize: '12px', flexShrink: 0 }}>{KIND_ICONS[account.kind] || '☁'}</span>
              <span style={{ flex: 1, minWidth: 0, color: COLORS.text, wordBreak: 'break-word' }}>
                {account.name}
              </span>
              <span style={{ flexShrink: 0, color: account.blocking ? COLORS.warn : COLORS.textMuted }}>
                {account.blocking
                  ? `${account.files} file${account.files === 1 ? '' : 's'} need it`
                  : 'spares only'}
              </span>
            </div>
          ))}
        </div>
      )}

      {report.missing?.length > 0 && (
        <div style={{ marginBottom: '14px' }}>
          <SectionLabel>Files still missing</SectionLabel>
          <div style={{ maxHeight: '190px', overflowY: 'auto' }}>
            {report.missing.map((file) => (
              <div key={file.path} style={listRow}>
                <span style={{ flex: 1, minWidth: 0, color: COLORS.text, wordBreak: 'break-all' }}>
                  {file.path}
                </span>
                <span style={{ flexShrink: 0, color: COLORS.textMuted }}>
                  {formatBytes(file.size)} · {file.parts_found}/{file.parts_needed} parts
                </span>
              </div>
            ))}
          </div>
          {report.missing_truncated > 0 && (
            <div style={{ ...listRow, color: COLORS.textMuted, border: 'none' }}>
              … and {report.missing_truncated} more
            </div>
          )}
        </div>
      )}
    </>
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

const listRow = {
  display: 'flex',
  alignItems: 'baseline',
  gap: '10px',
  padding: '6px 0',
  borderBottom: `1px solid ${COLORS.border}`,
  fontFamily: FONT.mono,
  fontSize: '11px',
  lineHeight: 1.5,
}

const paragraph = {
  margin: '0 0 16px',
  fontFamily: FONT.sans,
  fontSize: '12px',
  lineHeight: 1.6,
  color: COLORS.textMuted,
}

/* The account to read the backup from: any of them carrying a foreign one,
   since every copy is the same. */
function preferredSource(scan) {
  const holder = (scan?.sources || []).find((s) => s.backup && s.foreign)
  return holder ? holder.provider_id : ''
}
