import React, { useEffect, useState } from 'react'
import { COLORS, FONT, formatBytes, formatDate } from '../theme'
import { api } from '../api'
import { saveBlob } from '../download'
import { Banner, Button, Input, Modal, PasswordInput, Spinner } from './ui'

/* The recovery kit, from the settings side: making one, testing one, and
   reading back the code for the one you already made.

   `sand vault recover` and the browser's own recovery prompt put the *index*
   back and leave you to reconnect every cloud by hand. That is the afternoon
   that actually costs somebody — the sign-ins, the S3 key that was in a
   password manager on the dead machine — and manifest.sand cannot carry any of
   it, because a copy of that file sits on every account and a credential inside
   it would let one compromised account unlock the rest.

   A kit never touches a cloud, so it can carry them. See docs/recovery-kit.md. */

const CHILD_Z = 120

export default function RecoveryKit({ onClose, zIndex }) {
  const [status, setStatus] = useState(null)
  const [useVaultPassword, setUseVaultPassword] = useState(false)
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const [minted, setMinted] = useState(null)
  const [open, setOpen] = useState(null)

  const load = () => api.kitStatus().then(setStatus).catch((err) => setError(err.message))
  useEffect(() => { load() }, [])

  const exportKit = async () => {
    setBusy(true)
    setError(null)
    try {
      const kit = await api.exportKit({ useVaultPassword, password })
      const name = `sand-recovery-kit-${new Date().toISOString().slice(0, 10)}.zip`
      saveBlob(kit.blob, name)
      setPassword('')
      await load()
      // The code panel is not a receipt to skim past: it is the only moment
      // this secret is put in front of anybody, so it takes over the dialog.
      if (kit.code) setMinted({ code: kit.code, kitId: kit.kitId, filename: name })
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  if (minted) {
    return (
      <CodePanel
        code={minted.code}
        kitId={minted.kitId}
        filename={minted.filename}
        onClose={() => { setMinted(null); onClose() }}
        zIndex={zIndex}
      />
    )
  }

  return (
    <Modal
      title="Recovery kit"
      subtitle="One sealed file that reconnects every cloud and brings the tree back"
      onClose={() => !busy && onClose()}
      width={520}
      zIndex={zIndex}
    >
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

      <Staleness status={status} />

      <p style={{
        margin: '0 0 18px', fontFamily: FONT.sans, fontSize: '12px',
        lineHeight: 1.6, color: COLORS.textDim,
      }}>
        A kit carries the credentials for every connected cloud, the key that opens your files,
        and the whole index. It is the one thing that turns a fresh install back into this vault
        without signing in to every service by hand. Nothing in it is ever written to a cloud.
      </p>

      <label style={{
        display: 'flex', alignItems: 'flex-start', gap: '9px',
        marginBottom: '16px', cursor: busy ? 'default' : 'pointer',
      }}>
        <input
          type="checkbox"
          checked={useVaultPassword}
          disabled={busy}
          onChange={(e) => setUseVaultPassword(e.target.checked)}
          style={{ marginTop: '2px', accentColor: COLORS.accent, flexShrink: 0 }}
        />
        <span style={{ display: 'flex', flexDirection: 'column', gap: '3px' }}>
          <span style={{ fontFamily: FONT.sans, fontSize: '12px', color: COLORS.text, lineHeight: 1.4 }}>
            Use my vault password instead
          </span>
          <span style={{ fontFamily: FONT.sans, fontSize: '11px', color: COLORS.textMuted, lineHeight: 1.45 }}>
            The kit will open with the password you are using today, not with whatever you
            change it to later.
          </span>
        </span>
      </label>

      {useVaultPassword && (
        <PasswordInput
          label="Vault password"
          value={password}
          autoComplete="current-password"
          placeholder="The password that opens this vault"
          onChange={(e) => setPassword(e.target.value)}
        />
      )}

      <div style={{
        display: 'flex', alignItems: 'flex-start', gap: '8px',
        padding: '9px 11px', marginBottom: '20px',
        background: COLORS.bg, border: `1px solid ${COLORS.border}`, borderRadius: '6px',
      }}>
        <span aria-hidden="true" style={{ color: COLORS.textMuted, fontFamily: FONT.mono }}>⚠</span>
        <span style={{ fontFamily: FONT.sans, fontSize: '11px', lineHeight: 1.5, color: COLORS.textMuted }}>
          Do not save the kit into a folder one of your clouds is syncing. It would be uploaded
          to the very account whose credentials it carries.
        </span>
      </div>

      <Button
        variant="primary"
        disabled={busy || (useVaultPassword && !password)}
        onClick={exportKit}
        style={{ width: '100%', justifyContent: 'center' }}
      >
        {busy ? <Spinner size={13} color={COLORS.bg} /> : null}
        {busy ? 'BUILDING…' : 'EXPORT A NEW KIT'}
      </Button>

      {status?.exported && (
        <div style={{ marginTop: '18px', borderTop: `1px solid ${COLORS.border}`, paddingTop: '16px' }}>
          <div style={{ display: 'flex', flexDirection: 'column', gap: '8px' }}>
            <Secondary
              label="Test this kit"
              hint="Proves every credential in it still works, and changes nothing"
              onClick={() => setOpen('verify')}
            />
            {status.code_retained && (
              <Secondary
                label="Show code"
                hint={`The recovery code for the kit you made on ${formatDate(status.exported_at)}`}
                onClick={() => setOpen('code')}
              />
            )}
          </div>
        </div>
      )}

      {open === 'verify' && <VerifyKit onClose={() => setOpen(null)} zIndex={CHILD_Z} />}
      {open === 'code' && (
        <RetainedCode kitId={status.kit_id} onClose={() => setOpen(null)} zIndex={CHILD_Z} />
      )}
    </Modal>
  )
}

/* What has changed since the last kit.

   It leads on the two facts that actually reduce what a kit can do — a new
   account, and a password change — rather than on its age. A kit that is merely
   old still recovers everything, because the import reads a newer index off the
   clouds; a kit that predates an account never held a credential for it and
   never will. */
function Staleness({ status }) {
  if (!status) return null

  if (!status.exported) {
    /* A vault with nothing connected has nothing a kit could carry, so this is
       an explanation rather than a warning. Nagging here would teach somebody
       to ignore the banner before it ever has something to say. */
    if (status.accounts === 0) {
      return (
        <Banner tone="info">
          A kit is worth making once you have connected a cloud or two — what it carries is the
          credentials for them, and there are none yet.
        </Banner>
      )
    }
    return (
      <Banner tone="warn">
        This vault has no recovery kit. Losing this machine would mean reconnecting all{' '}
        {status.accounts} of your clouds by hand before anything could be read back.
      </Banner>
    )
  }

  const reasons = []
  if (status.accounts_changed) {
    reasons.push('you have connected or removed an account since')
  }
  if (status.password_changed_since) {
    reasons.push('your vault password has changed since')
  }
  if (status.files_added > 0) {
    reasons.push(`${status.files_added.toLocaleString()} file(s) have been added since`)
  }

  const days = status.age_days || 0
  const age = days === 0 ? 'today' : days === 1 ? 'yesterday' : `${days} days ago`
  if (reasons.length === 0) {
    return <Banner tone="success">Your kit is from {age} and nothing has changed since.</Banner>
  }

  return (
    <Banner tone={status.accounts_changed || status.password_changed_since ? 'warn' : 'info'}>
      Your kit is from {age}, and {reasons.join('; ')}.
      {status.accounts_changed
        ? ' An account it does not know about is the one thing an import could not bring back on its own.'
        : ''}
    </Banner>
  )
}

function Secondary({ label, hint, onClick }) {
  const [hover, setHover] = useState(false)
  return (
    <button
      type="button"
      onClick={onClick}
      onPointerEnter={(e) => { if (e.pointerType === 'mouse') setHover(true) }}
      onPointerLeave={() => setHover(false)}
      style={{
        display: 'flex', alignItems: 'center', gap: '12px', width: '100%',
        minHeight: '52px', padding: '10px 12px',
        background: hover ? COLORS.surfaceHover : COLORS.surfaceRaised,
        border: `1px solid ${hover ? COLORS.borderBright : COLORS.border}`,
        borderRadius: '8px', cursor: 'pointer', textAlign: 'left',
      }}
    >
      <span style={{ display: 'flex', flexDirection: 'column', gap: '3px', flex: 1, minWidth: 0 }}>
        <span style={{
          fontFamily: FONT.mono, fontSize: '12px', fontWeight: 600,
          letterSpacing: '0.5px', color: COLORS.text,
        }}>{label}</span>
        <span style={{ fontFamily: FONT.sans, fontSize: '11px', lineHeight: 1.45, color: COLORS.textMuted }}>
          {hint}
        </span>
      </span>
      <span aria-hidden="true" style={{ color: COLORS.textMuted, fontFamily: FONT.mono }}>›</span>
    </button>
  )
}

/* The one screen in SAND that must not be skimmed past.

   It behaves like a modal with a job: the code large and grouped, a copy and a
   save button, and a dismiss that waits on an acknowledgement. Not because a
   checkbox proves anything, but because the alternative is a panel people close
   by reflex, and this is the reflex that costs them the vault.

   It is a deliberately soft gate — Show code in settings is right there
   underneath, and the panel says so. Someone who ticks without reading has lost
   nothing, because the vault still holds the code. The gate is for the person
   who would otherwise never register that a second artefact exists at all. */
export function CodePanel({ code, kitId, filename, onClose, zIndex }) {
  const [written, setWritten] = useState(false)
  const [copied, setCopied] = useState(false)

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(code)
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    } catch {
      // A browser that refuses the clipboard leaves the code on screen to be
      // read, which is what it is there for.
    }
  }

  const save = () => {
    const body = [
      'SAND recovery kit — recovery code',
      '',
      `    ${code}`,
      '',
      `Kit id   ${kitId}`,
      `Archive  ${filename || 'sand-recovery-kit.zip'}`,
      '',
      'This code is not inside the kit and cannot be recovered from it.',
      'Keep the two apart — together, in one place, they are the vault.',
      '',
    ].join('\n')
    saveBlob(new Blob([body], { type: 'text/plain' }), 'sand-recovery-code.txt')
  }

  return (
    <Modal
      title="Your recovery code"
      subtitle="Write this down. It is the only thing that opens the kit you just saved."
      onClose={() => written && onClose()}
      width={560}
      zIndex={zIndex}
    >
      <div style={{
        padding: '22px 18px 18px', marginBottom: '16px',
        background: COLORS.bg, border: `1px solid ${COLORS.accentDim}`,
        borderRadius: '8px', textAlign: 'center',
      }}>
        <div style={{
          display: 'flex', alignItems: 'center', justifyContent: 'center',
          gap: '5px', flexWrap: 'wrap',
          /* Sized to land 25 symbols and four hyphens on one line inside a
             560px dialog. It wraps rather than clips on a narrow phone, which
             is the right failure — a code split across two lines is still
             readable, and a clipped one is not. */
          fontFamily: FONT.mono, fontSize: '18px', fontWeight: 700,
          letterSpacing: '2px', color: COLORS.text,
        }}>
          {code.split('-').map((group, i) => (
            <React.Fragment key={group + i}>
              {i > 0 && <span style={{ color: '#475569', fontWeight: 400 }}>-</span>}
              <span>{group}</span>
            </React.Fragment>
          ))}
        </div>
        {kitId && (
          <div style={{
            marginTop: '14px', fontFamily: FONT.mono, fontSize: '10px',
            letterSpacing: '1.5px', textTransform: 'uppercase', color: COLORS.textMuted,
          }}>Kit {kitId.slice(0, 8)}</div>
        )}
      </div>

      <div style={{ display: 'flex', gap: '8px', marginBottom: '18px' }}>
        <Button onClick={copy} style={{ flex: 1, justifyContent: 'center' }}>
          {copied ? 'COPIED' : 'COPY'}
        </Button>
        <Button onClick={save} style={{ flex: 1, justifyContent: 'center' }}>
          SAVE AS A TEXT FILE
        </Button>
      </div>

      <Banner tone="info">
        This code is <strong>not inside the kit</strong>, and cannot be recovered from it.
        Keep the two apart — together, they are your vault.
      </Banner>

      <label style={{
        display: 'flex', alignItems: 'flex-start', gap: '9px',
        padding: '12px', marginBottom: '16px',
        background: COLORS.surfaceRaised, border: `1px solid ${COLORS.borderBright}`,
        borderRadius: '8px', cursor: 'pointer',
      }}>
        <input
          type="checkbox"
          checked={written}
          onChange={(e) => setWritten(e.target.checked)}
          style={{ marginTop: '2px', accentColor: COLORS.accent, flexShrink: 0 }}
        />
        <span style={{ fontFamily: FONT.sans, fontSize: '12px', color: COLORS.text, lineHeight: 1.5 }}>
          I have written this down, or saved it somewhere other than beside the kit
        </span>
      </label>

      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '12px' }}>
        <span style={{
          flex: 1, fontFamily: FONT.sans, fontSize: '11px',
          lineHeight: 1.5, color: COLORS.textMuted,
        }}>
          You can see it again under <span style={{ color: COLORS.textDim }}>Recovery kit → Show code</span>,
          for as long as this vault is working.
        </span>
        <Button variant="primary" disabled={!written} onClick={onClose}>DONE</Button>
      </div>
    </Modal>
  )
}

