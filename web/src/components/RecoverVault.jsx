import React, { useState } from 'react'
import { COLORS, FONT, KIND_ICONS, formatBytes } from '../theme'
import { api } from '../api'
import { Banner, Button, Modal, PasswordInput, Spinner } from './ui'
import { PARTS_PER_FILE } from './CloudSelect'

/* The dialog for the day the machine died.

   Every connected account carries an encrypted copy of the index, so a fresh
   install that reconnects those accounts is sitting on everything it needs to
   rebuild the vault — it just does not know it yet. The app notices, and this is
   what it opens: what was found, the password of the vault that is gone, a dry
   run, and then the rebuild.

   The dry run is not a nicety. A recovery is only as complete as the accounts
   that were reconnected, and finding out which files did not come back *after*
   adopting the index is far worse than being told first.

   Which is also why this dialog has two shapes. Recovering with two of your
   three clouds back leaves an index that knows about files it cannot reach; when
   the third turns up there is nothing left to adopt and everything left to
   re-point, so the same dialog resumes instead — no password, because the key
   was adopted the first time round. */
export default function RecoverVault({ scan, onClose, onRecovered }) {
  const [password, setPassword] = useState('')
  const [source, setSource] = useState(() => preferredSource(scan))
  const [busy, setBusy] = useState(null) // 'preview' | 'recover'
  const [error, setError] = useState(null)
  const [preview, setPreview] = useState(null)
  const [result, setResult] = useState(null)

  const resuming = !scan?.available && !!scan?.resumable
  const holders = (scan?.sources || []).filter((s) => s.backup && s.foreign)

  const run = async (dryRun) => {
    setError(null)
    setBusy(dryRun ? 'preview' : 'recover')
    try {
      const resp = resuming
        ? await api.resumeRecovery({ dryRun })
        : await api.recover({ providerId: source, password, dryRun })
      if (dryRun) setPreview(resp)
      else setResult(resp)
    } catch (err) {
      setError(err.code === 'WRONG_PASSWORD'
        ? 'That password does not open the backup on this account. It is the password of the vault you lost, which may not be the one this vault uses.'
        : err.message)
      if (!dryRun) setPreview(null)
    } finally {
      setBusy(null)
    }
  }

  /* ---- After the rebuild: what came back, and what did not ---- */
  if (result) {
    const report = result.report
    return (
      <Modal title={report.lost > 0 ? 'Recovery finished, in part' : 'Recovery complete'}
        onClose={onClose} width={620}>
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

        {report.warnings?.length > 0 && (
          <Banner tone="warn">
            <span style={{ fontFamily: FONT.mono, fontSize: '11px', whiteSpace: 'pre-wrap' }}>
              {report.warnings.join('\n')}
            </span>
          </Banner>
        )}

        <p style={paragraph}>
          {report.lost > 0
            ? 'Nothing has been thrown away: the index knows these files exist and where their parts went, and connecting the accounts above brings them back within reach.'
            : 'The accounts now carry a copy of the index under this vault’s password, so this machine can be lost too.'}
        </p>

        <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
          <Button variant="primary" onClick={onRecovered}>Open the vault</Button>
        </div>
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
          Connect them, then run this — it asks every account what it holds and re-points the
          index at whichever one answers. No password: the key is already here.
        </p>

        {preview && <PreviewReport report={preview.report} />}

        <div style={{ display: 'flex', justifyContent: 'flex-end', gap: '8px', flexWrap: 'wrap' }}>
          <Button variant="ghost" onClick={onClose} disabled={!!busy}>Not now</Button>
          <Button onClick={() => run(true)} disabled={!!busy}>
            {busy === 'preview' ? <Spinner size={12} /> : null}
            {busy === 'preview' ? 'Checking…' : 'Check what is reachable'}
          </Button>
          <Button variant="primary" onClick={() => run(false)} disabled={!!busy}>
            {busy === 'recover' ? <Spinner size={12} color={COLORS.bg} /> : null}
            {busy === 'recover' ? 'Finishing…' : 'Finish recovery'}
          </Button>
        </div>
      </Modal>
    )
  }

  /* ---- Before: what was found, and what a recovery would bring back ---- */
  return (
    <Modal
      title="Sand files detected"
      subtitle="These accounts are still carrying a vault. This one is empty, so it can take it over."
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

      {/* The prompt fires on the first cloud you reconnect, which is rarely the
          last one you mean to. Recovering now is not a mistake — the index
          comes back whole and the parts that were out of reach are picked up
          later — but it is worth knowing before rather than after. */}
      {(scan.sources || []).length < PARTS_PER_FILE && (
        <Banner tone="info">
          Only {scan.sources.length} cloud{scan.sources.length === 1 ? ' is' : 's are'} connected.
          A file is rebuilt from two of its three parts, so connect the rest first if you can —
          or recover now and finish once they turn up.
        </Banner>
      )}

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

      {preview && <PreviewReport report={preview.report} at={preview.backup_at} />}

      <div style={{ display: 'flex', justifyContent: 'flex-end', gap: '8px', flexWrap: 'wrap' }}>
        <Button variant="ghost" onClick={onClose} disabled={!!busy}>Not now</Button>
        <Button onClick={() => run(true)} disabled={!!busy || !password}>
          {busy === 'preview' ? <Spinner size={12} /> : null}
          {busy === 'preview' ? 'Checking…' : 'Check what is there'}
        </Button>
        <Button variant="primary" onClick={() => run(false)} disabled={!!busy || !password}>
          {busy === 'recover' ? <Spinner size={12} color={COLORS.bg} /> : null}
          {busy === 'recover' ? 'Recovering…' : 'Recover'}
        </Button>
      </div>

      <p style={{ ...paragraph, margin: '14px 0 0' }}>
        Recovering adopts the lost vault&apos;s encryption key, which is what makes the parts on
        those accounts readable again. It only runs against an empty vault, so nothing here can
        be overwritten.
      </p>
    </Modal>
  )
}

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
