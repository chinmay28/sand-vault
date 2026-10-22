import React, { useEffect, useMemo, useState } from 'react'
import { COLORS, FONT, KIND_ICONS, accountColor, formatBytes } from '../theme'
import { api } from '../api'
import { useIsMobile } from '../hooks'
import { Banner, Button, Modal, Spinner } from './ui'
import { ThresholdChoice, defaultSchemeFor, schemeName, usableCut } from './CloudSelect'

/* Second copies.

   A replica is a second, independent placement of a file: the same bytes cut
   again, under their own archive ID and the current key, into a *replica
   folder* on each of the clouds the policy names. Those clouds are ones
   already connected. The replica folder is a second folder beside the one
   their parts are in, and it is the only thing that tells a replica object
   from a part — nothing on a cloud names a file, and nothing on a replica
   says it is one.

   With one cloud a replica is the whole sealed file on that cloud, which is
   the one placement in SAND where a single account holds a file. The dialog
   says so plainly rather than hiding it, because somebody choosing one cloud
   is choosing exactly that. With several a replica is cut k-of-n like
   anything else, and the same three trades the upload picker shows apply.

   What lives here: the settings dialog, the folder dialog a row's menu opens,
   the outlined badges a file row draws beside its parts, and the control
   that says which placement a read is served from. */

/* What the settings line says on the right. */
export function replicationStatusLine(stats) {
  const r = stats?.replication
  if (!r) return 'Not set'
  const parts = [`${r.accounts} cloud${r.accounts === 1 ? '' : 's'}`, r.scheme === 'whole' ? 'whole copy' : r.scheme]
  if (r.paused) parts.push('paused')
  else if (r.pending > 0) parts.push(`${r.pending} to go`)
  else parts.push(`${r.replicated} copied`)
  return parts.join(' · ')
}

export function replicationStatusTone(stats) {
  const r = stats?.replication
  if (!r) return COLORS.textMuted
  if (r.pending > 0 && !r.paused) return COLORS.warn
  return COLORS.textDim
}

/* The folder an account keeps its parts in, as the backend names it, for
   showing beside the replica folder field. */
function partsFolderOf(provider) {
  const o = provider.options || {}
  return o.folder || o.prefix || o.path || ''
}

/* A placeholder a person can take as is: a sibling of the parts folder. */
function suggestedReplicaFolder(provider) {
  const parts = partsFolderOf(provider)
  if (!parts) return provider.kind === 'local' || provider.kind === 'sftp' ? '/path/to/replicas' : 'sand-replica'
  const trimmed = parts.replace(/\/+$/, '')
  return `${trimmed}-replica`
}

const label = {
  fontFamily: FONT.mono, fontSize: '10px', fontWeight: 600, letterSpacing: '1.5px',
  textTransform: 'uppercase', color: COLORS.textMuted, margin: '16px 0 8px',
}

/* The settings dialog: which clouds, where on each, how a replica is cut, how
   much a day — and how far the worker has got. */