/* Reading a retained code back. */
function RetainedCode({ kitId, onClose, zIndex }) {
  const [code, setCode] = useState(null)
  const [error, setError] = useState(null)

  useEffect(() => {
    api.kitCode(kitId)
      .then((resp) => setCode(resp.code))
      .catch((err) => setError(err.message))
  }, [kitId])

  if (code) {
    return <CodePanel code={code} kitId={kitId} onClose={onClose} zIndex={zIndex} />
  }
  return (
    <Modal title="Recovery code" onClose={onClose} width={480} zIndex={zIndex}>
      {error
        ? <Banner tone="error">{error}</Banner>
        : <div style={{ display: 'flex', justifyContent: 'center', padding: '20px' }}><Spinner /></div>}
    </Modal>
  )
}

/* The fire drill.

   It asks for the code rather than opening the kit from the running vault's own
   keys, and that is half of why it exists: the failure nothing else here can
   catch is the slip of paper that went missing in a house move, and the only
   way to catch it is to make somebody find the paper. */
function VerifyKit({ onClose, zIndex }) {
  const [file, setFile] = useState(null)
  const [envelope, setEnvelope] = useState(null)
  const [secret, setSecret] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const [report, setReport] = useState(null)

  /* Inspected before the field is drawn, as the import does it: a kit sealed
     under the vault password must not be asked for a recovery code. */
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

  const run = async () => {
    setBusy(true)
    setError(null)
    try {
      setReport(await api.verifyKit({ file, secret }))
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      title="Kit test"
      subtitle="If you needed this kit today, here is what you would get back"
      onClose={() => !busy && onClose()}
      width={520}
      zIndex={zIndex}
    >
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

      {!report && (
        <>
          <p style={{
            margin: '0 0 16px', fontFamily: FONT.sans, fontSize: '12px',
            lineHeight: 1.6, color: COLORS.textDim,
          }}>
            Every credential in the kit is tried against the real account, and its index is
            checked against what is actually there. Nothing is written anywhere.
          </p>

          <FilePicker file={file} onPick={pick} />

          {envelope && (
            wantsCode ? (
              <Input
                label="Recovery code"
                value={secret}
                spellCheck={false}
                autoCapitalize="characters"
                autoComplete="off"
                placeholder="XXXXX-XXXXX-XXXXX-XXXXX-XXXXX"
                help="Asked for on purpose — finding it is half of what this test is for."
                onChange={(e) => setSecret(e.target.value)}
                style={{ letterSpacing: '1.5px', fontSize: '14px' }}
              />
            ) : (
              <PasswordInput
                label="Vault password for this kit"
                value={secret}
                placeholder="The password in use when it was made"
                help="This kit was sealed under a password rather than a code."
                onChange={(e) => setSecret(e.target.value)}
              />
            )
          )}

          <Button
            variant="primary"
            disabled={busy || !file || !secret}
            onClick={run}
            style={{ width: '100%', justifyContent: 'center' }}
          >
            {busy ? <Spinner size={13} color={COLORS.bg} /> : null}
            {busy ? 'TESTING…' : 'TEST THIS KIT'}
          </Button>
        </>
      )}

      {report && <VerifyReport report={report} />}
    </Modal>
  )
}

function VerifyReport({ report }) {
  const short = report.kit_files - report.recoverable

  return (
    <>
      <div style={{
        padding: '16px', marginBottom: '16px',
        background: COLORS.surfaceRaised, border: `1px solid ${COLORS.border}`, borderRadius: '8px',
      }}>
        <div style={{ fontFamily: FONT.mono, fontSize: '26px', fontWeight: 700, color: COLORS.text, lineHeight: 1.1 }}>
          {report.recoverable.toLocaleString()}
          <span style={{ fontSize: '15px', fontWeight: 400, color: COLORS.textMuted }}>
            {' '}of {report.kit_files.toLocaleString()}
          </span>
        </div>
        <div style={{ marginTop: '6px', fontFamily: FONT.sans, fontSize: '11.5px', color: COLORS.textMuted, lineHeight: 1.45 }}>
          files this kit alone would bring back, if every cloud were also out of reach
        </div>
      </div>

      <div style={{ display: 'flex', flexDirection: 'column', gap: '6px', marginBottom: '16px' }}>
        <Row
          tone={report.unusable === 0 ? COLORS.success : COLORS.warn}
          label={`${report.working} of ${report.accounts.length} credentials in the kit still work`}
        />
        {report.accounts.filter((a) => a.status !== 'connected').map((a) => (
          <Row key={a.id} tone={COLORS.warn} label={a.name} detail={a.detail || a.status} indent />
        ))}
        {report.accounts_added?.map((a) => (
          <Row
            key={a.id}
            tone={COLORS.warn}
            label={a.name}
            detail="connected after this kit was made — it holds no credentials for it"
            indent
          />
        ))}
        {report.added_since > 0 && (
          <Row tone={COLORS.info} label={`${report.added_since.toLocaleString()} file(s) added since this kit was made`} />
        )}
      </div>

      <Banner tone="info">
        In a real recovery your clouds would almost certainly be reachable, and the newer index
        on them would close this gap — the figure above is the floor, not the forecast.
      </Banner>

      {(short > 0 || report.added_since > 0 || report.accounts_added?.length > 0 || report.unusable > 0) && (
        <p style={{ margin: 0, fontFamily: FONT.sans, fontSize: '11.5px', color: COLORS.textDim, lineHeight: 1.55 }}>
          Export a fresh kit to close it properly.
        </p>
      )}
    </>
  )
}

function Row({ tone, label, detail, indent }) {
  return (
    <div style={{
      display: 'flex', alignItems: 'center', gap: '10px',
      padding: '10px 12px', marginLeft: indent ? '16px' : 0,
      background: COLORS.surfaceRaised, border: `1px solid ${COLORS.border}`, borderRadius: '6px',
    }}>
      <span aria-hidden="true" style={{ color: tone, fontFamily: FONT.mono, fontWeight: 700, flexShrink: 0 }}>
        {tone === COLORS.success ? '✓' : tone === COLORS.warn ? '⚠' : 'ℹ'}
      </span>
      <span style={{ display: 'flex', flexDirection: 'column', gap: '2px', flex: 1, minWidth: 0 }}>
        <span style={{ fontFamily: FONT.sans, fontSize: '12px', color: COLORS.text }}>{label}</span>
        {detail && (
          <span style={{ fontFamily: FONT.sans, fontSize: '10.5px', color: COLORS.textMuted, lineHeight: 1.4 }}>
            {detail}
          </span>
        )}
      </span>
    </div>
  )
}

/* Shared by the drill here and the import on the lock screen. */
export function FilePicker({ file, onPick }) {
  const [over, setOver] = useState(false)

  return (
    <label
      onDragOver={(e) => { e.preventDefault(); setOver(true) }}
      onDragLeave={() => setOver(false)}
      onDrop={(e) => {
        e.preventDefault()
        setOver(false)
        if (e.dataTransfer.files?.[0]) onPick(e.dataTransfer.files[0])
      }}
      style={{
        display: 'flex', alignItems: 'center', gap: '11px',
        padding: '14px 12px', marginBottom: '16px',
        background: COLORS.bg,
        border: `1px ${over ? 'solid' : 'dashed'} ${over ? COLORS.accent : COLORS.border}`,
        borderRadius: '8px', cursor: 'pointer',
      }}
    >
      <input
        type="file"
        accept=".zip,.sand,application/zip"
        onChange={(e) => e.target.files?.[0] && onPick(e.target.files[0])}
        style={{ display: 'none' }}
      />
      <span style={{ display: 'flex', flexDirection: 'column', gap: '3px', flex: 1, minWidth: 0 }}>
        <span style={{
          fontFamily: FONT.mono, fontSize: '12px', color: file ? COLORS.text : COLORS.textDim,
          overflowWrap: 'anywhere',
        }}>
          {file ? file.name : 'Choose your recovery kit, or drop it here'}
        </span>
        {file && (
          <span style={{ fontFamily: FONT.sans, fontSize: '11px', color: COLORS.textMuted }}>
            {formatBytes(file.size)}
          </span>
        )}
      </span>
      <span style={{ fontFamily: FONT.mono, fontSize: '10.5px', color: COLORS.textMuted, flexShrink: 0 }}>
        {file ? 'Change' : 'Browse'}
      </span>
    </label>
  )
}
