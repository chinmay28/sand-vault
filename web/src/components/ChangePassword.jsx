import React, { useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { api } from '../api'
import { Banner, Button, Modal, PasswordInput, Spinner } from './ui'

/* Changing the password is not a metadata edit. The parts sitting on your cloud
   accounts are encrypted under a random key held inside the vault, not under
   the password, so a new password that left that key alone would change
   nothing about what an old password can still read. Instead a new key is
   generated and every stored file is rebuilt onto it — which is a download and
   an upload per file, and why this dialog talks about time rather than just
   asking for two strings. */
export default function ChangePassword({ stats, onClose, onChanged }) {
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [migrate, setMigrate] = useState(true)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const [report, setReport] = useState(null)

  const files = stats?.files || 0
  const bytes = stats?.bytes || 0

  const submit = async (e) => {
    e.preventDefault()
    setError(null)

    if (!current || !next) return
    if (next !== confirm) {
      setError('The two new passwords do not match.')
      return
    }
    if (next === current) {
      setError('That is the password you already have.')
      return
    }

    setBusy(true)
    try {
      const result = await api.changePassword(current, next, { migrate })
      setCurrent('')
      setNext('')
      setConfirm('')
      setReport(result)
      onChanged()
    } catch (err) {
      setError(err.code === 'WRONG_PASSWORD' ? 'That is not your current password.' : err.message)
    } finally {
      setBusy(false)
    }
  }

  if (report) {
    return (
      <Modal title="Password changed" onClose={onClose}>
        <Banner tone="success">
          Your vault now opens with the new password, and the old one opens nothing.
        </Banner>

        <p style={paragraph}>
          {report.migrated > 0
            ? `${report.migrated} file${report.migrated === 1 ? '' : 's'} re-encrypted under the new key (${formatBytes(report.bytes)}). The parts the old key opened have been erased from your accounts.`
            : 'No files needed re-encrypting.'}
        </p>

        {report.remaining > 0 && (
          <Banner tone="warn">
            {report.remaining} file{report.remaining === 1 ? ' is' : 's are'} still on the old
            key. Until they move, someone with your old password and an old copy of the vault
            file could still read their parts — finish it from the accounts panel whenever the
            accounts holding them are reachable.
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
      title="Change vault password"
      subtitle="Your files are encrypted under a key inside the vault, not under your password — so changing it means re-encrypting them."
      onClose={busy ? undefined : onClose}
    >
      <form onSubmit={submit}>
        {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

        <PasswordInput
          label="Current password"
          value={current}
          autoFocus
          autoComplete="current-password"
          disabled={busy}
          onChange={(e) => setCurrent(e.target.value)}
        />
        <PasswordInput
          label="New password"
          value={next}
          autoComplete="new-password"
          placeholder="Choose a strong passphrase"
          disabled={busy}
          onChange={(e) => setNext(e.target.value)}
        />
        <PasswordInput
          label="Confirm new password"
          value={confirm}
          autoComplete="new-password"
          placeholder="Type it again"
          disabled={busy}
          help="There is no recovery. If you lose this password, nothing can rebuild your files."
          onChange={(e) => setConfirm(e.target.value)}
        />

        <label style={{
          display: 'flex',
          alignItems: 'flex-start',
          gap: '10px',
          padding: '11px 12px',
          marginBottom: '14px',
          background: COLORS.bg,
          border: `1px solid ${COLORS.border}`,
          borderRadius: '6px',
          cursor: busy ? 'not-allowed' : 'pointer',
        }}>
          <input
            type="checkbox"
            checked={migrate}
            disabled={busy}
            onChange={(e) => setMigrate(e.target.checked)}
            style={{ marginTop: '2px', accentColor: COLORS.accent }}
          />
          <span style={{ fontFamily: FONT.sans, fontSize: '12px', lineHeight: 1.55, color: COLORS.text }}>
            Re-encrypt my files now
            <span style={{ display: 'block', marginTop: '4px', color: COLORS.textMuted }}>
              {migrate
                ? `${files} file${files === 1 ? '' : 's'} (${formatBytes(bytes)}) will be gathered from your accounts, re-encrypted and scattered again. Keep this tab open — on a full vault it takes a while.`
                : 'The password changes straight away and your files stay readable, but their parts keep answering to the old key until you finish the job from the accounts panel.'}
            </span>
          </span>
        </label>

        <div style={{ display: 'flex', justifyContent: 'flex-end', gap: '8px' }}>
          <Button type="button" variant="ghost" onClick={onClose} disabled={busy}>Cancel</Button>
          <Button type="submit" variant="primary" disabled={busy || !current || !next}>
            {busy ? <Spinner size={12} color={COLORS.bg} /> : null}
            {busy
              ? (migrate ? 'Re-encrypting…' : 'Changing…')
              : 'Change password'}
          </Button>
        </div>

        {busy && migrate && files > 0 && (
          <p style={{ ...paragraph, marginBottom: 0, marginTop: '14px' }}>
            Every file is committed as it moves, so closing this tab cannot leave one
            half-encrypted — it only leaves the rest for later.
          </p>
        )}
      </form>
    </Modal>
  )
}

const paragraph = {
  margin: '0 0 16px',
  fontFamily: FONT.sans,
  fontSize: '12px',
  lineHeight: 1.6,
  color: COLORS.textMuted,
}
