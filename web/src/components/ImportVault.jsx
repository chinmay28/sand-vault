import React, { useEffect, useState } from 'react'
import { COLORS, FONT } from '../theme'
import { api } from '../api'
import { Banner, Button, Input, Modal, PasswordInput, Spinner } from './ui'

/* Another vault, found on one of your accounts.

   Every vault replicates its index to every account it is connected to, so an
   account you have used before carries one. Reconnecting an old cloud after a
   machine died means you are looking straight at your own files without being
   told so — and until sub vaults existed the only way to get them back was a
   recovery, which refuses to run against a vault that already holds anything.

   Importing it as a sub vault dissolves that. What was found lands beside what
   is already here, with its own tree and its own password, and nothing has to
   be replaced. */

/* Which accounts hold a vault that is not this one. The scan is the recovery
   scan — the same question, already asked and answered over there — so this
   only picks the foreign ones out of its answer. */
export function useForeignVaults(enabled) {
  const [found, setFound] = useState([])
  const [scanning, setScanning] = useState(false)

  const scan = React.useCallback(async () => {
    setScanning(true)
    try {
      const resp = await api.recoveryScan()
      setFound((resp?.sources || []).filter((s) => s.backup && s.foreign))
    } catch {
      // A scan that fails is not worth interrupting anyone over: it is
      // offered, not asked for, and the button is still there to try again.
      setFound([])
    } finally {
      setScanning(false)
    }
  }, [])

  useEffect(() => { if (enabled) scan() }, [enabled, scan])
  return { found, scanning, scan }
}

/* The list, for the settings panel. Absent entirely when nothing was found,
   which is the ordinary case. */
export function FoundVaults({ found, scanning, onScan, onImport }) {
  return (
    <div>
      <p style={note}>
        Every vault keeps a copy of its index on each account it uses. If an
        account here holds one that is not this vault’s, it is another vault’s —
        an older install, or the machine you had before — and it can be brought
        in as a sub vault without replacing anything you have.
      </p>

      {found.length > 0 && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: '8px', margin: '12px 0' }}>
          {found.map((f) => (
            <div key={f.provider_id} style={{
              display: 'flex', alignItems: 'center', gap: '10px', padding: '10px 12px',
              background: COLORS.surfaceRaised, border: `1px solid ${COLORS.border}`, borderRadius: '8px',
            }}>
              <span aria-hidden="true" style={{ fontSize: '15px', opacity: 0.8 }}>🗝</span>
              <span style={{ flex: 1, minWidth: 0 }}>
                <span style={{ display: 'block', fontFamily: FONT.mono, fontSize: '12px', color: COLORS.text }}>
                  {f.name}
                </span>
                <span style={{ display: 'block', fontFamily: FONT.mono, fontSize: '9px', color: COLORS.textMuted, marginTop: '2px' }}>
                  holds another vault’s index
                </span>
              </span>
              <Button size="sm" onClick={() => onImport(f)}>Import</Button>
            </div>
          ))}
        </div>
      )}

      <Button size="sm" variant="ghost" onClick={onScan} disabled={scanning}>
        {scanning ? <Spinner /> : found.length > 0 ? 'Scan again' : 'Scan accounts for vaults'}
      </Button>
      {!scanning && found.length === 0 && (
        <span style={{ ...note, marginLeft: '10px' }}>Nothing but this vault’s own.</span>
      )}
    </div>
  )
}

const note = {
  fontFamily: FONT.mono,
  fontSize: '10px',
  lineHeight: 1.6,
  color: COLORS.textDim,
  margin: '0 0 4px',
}

/* Two passwords: the old vault's, to open what was found, and the one the sub
   vault will answer to from here.

   The second is free — the old data key is adopted as it stands, so no file is
   re-encrypted by the import itself — which is worth saying, because being
   asked to invent a password usually means waiting for something. */
export default function ImportVault({ found, onClose, onImported }) {
  const [backupPassword, setBackupPassword] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [label, setLabel] = useState(found.name || '')
  const [adopt, setAdopt] = useState(true)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const [report, setReport] = useState(null)

  const submit = async (e) => {
    e.preventDefault()
    setError(null)
    if (password !== confirm) {
      setError('The two new passwords do not match.')
      return
    }
    if (!password) {
      setError('Choose a password for the imported sub vault.')
      return
    }

    setBusy(true)
    try {
      setReport(await api.importVault({
        provider: found.provider_id,
        backupPassword,
        password,
        label: label.trim(),
        adoptBackup: adopt,
      }))
      onImported()
    } catch (err) {
      setError(err.code === 'WRONG_PASSWORD'
        ? 'That does not open the index on this account.'
        : err.message)
    } finally {
      setBusy(false)
    }
  }

  if (report) {
    return (
      <Modal title="Imported" onClose={onClose} zIndex={120} width={460}>
        <Banner tone="success">
          {report.files} file(s) in {report.folders} folder(s) are now the sub vault
          “{report.sub_vault?.label}”.
        </Banner>
        {report.unreachable > 0 && (
          <Banner tone="warning">
            {report.unreachable} part(s) are on accounts that are not connected here.
            Connect them and the files they belong to come back with them.
          </Banner>
        )}
        <p style={note}>
          The imported files are still encrypted under the key the old password
          opens. Changing this sub vault’s password rotates that key and rebuilds
          them onto it, which is what finally makes the old password useless.
        </p>
        <div style={{ display: 'flex', justifyContent: 'flex-end', marginTop: '16px' }}>
          <Button onClick={onClose}>Done</Button>
        </div>
      </Modal>
    )
  }

  return (
    <Modal
      title="Import a vault"
      subtitle={`Found on ${found.name}`}
      onClose={onClose}
      zIndex={120}
      width={480}
    >
      <form onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}

        <p style={note}>
          What is on this account becomes a sub vault of this one, with its own
          tree and its own password. Nothing here is replaced, and nothing is
          re-uploaded — the parts stay where they are.
        </p>

        <PasswordInput
          label="Password of the vault being imported"
          value={backupPassword}
          autoFocus
          onChange={(e) => setBackupPassword(e.target.value)}
          help="The one that vault used, on the machine it came from."
        />

        <Input label="Call it" value={label} onChange={(e) => setLabel(e.target.value)}
          placeholder={found.name} />

        <PasswordInput label="New password for the sub vault" value={password}
          onChange={(e) => setPassword(e.target.value)} />
        <PasswordInput
          label="Confirm"
          value={confirm}
          onChange={(e) => setConfirm(e.target.value)}
          help="Choosing a new one costs nothing — only the wrapping is redone, not a single file."
        />

        <label style={{
          display: 'flex', alignItems: 'flex-start', gap: '8px', marginTop: '12px',
          fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textDim, lineHeight: 1.6,
        }}>
          <input type="checkbox" checked={adopt} onChange={(e) => setAdopt(e.target.checked)}
            style={{ marginTop: '2px' }} />
          <span>
            Replace the old index on {found.name} with this vault’s.
            Leaving it in place means this vault never backs up to that account,
            and the old password can still recover those files on its own.
          </span>
        </label>

        <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end', marginTop: '16px' }}>
          <Button type="button" variant="ghost" onClick={onClose} disabled={busy}>Cancel</Button>
          <Button type="submit" disabled={busy || !backupPassword || !password}>
            {busy ? <Spinner /> : 'Import as a sub vault'}
          </Button>
        </div>
      </form>
    </Modal>
  )
}