export function ReplicationSettings({ providers, stats, zIndex, onClose, onChanged }) {
  const mobile = useIsMobile()
  const [status, setStatus] = useState(null)
  const [loadError, setLoadError] = useState(null)
  const [selected, setSelected] = useState([])
  const [folders, setFolders] = useState({})
  const [threshold, setThreshold] = useState(null)
  const [perDay, setPerDay] = useState('')
  const [paused, setPaused] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  const defaults = new Set(stats?.default_accounts || [])

  const load = () => api.replicationStatus()
    .then((s) => {
      setStatus(s)
      const policy = s.policy
      const ids = (policy?.accounts || []).filter((id) => providers.some((p) => p.id === id))
      setSelected(ids)
      setFolders({ ...(s.folders || {}) })
      setThreshold(policy && policy.data_shards > 1 ? policy.data_shards : null)
      setPerDay(policy?.bytes_per_day ? String(Math.round((policy.bytes_per_day / 2 ** 30) * 10) / 10) : '')
      setPaused(!!policy?.paused)
    })
    .catch((err) => setLoadError(err.message))

  useEffect(() => { load() }, []) // eslint-disable-line react-hooks/exhaustive-deps

  const toggle = (id) => {
    setSelected((prev) => (prev.includes(id) ? prev.filter((s) => s !== id) : [...prev, id]))
  }

  const scheme = useMemo(() => {
    if (selected.length === 0) return null
    if (selected.length === 1) return { data: 1, total: 1 }
    const suggested = defaultSchemeFor(selected.length)
    if (!suggested) return null
    if (threshold === null) return suggested
    return { data: Math.min(Math.max(threshold, 2), selected.length), total: selected.length }
  }, [selected.length, threshold])

  const whole = selected.length === 1
  const cutOK = selected.length === 0 || whole || usableCut(selected.length, scheme)
  const foldersOK = selected.every((id) => (folders[id] || '').trim() !== '')
  const bytesPerDay = perDay.trim() === '' ? 0 : Math.round(parseFloat(perDay) * 2 ** 30)
  const perDayOK = perDay.trim() === '' || (Number.isFinite(parseFloat(perDay)) && parseFloat(perDay) >= 0)

  const save = async (accounts) => {
    setBusy(true)
    setError(null)
    try {
      await api.setReplication({
        accounts: accounts.map((id) => ({ id, replica_folder: (folders[id] || '').trim() })),
        scheme: accounts.length === 0 ? '' : whole ? 'whole' : schemeName(scheme),
        bytesPerDay,
        paused,
      })
      onChanged()
      onClose()
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }

  return (
    <Modal
      title="Replication"
      subtitle="Chosen folders get a second copy, placed and sealed on its own, on clouds you already have"
      onClose={busy ? undefined : onClose}
      width={560}
      zIndex={zIndex}
    >
      {loadError && <Banner tone="error">{loadError}</Banner>}
      {error && <Banner tone="error">{error}</Banner>}
      {!status && !loadError && (
        <div style={{ padding: '24px', textAlign: 'center' }}><Spinner size={18} /></div>
      )}

      {status && (
        <>
          <ReplicationProgress status={status} providers={providers} />

          <div style={label}>Replication clouds</div>
          <p style={{ margin: '0 0 8px', fontFamily: FONT.sans, fontSize: '12px', color: COLORS.textMuted, lineHeight: 1.5 }}>
            Any connected cloud. Each one you tick needs a folder of its own for replicas,
            beside the one its parts are in — not inside it.
          </p>
          <div style={{ display: 'flex', flexDirection: 'column', gap: '6px' }}>
            {providers.map((p) => {
              const on = selected.includes(p.id)
              const color = accountColor(p.id)
              return (
                <div key={p.id}>
                  <button
                    type="button"
                    onClick={() => toggle(p.id)}
                    style={{
                      display: 'flex', alignItems: 'center', gap: '9px', width: '100%',
                      padding: '8px 10px', background: COLORS.bg,
                      border: `1px solid ${on ? color : COLORS.border}`, borderRadius: '6px',
                      cursor: 'pointer', textAlign: 'left', color: COLORS.text,
                      fontFamily: FONT.mono, fontSize: '11.5px', minHeight: mobile ? '44px' : undefined,
                    }}
                  >
                    <span aria-hidden="true" style={{
                      width: '14px', height: '14px', borderRadius: '3px', flexShrink: 0,
                      border: `1px solid ${on ? COLORS.accentBright : COLORS.borderBright}`,
                      background: on ? COLORS.accentBright : 'transparent',
                      color: COLORS.bg, fontSize: '10px', display: 'inline-flex',
                      alignItems: 'center', justifyContent: 'center',
                    }}>{on ? '✓' : ''}</span>
                    <span style={{ width: '12px', height: '12px', borderRadius: '3px', background: color, flexShrink: 0 }} />
                    <span style={{ color: COLORS.textMuted }}>{KIND_ICONS[p.kind] || '☁'}</span>
                    <span style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{p.name}</span>
                    {defaults.has(p.id) && (
                      <span title="This cloud holds parts too" style={{
                        fontSize: '9px', color: COLORS.textMuted, border: `1px dashed ${COLORS.borderBright}`,
                        padding: '1px 5px', borderRadius: '3px', flexShrink: 0,
                      }}>holds parts</span>
                    )}
                  </button>
                  {on && (
                    <div style={{
                      display: 'grid',
                      /* The field is the point; the parts folder beside it is a
                         reminder, and a long path must not squeeze the field to
                         nothing. */
                      gridTemplateColumns: mobile ? '1fr' : 'auto minmax(140px, 1fr) minmax(0, 38%)',
                      gap: '8px', alignItems: 'center', margin: '4px 0 2px 24px',
                      padding: '6px 10px', borderLeft: `2px solid ${color}`,
                    }}>
                      <label htmlFor={`replica-folder-${p.id}`} style={{
                        fontFamily: FONT.mono, fontSize: '9px', letterSpacing: '1.2px',
                        textTransform: 'uppercase', color: COLORS.textMuted,
                      }}>Replica folder</label>
                      <input
                        id={`replica-folder-${p.id}`}
                        value={folders[p.id] || ''}
                        placeholder={suggestedReplicaFolder(p)}
                        onChange={(e) => setFolders((prev) => ({ ...prev, [p.id]: e.target.value }))}
                        style={{
                          background: COLORS.surface, border: `1px solid ${COLORS.borderBright}`,
                          borderRadius: '4px', padding: '6px 8px', color: COLORS.text,
                          fontFamily: FONT.mono, fontSize: '11.5px', minWidth: 0, outline: 'none',
                        }}
                      />
                      <span
                        title={`Parts are in ${partsFolderOf(p) || 'the root'}`}
                        style={{
                          fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textMuted,
                          minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
                        }}
                      >
                        parts in <code>{partsFolderOf(p) || 'the root'}</code>
                      </span>
                    </div>
                  )}
                </div>
              )
            })}
          </div>

          {selected.length > 0 && (
            <>
              <div style={label}>How a replica is cut</div>
              {whole ? (
                <>
                  <span style={{
                    display: 'inline-block', fontFamily: FONT.mono, fontSize: '11px', fontWeight: 600,
                    padding: '6px 11px', borderRadius: '6px', border: `1px solid ${COLORS.accent}`,
                    background: 'rgba(217,119,6,0.14)', color: COLORS.accentBright,
                  }}>Whole copy</span>
                  <div style={{ marginTop: '10px' }}>
                    <Banner tone="warn">
                      With one replication cloud the replica is the <strong>entire file</strong>,
                      compressed and encrypted under the vault's data key, on that one cloud
                      alone. The key never leaves this machine, so what the cloud holds is still
                      unreadable without it. But unlike a part, it is enough on its own once the
                      key is known. Add a second cloud and the replica is cut like everything else.
                    </Banner>
                  </div>
                </>
              ) : (
                <>
                  <ThresholdChoice scheme={scheme} onChange={(next) => setThreshold(next.data)} />
                  {!cutOK && (
                    <div style={{ marginTop: '10px' }}>
                      <Banner tone="error">Choose a code as wide as the clouds ticked.</Banner>
                    </div>
                  )}
                </>
              )}

              <div style={label}>Pace</div>
              <div style={{ display: 'flex', alignItems: 'center', gap: '10px', flexWrap: 'wrap' }}>
                <span style={{ fontFamily: FONT.mono, fontSize: '11.5px' }}>At most</span>
                <input
                  id="replication-per-day"
                  value={perDay}
                  onChange={(e) => setPerDay(e.target.value)}
                  placeholder="no limit"
                  inputMode="decimal"
                  style={{
                    width: '90px', background: COLORS.bg, border: `1px solid ${perDayOK ? COLORS.borderBright : COLORS.error}`,
                    borderRadius: '4px', padding: '6px 8px', color: COLORS.text,
                    fontFamily: FONT.mono, fontSize: '11.5px', outline: 'none',
                  }}
                />
                <span style={{ fontFamily: FONT.mono, fontSize: '10.5px', color: COLORS.textMuted }}>
                  GB a day, smallest files first
                </span>
                <label style={{
                  marginLeft: 'auto', display: 'inline-flex', alignItems: 'center', gap: '6px',
                  fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textDim, cursor: 'pointer',
                }}>
                  <input id="replication-paused" type="checkbox" checked={paused} onChange={(e) => setPaused(e.target.checked)} />
                  Paused
                </label>
              </div>
            </>
          )}

          <p style={{ margin: '16px 0', fontFamily: FONT.sans, fontSize: '12px', color: COLORS.textMuted, lineHeight: 1.6 }}>
            Folders are chosen from their own menu in the file list, which is also where a single
            file is. A replica is remade when the file changes, the key changes, or this policy
            does; clearing the policy leaves every replica already made where it is.
          </p>

          <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end', flexWrap: 'wrap' }}>
            {status.policy && (
              <Button type="button" variant="ghost" onClick={() => save([])} disabled={busy}>Clear</Button>
            )}
            <Button type="button" variant="ghost" onClick={onClose} disabled={busy}>Cancel</Button>
            <Button
              type="button"
              variant="primary"
              onClick={() => save(selected)}
              disabled={busy || selected.length === 0 || !cutOK || !foldersOK || !perDayOK}
            >
              {busy ? <Spinner size={10} /> : null}Save
            </Button>
          </div>
        </>
      )}
    </Modal>
  )
}

/* How far the worker has got, at the top of the dialog because a policy
   nobody reads the results of is not doing anything. Nothing is drawn until
   there is a policy. */
function ReplicationProgress({ status, providers }) {
  if (!status.policy) return null
  const total = status.replicated + status.pending
  const fraction = total > 0 ? status.replicated / total : 1
  const name = (id) => providers.find((p) => p.id === id)?.name || id

  return (
    <div style={{
      background: COLORS.bg, border: `1px solid ${COLORS.border}`, borderRadius: '8px',
      padding: '10px 12px', fontFamily: FONT.mono, fontSize: '11px',
    }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', gap: '10px', color: COLORS.textDim }}>
        <span>⧉ {status.pending > 0 ? (status.policy.paused ? 'Paused' : 'Replicating') : 'In step'}</span>
        <span>{status.replicated} of {total} file{total === 1 ? '' : 's'}</span>
      </div>
      <div style={{ height: '6px', margin: '6px 0', background: COLORS.surfaceRaised, borderRadius: '3px', overflow: 'hidden' }}>
        <div style={{ height: '100%', width: `${Math.max(2, fraction * 100)}%`, background: COLORS.accentBright, borderRadius: '3px' }} />
      </div>
      <div style={{ display: 'flex', justifyContent: 'space-between', gap: '10px', color: COLORS.textMuted, flexWrap: 'wrap' }}>
        <span>{formatBytes(status.replica_bytes)} across the replica folders</span>
        {status.pending > 0 && (
          <span>
            {formatBytes(status.pending_bytes)} to go
            {status.eta_seconds > 0 ? ` · about ${durationPhrase(status.eta_seconds)}` : ''}
          </span>
        )}
      </div>
      {status.current && (
        <div style={{ marginTop: '6px', color: COLORS.textDim, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
          Copying {status.current}{status.current_size > 0 ? ` · ${formatBytes(status.current_done)} of ${formatBytes(status.current_size)}` : ''}
        </div>
      )}
      {(status.warnings || []).slice(-3).map((w, i) => (
        <div key={i} style={{ marginTop: '4px', color: COLORS.warn, wordBreak: 'break-word' }}>{w}</div>
      ))}
      <div style={{ marginTop: '6px', color: COLORS.textMuted }}>
        {status.policy.accounts.map(name).join(' · ')}
      </div>
    </div>
  )
}

function durationPhrase(seconds) {
  if (seconds < 3600) return `${Math.max(1, Math.round(seconds / 60))} min`
  if (seconds < 86400) return `${Math.round(seconds / 3600)} h`
  return `${Math.round(seconds / 86400)} day${Math.round(seconds / 86400) === 1 ? '' : 's'}`
}

/* "Replicate this folder": what the copy costs, said before anything moves.
   `held` is what the folder's menu already fetched about it, or null. */
export function ReplicateFolderDialog({ path, name, held, replicated, zIndex, onClose, onChanged }) {
  const [status, setStatus] = useState(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  useEffect(() => {
    let live = true
    api.replicationStatus().then((s) => { if (live) setStatus(s) }).catch((err) => { if (live) setError(err.message) })
    return () => { live = false }
  }, [])

  const policy = status?.policy
  const factor = policy ? policy.total_shards / policy.data_shards : 1
  const bytes = held?.bytes || 0
  const days = policy?.bytes_per_day > 0 ? Math.ceil((bytes * factor) / policy.bytes_per_day) : 0

  const set = async (enabled) => {
    setBusy(true)
    setError(null)
    try {
      await api.setReplicate(path, enabled)
      onChanged()
      onClose()
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }

  return (
    <Modal
      title={replicated ? 'Stop replicating this folder' : 'Replicate this folder'}
      subtitle={held ? `${path} · ${held.files} file${held.files === 1 ? '' : 's'} · ${formatBytes(bytes)}` : path}
      onClose={busy ? undefined : onClose}
      width={480}
      zIndex={zIndex}
    >
      {error && <Banner tone="error">{error}</Banner>}
      {status && !policy && (
        <Banner tone="warn">
          No replication clouds are set. Choose them in Vault settings › Replication first; the
          folder is remembered either way and copies start once they are.
        </Banner>
      )}
      {policy && (
        <div style={{ fontFamily: FONT.sans, fontSize: '12.5px', color: COLORS.textDim, lineHeight: 1.6 }}>
          <div style={label}>Where the copy goes</div>
          <div style={{ fontFamily: FONT.mono, fontSize: '11.5px', color: COLORS.text }}>
            {policy.total_shards === 1 ? 'A whole copy' : schemeName({ data: policy.data_shards, total: policy.total_shards })}
            {' '}across {policy.accounts.length} cloud{policy.accounts.length === 1 ? '' : 's'}, each in its replica folder
          </div>
          {!replicated && (
            <>
              <div style={label}>What it costs</div>
              <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(120px, 1fr))', gap: '8px' }}>
                <Figure value={held ? formatBytes(bytes * factor) : '…'} hint={`written across the clouds, ${factor.toFixed(1)}× of ${formatBytes(bytes)}`} />
                <Figure value={days > 0 ? `${days} day${days === 1 ? '' : 's'}` : 'no limit'} hint={policy.bytes_per_day > 0 ? `at ${formatBytes(policy.bytes_per_day)} a day, smallest files first` : 'the copies start as soon as the vault is open'} />
              </div>
            </>
          )}
          <p style={{ margin: '14px 0 0' }}>
            {replicated
              ? 'Files already copied keep their replica until it is dropped from their own menu; nothing is erased by stopping.'
              : 'Files added to this folder later replicate on their own. A file can opt out, or in, from its own menu.'}
          </p>
        </div>
      )}
      <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end', flexWrap: 'wrap', marginTop: '18px' }}>
        <Button type="button" variant="ghost" onClick={onClose} disabled={busy}>Cancel</Button>
        <Button type="button" variant="primary" onClick={() => set(!replicated)} disabled={busy || !status}>
          {busy ? <Spinner size={10} /> : null}{replicated ? 'Stop replicating' : '⧉ Start replicating'}
        </Button>
      </div>
    </Modal>
  )
}

function Figure({ value, hint }) {
  return (
    <div style={{ background: COLORS.bg, border: `1px solid ${COLORS.border}`, borderRadius: '6px', padding: '8px 10px' }}>
      <div style={{ fontFamily: FONT.mono, fontSize: '15px', fontWeight: 600, color: COLORS.text, letterSpacing: '-0.5px' }}>{value}</div>
      <div style={{ fontFamily: FONT.sans, fontSize: '10px', color: COLORS.textMuted, lineHeight: 1.35, marginTop: '2px' }}>{hint}</div>
    </div>
  )
}

/* Whether a file's replica is whole: every shard of its code recorded. */
export function replicaWhole(file) {
  const r = file?.replica
  if (!r) return false
  return new Set(r.shards.map((s) => s.part)).size >= r.total_shards
}

/* The replica's shards, drawn beside the parts: one outlined square per
   shard in its cloud's colour, or one wide "whole" badge for a single-cloud
   copy. Outlined rather than filled because that is the difference — a part
   is the placement the file depends on, a replica is the one standing by. */
export function ReplicaBadges({ file }) {
  const r = file.replica
  if (!r) return null
  const whole = r.total_shards === 1
  const tight = r.total_shards > 3
  const shard = (part) => r.shards.find((s) => s.part === part)

  return (
    <>
      <span aria-hidden="true" style={{ width: '1px', height: '12px', background: COLORS.borderBright, margin: '0 2px', flexShrink: 0 }} />
      {whole ? (
        <span
          title={`A whole copy on ${shard(1)?.provider_name || 'a replication cloud'}`}
          style={{
            height: '15px', padding: '0 5px', borderRadius: '3px', fontFamily: FONT.mono,
            fontSize: '7.5px', fontWeight: 700, letterSpacing: '0.6px', display: 'flex',
            alignItems: 'center', flexShrink: 0, boxSizing: 'border-box',
            color: shard(1) ? accountColor(shard(1).provider_id) : COLORS.textMuted,
            border: `1.5px solid ${shard(1) ? accountColor(shard(1).provider_id) : COLORS.border}`,
          }}
        >WHOLE</span>
      ) : Array.from({ length: r.total_shards }, (_, i) => i + 1).map((part) => {
        const s = shard(part)
        return (
          <span
            key={part}
            title={s ? `Replica shard ${part} on ${s.provider_name}` : `Replica shard ${part} not stored`}
            style={{
              width: tight ? '12px' : '19px', height: '15px', borderRadius: '3px',
              fontFamily: FONT.mono, fontSize: tight ? '7.5px' : '8.5px', fontWeight: 700,
              display: 'flex', alignItems: 'center', justifyContent: 'center', flexShrink: 0,
              boxSizing: 'border-box',
              color: s ? accountColor(s.provider_id) : COLORS.textMuted,
              background: 'transparent',
              border: s ? `1.5px solid ${accountColor(s.provider_id)}` : `1px dashed ${COLORS.border}`,
            }}
          >{part}</span>
        )
      })}
    </>
  )
}

/* The phone's version: hollow dots after the parts' filled ones. */
export function ReplicaDots({ file }) {
  const r = file.replica
  if (!r) return null
  const whole = r.total_shards === 1
  return (
    <>
      <span aria-hidden="true" style={{ width: '1px', height: '10px', background: COLORS.borderBright, margin: '0 2px' }} />
      {whole ? (
        <span style={{
          fontFamily: FONT.mono, fontSize: '7.5px', fontWeight: 700, letterSpacing: '0.6px',
          padding: '0 4px', borderRadius: '3px', boxSizing: 'border-box',
          color: accountColor(r.shards[0]?.provider_id), border: `1.5px solid ${accountColor(r.shards[0]?.provider_id)}`,
        }}>WHOLE</span>
      ) : (
        <span style={{ display: 'flex', alignItems: 'center', gap: '2px' }}>
          {Array.from({ length: r.total_shards }, (_, i) => i + 1).map((part) => {
            const s = r.shards.find((x) => x.part === part)
            return (
              <span key={part} style={{
                width: '6px', height: '6px', borderRadius: '50%', flexShrink: 0, boxSizing: 'border-box',
                border: s ? `1.5px solid ${accountColor(s.provider_id)}` : `1px dashed ${COLORS.borderBright}`,
              }} />
            )
          })}
        </span>
      )}
    </>
  )
}

/* Which placement a read is served from. Three positions: the parts, which is
   every read until now; the replica alone; or either — the parts, with the
   replica standing in for any chunk they cannot give. Only drawn for a file
   that has a replica: without one there is nothing to choose. */
export const READ_SOURCES = [
  { id: 'parts', label: 'Parts', hint: 'The parts, as every read is' },
  { id: 'replica', label: 'Replica', hint: 'The replica alone' },
  { id: 'either', label: 'Either', hint: 'The parts, with the replica standing in for anything they cannot give' },
]

export function ReadFromChoice({ file, value, onChange, compact = false }) {
  if (!file?.replica) return null
  const dots = (shards, ring) => (
    <span style={{ display: 'inline-flex', gap: '2px', marginLeft: '5px', verticalAlign: 'middle' }}>
      {shards.slice(0, 6).map((s, i) => (
        <span key={i} style={{
          width: '6px', height: '6px', borderRadius: '50%', boxSizing: 'border-box',
          background: ring ? 'transparent' : accountColor(s.provider_id),
          border: ring ? `1.5px solid ${accountColor(s.provider_id)}` : 'none',
        }} />
      ))}
    </span>
  )

  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: '10px', flexWrap: 'wrap' }}>
      <span style={{ ...label, margin: 0 }}>Read from</span>
      <span role="radiogroup" aria-label="Read from" style={{
        display: compact ? 'flex' : 'inline-flex', flex: compact ? 1 : undefined,
        border: `1px solid ${COLORS.border}`, borderRadius: '6px', overflow: 'hidden', background: COLORS.bg,
      }}>
        {READ_SOURCES.map((s, i) => {
          const on = (value || 'parts') === s.id
          return (
            <button
              key={s.id}
              type="button"
              role="radio"
              aria-checked={on}
              title={s.hint}
              onClick={() => onChange(s.id)}
              style={{
                flex: compact ? 1 : undefined, padding: '6px 11px', border: 'none',
                borderRight: i < READ_SOURCES.length - 1 ? `1px solid ${COLORS.border}` : 'none',
                background: on ? 'rgba(217,119,6,0.16)' : 'transparent',
                color: on ? COLORS.accentBright : COLORS.textDim, cursor: 'pointer',
                fontFamily: FONT.mono, fontSize: '10.5px', fontWeight: 600, letterSpacing: '0.5px',
              }}
            >
              {s.label}
              {!compact && s.id === 'parts' && dots(file.shards, false)}
              {!compact && s.id === 'replica' && dots(file.replica.shards, true)}
            </button>
          )
        })}
      </span>
    </div>
  )
}
