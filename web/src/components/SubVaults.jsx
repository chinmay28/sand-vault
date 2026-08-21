import React, { useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { api } from '../api'
import { Banner, Button, ConfirmDialog, Input, Modal, PasswordInput, Spinner } from './ui'
import ImportVault, { FoundVaults, useForeignVaults } from './ImportVault'

/* A sub vault is a vault inside the vault, with a password of its own.

   The main password lists them and does not open them, which is the whole
   point — and it is what this panel has to make legible, because a row that
   looked like a folder would suggest the wrong thing entirely. A locked sub
   vault shows what the main vault is allowed to know about it (a name, a file
   count, how much it is taking up on the accounts) and nothing else.

   None of this appears on a WebDAV mount, locked or unlocked. That is worth
   saying out loud in the panel rather than only in the docs: it is the reason
   somebody would put a file here rather than in a folder. */

export default function SubVaults({
  subVaults = [], showSubVaults, onToggleSubVaults, zIndex = 110, onClose, onChanged, onOpen,
}) {
  const [creating, setCreating] = useState(false)
  const [unlocking, setUnlocking] = useState(null)
  const [changing, setChanging] = useState(null)
  const [deleting, setDeleting] = useState(null)
  const [importing, setImporting] = useState(null)
  const [busy, setBusy] = useState(null)
  const [error, setError] = useState(null)

  /* An account you reconnect can be carrying a vault of its own. Asked when
     this opens rather than when the app does, since nothing before now had a
     use for the answer. */
  const foreign = useForeignVaults(true)

  const lock = async (sub) => {
    setBusy(sub.id)
    setError(null)
    try {
      await api.lockSubVault(sub.id)
      onChanged(sub.id)
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(null)
    }
  }

  return (
    <Modal
      title="Sub vaults"
      subtitle="Vaults inside this one, each with a password of its own"
      onClose={onClose}
      zIndex={zIndex}
      width={520}
    >
      <p style={note}>
        A sub vault is sealed under a password of its own. Your vault password lists
        it and cannot open it — and nothing inside one ever appears on a mounted
        drive, whether it is open or not.
      </p>

      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

      {subVaults.length === 0 && (
        <p style={{ ...note, color: COLORS.textMuted }}>
          No sub vaults yet.
        </p>
      )}

      <div style={{ display: 'flex', flexDirection: 'column', gap: '8px', margin: '12px 0' }}>
        {subVaults.map((sub) => (
          <div
            key={sub.id}
            style={{
              display: 'flex',
              alignItems: 'center',
              gap: '10px',
              padding: '10px 12px',
              background: COLORS.surfaceRaised,
              border: `1px solid ${sub.unlocked ? COLORS.accentDim || COLORS.border : COLORS.border}`,
              borderRadius: '8px',
            }}
          >
            <span aria-hidden="true" style={{ fontSize: '15px', opacity: 0.8 }}>
              {sub.unlocked ? '🔓' : '🔒'}
            </span>

            <span style={{ flex: 1, minWidth: 0 }}>
              <span style={{
                display: 'block',
                fontFamily: FONT.mono,
                fontSize: '12px',
                fontWeight: 600,
                color: COLORS.text,
                overflow: 'hidden',
                textOverflow: 'ellipsis',
                whiteSpace: 'nowrap',
              }}>{sub.label}</span>
              <span style={{ display: 'block', fontFamily: FONT.mono, fontSize: '9px', color: COLORS.textMuted, marginTop: '2px' }}>
                {/* The figures come from the inventory while it is shut, so they
                    do not drop to zero the moment it is locked. */}
                {sub.files} file{sub.files === 1 ? '' : 's'} · {formatBytes(sub.stored_bytes || 0)} stored
                {sub.pending_migration > 0 && ` · ${sub.pending_migration} still on an old key`}
              </span>
            </span>

            {busy === sub.id ? <Spinner /> : (
              <span style={{ display: 'flex', gap: '4px', flexShrink: 0 }}>
                {sub.unlocked ? (
                  <>
                    <Button size="sm" variant="ghost" onClick={() => onOpen(sub)}>Open</Button>
                    <Button size="sm" variant="ghost" onClick={() => lock(sub)}>Lock</Button>
                    <Button size="sm" variant="ghost" onClick={() => setChanging(sub)}
                      title="Change this sub vault's password">🔑</Button>
                  </>
                ) : (
                  <Button size="sm" onClick={() => setUnlocking(sub)}>Unlock</Button>
                )}
                <Button size="sm" variant="ghost" onClick={() => setDeleting(sub)}
                  title="Delete this sub vault and erase everything in it">🗑</Button>
              </span>
            )}
          </div>
        ))}
      </div>

      <Button size="sm" onClick={() => setCreating(true)}>+ New sub vault</Button>

      {/* Where they are drawn, which is a preference of this browser rather
          than a property of the vault. It changes what the file list shows and
          nothing else: a locked sub vault stays locked either way, and no
          setting puts one on a mounted drive. */}
      <label style={{
        display: 'flex', alignItems: 'center', gap: '9px', marginTop: '18px',
        paddingTop: '14px', borderTop: `1px solid ${COLORS.border}`,
        fontFamily: FONT.mono, fontSize: '11px', color: COLORS.text, cursor: 'pointer',
      }}>
        <input
          type="checkbox"
          checked={!!showSubVaults}
          onChange={(e) => onToggleSubVaults(e.target.checked)}
        />
        Show them at the top of the vault, locked ones included
      </label>

      <div style={{ marginTop: '22px', paddingTop: '16px', borderTop: `1px solid ${COLORS.border}` }}>
        <div style={{
          fontFamily: FONT.mono, fontSize: '11px', fontWeight: 600,
          letterSpacing: '0.5px', color: COLORS.text, marginBottom: '6px',
        }}>Vaults on your accounts</div>
        <FoundVaults
          found={foreign.found}
          scanning={foreign.scanning}
          onScan={foreign.scan}
          onImport={setImporting}
        />
      </div>

      {creating && (
        <CreateSubVault
          onClose={() => setCreating(false)}
          onCreated={(id) => { setCreating(false); onChanged(); if (id) onOpen({ id, unlocked: true }) }}
        />
      )}
      {unlocking && (
        <UnlockSubVault
          sub={unlocking}
          onClose={() => setUnlocking(null)}
          /* Marked open on the way through. onChanged is a round-trip to the
             server for a fresh status, and the walk in happens now — so the
             copy handed over has to carry the unlock that just happened, or
             whoever opens it is reading a list that still says locked and
             asks for the password a second time. */
          onUnlocked={() => {
            const sub = { ...unlocking, unlocked: true }
            setUnlocking(null)
            onChanged()
            onOpen(sub)
          }}
        />
      )}
      {changing && (
        <ChangeSubVaultPassword
          sub={changing}
          onClose={() => setChanging(null)}
          onChanged={() => { setChanging(null); onChanged() }}
        />
      )}
      {importing && (
        <ImportVault
          found={importing}
          onClose={() => setImporting(null)}
          onImported={() => { onChanged(); foreign.scan() }}
        />
      )}

      {deleting && (
        <DeleteSubVault
          sub={deleting}
          onClose={() => setDeleting(null)}
          onDeleted={() => { const id = deleting.id; setDeleting(null); onChanged(id) }}
        />
      )}
    </Modal>
  )
}

const note = {
  fontFamily: FONT.mono,
  fontSize: '10px',
  lineHeight: 1.6,
  color: COLORS.textDim,
  margin: '0 0 4px',
}

/* Making one. The warning is the important half: there is no recovery for this
   password, and unlike the vault's own there is no manifest backup anyone can
   fall back on either — the backup carries the sub vault as ciphertext. */
function CreateSubVault({ onClose, onCreated }) {
  const [label, setLabel] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  const submit = async (e) => {
    e.preventDefault()
    setError(null)
    if (!label.trim()) return
    if (password !== confirm) {
      setError('The two passwords do not match.')
      return
    }
    if (!password) {
      setError('A sub vault needs a password of its own.')
      return
    }

    setBusy(true)
    try {
      const created = await api.createSubVault(label.trim(), password)
      onCreated(created?.id)
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal title="New sub vault" subtitle="Sealed under a password of its own" onClose={onClose} zIndex={120}>
      <form onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}

        <Input
          label="Name"
          value={label}
          autoFocus
          placeholder="Taxes"
          onChange={(e) => setLabel(e.target.value)}
          help="Visible to anyone with your vault password. What is inside is not."
        />
        <PasswordInput label="Password" value={password} onChange={(e) => setPassword(e.target.value)} />
        <PasswordInput
          label="Confirm password"
          value={confirm}
          onChange={(e) => setConfirm(e.target.value)}
          help="There is no recovery for this one. Your vault password will not open it, and neither will a manifest backup — the backup carries this sub vault sealed."
        />

        <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end', marginTop: '16px' }}>
          <Button type="button" variant="ghost" onClick={onClose}>Cancel</Button>
          <Button type="submit" disabled={busy || !label.trim() || !password}>
            {busy ? <Spinner /> : 'Create'}
          </Button>
        </div>
      </form>
    </Modal>
  )
}

export function UnlockSubVault({ sub, onClose, onUnlocked }) {
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  const submit = async (e) => {
    e.preventDefault()
    setError(null)
    setBusy(true)
    try {
      await api.unlockSubVault(sub.id, password)
      setPassword('')
      onUnlocked()
    } catch (err) {
      setError(err.code === 'WRONG_PASSWORD'
        ? 'That is not this sub vault’s password.'
        : err.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal title={`Unlock ${sub.label}`} onClose={onClose} zIndex={120} width={420}>
      <form onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}
        <PasswordInput
          label="Sub vault password"
          value={password}
          autoFocus
          onChange={(e) => setPassword(e.target.value)}
          help="Not your vault password — this one is its own."
        />
        <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end', marginTop: '16px' }}>
          <Button type="button" variant="ghost" onClick={onClose}>Cancel</Button>
          <Button type="submit" disabled={busy || !password}>{busy ? <Spinner /> : 'Unlock'}</Button>
        </div>
      </form>
    </Modal>
  )
}

/* Changing a sub vault's password rotates the key its files are stored under,
   for the same reason the vault's own change does — so this can take a while on
   a full one, and says so. */
function ChangeSubVaultPassword({ sub, onClose, onChanged }) {
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const [report, setReport] = useState(null)

  const submit = async (e) => {
    e.preventDefault()
    setError(null)
    if (next !== confirm) {
      setError('The two new passwords do not match.')
      return
    }
    if (next === current) {
      setError('That is the password it already has.')
      return
    }

    setBusy(true)
    try {
      setReport(await api.changeSubVaultPassword(sub.id, current, next))
      onChanged()
    } catch (err) {
      setError(err.code === 'WRONG_PASSWORD'
        ? 'That is not this sub vault’s current password.'
        : err.message)
    } finally {
      setBusy(false)
    }
  }

  if (report) {
    return (
      <Modal title="Password changed" onClose={onClose} zIndex={120} width={420}>
        <Banner tone={report.remaining > 0 ? 'warning' : 'success'}>
          {report.remaining > 0
            ? `${report.remaining} file(s) are still stored under the old key. Until they move, the old password would still open their parts.`
            : `${sub.label} now opens with the new password, and the old one opens nothing.`}
        </Banner>
        <div style={{ display: 'flex', justifyContent: 'flex-end', marginTop: '16px' }}>
          <Button onClick={onClose}>Done</Button>
        </div>
      </Modal>
    )
  }

  return (
    <Modal
      title={`${sub.label} — change password`}
      subtitle={`${sub.files} file(s) will be re-encrypted`}
      onClose={onClose}
      zIndex={120}
      width={460}
    >
      <form onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}
        <p style={note}>
          The parts on your accounts are encrypted under a key held inside this sub
          vault rather than under the password, so a new password only means
          something if that key changes too. Every file in it is rebuilt onto a
          fresh key — a download and an upload each. Nothing is unreadable while
          that runs.
        </p>
        <PasswordInput label="Current sub vault password" value={current} autoFocus
          onChange={(e) => setCurrent(e.target.value)} />
        <PasswordInput label="New password" value={next} onChange={(e) => setNext(e.target.value)} />
        <PasswordInput label="Confirm new password" value={confirm} onChange={(e) => setConfirm(e.target.value)} />
        <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end', marginTop: '16px' }}>
          <Button type="button" variant="ghost" onClick={onClose} disabled={busy}>Cancel</Button>
          <Button type="submit" disabled={busy || !current || !next}>
            {busy ? <Spinner /> : 'Change and re-encrypt'}
          </Button>
        </div>
      </form>
    </Modal>
  )
}

/* Deleting one erases its parts from the accounts rather than merely forgetting
   it. A locked sub vault can still be erased — the vault keeps an inventory of
   what it owns — but it cannot be shown first, which is why that case asks
   twice. */
function DeleteSubVault({ sub, onClose, onDeleted }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  const confirm = async () => {
    setBusy(true)
    setError(null)
    try {
      await api.deleteSubVault(sub.id, !sub.unlocked)
      onDeleted()
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }

  return (
    <ConfirmDialog
      title={`Delete ${sub.label}?`}
      subtitle={`${sub.files} file(s), ${formatBytes(sub.stored_bytes || 0)} on your accounts`}
      confirmLabel="Delete and erase"
      busy={busy}
      onConfirm={confirm}
      /* Above the panel it was opened from, like every other dialog in here. */
      zIndex={120}
      /* The erasure runs across every account holding a part, so it can take a
         while. Dismissing it midway would leave that running with nothing left
         to report to, so while it runs the backdrop and Escape do not close. */
      onClose={() => !busy && onClose()}
    >
      {error && <Banner tone="error">{error}</Banner>}
      <p style={note}>
        Everything in it is erased from your cloud accounts. There is no undo, and
        no manifest backup to fall back on — the backups carry this sub vault
        sealed under the password you are about to throw away.
      </p>
      {!sub.unlocked && (
        <Banner tone="warning">
          It is locked, so what is about to go cannot be listed for you. It can
          still be erased: the vault records where each of its parts sits, without
          knowing what any of them are.
        </Banner>
      )}
    </ConfirmDialog>
  )
}
