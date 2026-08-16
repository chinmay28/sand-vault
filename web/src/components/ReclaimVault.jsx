import React, { useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { api } from '../api'
import { Banner, Button, Modal, Spinner } from './ui'
import { CloudChoice, PARTS_PER_FILE, initialSelection } from './CloudSelect'

/* Making recovered files your own.

   A recovery adopts the data key of the vault it rebuilt, because that key is
   the only thing that opens the parts already sitting on the accounts. It gets
   the files back, and it leaves them readable by the vault that died: the key
   is derived from the old password, and every copy of the old manifest.sand
   hands it over — including any that was taken off an account before this vault
   existed, which no amount of overwriting can reach.

   This is the step that ends it. A fresh key under the current password, every
   file rebuilt onto it, the old parts erased. And since every file is being
   gathered and scattered anyway, it is also the one cheap moment to say where
   they should live — the accounts a recovery lands on are the ones somebody
   else chose, on a machine that is gone. */
export default function ReclaimVault({ stats, providers, onClose, onDone }) {
  const [selected, setSelected] = useState(() => initialSelection(providers, stats?.default_accounts))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const [report, setReport] = useState(null)

  const files = stats?.files || 0
  const bytes = stats?.bytes || 0
  const enough = selected.length >= 2

  const run = async () => {
    setError(null)
    setBusy(true)
    try {
      setReport(await api.reclaim(selected))
      onDone?.()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  if (report) {
    const done = report.remaining === 0
    return (
      <Modal title={done ? 'These files are yours now' : 'Partly re-encrypted'} onClose={onClose} width={560}>
        <Banner tone={done ? 'success' : 'warn'}>
          {report.migrated} of {report.pending} file{report.pending === 1 ? '' : 's'} re-encrypted
          under a key of this vault&apos;s own ({formatBytes(report.bytes)}).
          {done && ' The parts the old password could open have been erased.'}
        </Banner>

        {report.remaining > 0 && (
          <Banner tone="warn">
            {report.remaining} file{report.remaining === 1 ? ' is' : 's are'} still on the old key
            and still readable. Nothing is lost — finish from the accounts panel when whatever
            held it up is fixed.
          </Banner>
        )}

        {report.warnings?.length > 0 && (
          <Banner tone="warn">
            <span style={{ fontFamily: FONT.mono, fontSize: '11px', whiteSpace: 'pre-wrap' }}>
              {report.warnings.join('\n')}
            </span>
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
      title="Re-encrypt under your own key"
      subtitle="These files came back on the key of the vault they came from, which the old password still opens."
      onClose={busy ? undefined : onClose}
      width={560}
    >
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

      <p style={paragraph}>
        Recovery had to adopt that key — it is the only thing that opens the parts already on your
        clouds. But it is derived from the password of the vault that died, and every copy of that
        vault&apos;s index backup carries it, including any taken off an account before this vault
        existed. Until these files move, whoever could read them before still can — and anything
        you upload in the meantime is sealed under the same key, so waiting widens that rather
        than holding it still.
      </p>

      <p style={paragraph}>
        This mints a fresh key under <em>your</em> password, rebuilds every file onto it, and erases
        the parts the old key opened. Your password does not change.
      </p>

      <SectionLabel>Store them on</SectionLabel>
      <p style={{ ...paragraph, marginBottom: '10px' }}>
        Every file is gathered and scattered anyway, so this is the cheap moment to move them off
        the clouds a vault you no longer have picked.
      </p>
      <CloudChoice providers={providers} selected={selected} onChange={setSelected} />

      {!enough && (
        <Banner tone="warn">
          Choose at least 2 clouds — a file is rebuilt from two of its three parts, and no single
          account may hold enough of one to rebuild it on its own.
        </Banner>
      )}
      {enough && selected.length < PARTS_PER_FILE && (
        <Banner tone="info">
          {selected.length} clouds means {selected.length} parts per file: enough to rebuild, with
          nothing to spare if one goes down.
        </Banner>
      )}

      <Banner tone="info">
        {files} file{files === 1 ? '' : 's'} ({formatBytes(bytes)}) will be gathered from your
        clouds, re-encrypted and scattered again. Keep this tab open — on a full vault it takes a
        while. Stopping is safe: whatever moved stays moved, and the accounts panel offers to
        finish the rest.
      </Banner>

      <div style={{ display: 'flex', justifyContent: 'flex-end', gap: '8px', flexWrap: 'wrap' }}>
        <Button variant="ghost" onClick={onClose} disabled={busy}>Not now</Button>
        <Button variant="primary" onClick={run} disabled={busy || !enough || files === 0}>
          {busy ? <Spinner size={12} color={COLORS.bg} /> : null}
          {busy ? 'Re-encrypting…' : 'Re-encrypt and move'}
        </Button>
      </div>
    </Modal>
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
