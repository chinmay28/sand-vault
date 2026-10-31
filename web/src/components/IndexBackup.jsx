import React, { useState } from 'react'
import { COLORS, FONT } from '../theme'
import { api } from '../api'
import { Modal } from './ui'
import { Choice } from './CloudHealth'

/* How far apart the copies of the index are written to the clouds.

   Every cloud holds manifest.sand, an encrypted copy of the index, so that a
   password and any one cloud bring the vault back if the machine holding it is
   lost. It is rewritten whenever the index changes — and the index changes
   with every upload, rename, move and delete, so a folder of a few hundred
   films going in or out is a few hundred rewrites on every cloud in a row,
   which is how a provider comes to answer "slow down". So the rewrites are
   paced: the first change after a quiet spell is written at once, and the
   changes that follow within the gap are folded into one more write when it
   has passed. This is where the gap is chosen.

   The choices are few and far apart on purpose, in the shape the cloud health
   interval uses: nobody tunes this to the second, and a figure means less the
   more precise it gets. "At once" is the old behaviour, for someone with a
   small vault and a provider that does not mind. */
const DELAYS = [
  { seconds: 0, label: 'At once' },
  { seconds: 5, label: '5 s' },
  { seconds: 30, label: '30 s' },
  { seconds: 120, label: '2 min' },
  { seconds: 600, label: '10 min' },
]

/* The settings line's reading: the gap, in the words the chips use. */
export function backupDelayLabel(seconds) {
  if (seconds === 0) return 'At once'
  const known = DELAYS.find((d) => d.seconds === seconds)
  if (known) return `Every ${known.label}`
  if (seconds % 60 === 0) return `Every ${seconds / 60} min`
  return `Every ${seconds} s`
}

export default function IndexBackup({ status, zIndex, onClose, onChanged }) {
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState(null)

  const enabled = status?.manifest_backup !== false
  const seconds = status?.manifest_backup_delay_seconds ?? 5
  const custom = !DELAYS.some((d) => d.seconds === seconds)

  const setDelay = async (next) => {
    setSaving(true)
    setError(null)
    try {
      await api.setManifestBackupDelay(next)
      onChanged()
    } catch (err) {
      setError(err.message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <Modal
      title="Index backup"
      subtitle="The encrypted copy of the index that every cloud holds"
      onClose={onClose}
      width={460}
      zIndex={zIndex}
    >
      <p style={{
        margin: '0 0 14px',
        fontFamily: FONT.sans,
        fontSize: '12px',
        lineHeight: 1.6,
        color: COLORS.textDim,
      }}>
        Each cloud keeps a sealed copy of the index, so your password and any
        one cloud bring the vault back if this machine is lost. It is rewritten
        whenever the index changes — every upload, rename, move and delete — so
        a folder going in or out is a run of rewrites on every cloud at once.
        Keeping them apart turns that run into a few writes instead.
      </p>

      {!enabled && (
        <p style={{
          margin: '0 0 14px',
          padding: '10px 12px',
          fontFamily: FONT.sans,
          fontSize: '11.5px',
          lineHeight: 1.6,
          color: COLORS.warn,
          background: COLORS.surfaceRaised,
          border: `1px solid ${COLORS.border}`,
          borderRadius: '6px',
        }}>
          The index backup is switched off for this vault, so nothing is written
          to the clouds whatever is chosen here. Losing the vault file means
          losing every stored file. It is switched back on from the command
          line, with <span style={{ fontFamily: FONT.mono }}>sand vault backup --enable</span>.
        </p>
      )}

      <div style={{
        fontFamily: FONT.mono,
        fontSize: '10px',
        fontWeight: 700,
        letterSpacing: '1.2px',
        textTransform: 'uppercase',
        color: COLORS.textMuted,
        marginBottom: '10px',
      }}>At most once every</div>

      <div style={{ display: 'flex', flexWrap: 'wrap', gap: '6px' }}>
        {DELAYS.map((choice) => (
          <Choice
            key={choice.seconds}
            label={choice.label}
            on={seconds === choice.seconds}
            disabled={saving}
            onClick={() => setDelay(choice.seconds)}
          />
        ))}
        {/* A figure set from the command line that none of the chips names is
            shown rather than silently rounded to one of them. */}
        {custom && (
          <Choice label={backupDelayLabel(seconds).replace(/^Every /, '')} on disabled />
        )}
      </div>

      <p style={{
        margin: '10px 0 0',
        fontFamily: FONT.sans,
        fontSize: '11px',
        lineHeight: 1.6,
        color: COLORS.textMuted,
      }}>
        The first change after a quiet spell is written at once. What follows
        within the gap waits for it to pass and goes out as one write, so the
        copies are never more than a gap and a write behind. Locking the vault
        writes nothing; the next unlock brings the copies up to date.
      </p>

      {error && (
        <p style={{
          margin: '12px 0 0',
          fontFamily: FONT.sans,
          fontSize: '11.5px',
          color: COLORS.error,
        }}>{error}</p>
      )}
    </Modal>
  )
}
