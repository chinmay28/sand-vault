import React, { useEffect, useState } from 'react'
import { COLORS, FONT } from '../theme'
import { api } from '../api'
import { Banner, Button, Modal, Spinner } from './ui'
import { ago } from './CloudHealth'

/* The machines this vault is open on.

   A vault file is small and a cloud answers from anywhere, so the same vault
   can run on a desk and on a Raspberry Pi at once: same password, same clouds,
   same tree. Each machine keeps its own vault file, and the copies of the
   index every cloud already holds for recovery are how a change on one reaches
   the other — pulled at every unlock, every few minutes once a second machine
   has been seen, and before every push. This dialog lists the machines, says
   when each last wrote, and pulls now for somebody who has just uploaded on
   the other one.

   A folder on a machine's own disk is that machine's alone. Files whose parts
   are on it are greyed out everywhere else and say which machine can read
   them; that is what the account count beside each machine is about. */
export default function Instances({ status, zIndex, onClose, onChanged }) {
  const [list, setList] = useState(null)
  const [error, setError] = useState(null)
  const [pulling, setPulling] = useState(false)
  const [report, setReport] = useState(null)

  const load = () => api.instances()
    .then((resp) => { setList(resp); setError(null) })
    .catch((err) => setError(err.message))
  useEffect(() => { load() }, [])

  const pull = async () => {
    setPulling(true)
    setError(null)
    try {
      const resp = await api.syncIndex()
      setReport(resp)
      setList({ instance: resp.instance, instances: resp.instances, last_sync: resp.synced_at })
      if (resp.outcome === 'adopted' || resp.outcome === 'merged') onChanged?.()
    } catch (err) {
      setError(err.message)
    } finally {
      setPulling(false)
    }
  }

  const instances = list?.instances || []
  const others = instances.filter((i) => !i.here)

  return (
    <Modal
      title="Machines"
      subtitle="Where this vault is open, and what each machine can reach"
      onClose={onClose}
      width={520}
      zIndex={zIndex}
    >
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

      {status?.manifest_backup === false && (
        <Banner tone="warn">
          The index backup is off, so there is no copy of the index on any cloud for another
          machine to read or write. Switch it on under Index backup to open this vault from more
          than one machine.
        </Banner>
      )}

      {report && (
        <Banner tone={report.outcome === 'current' || report.outcome === 'ahead' ? 'info' : 'success'}>
          {pullSentence(report)}
        </Banner>
      )}

      {!list && !error && (
        <div style={{ display: 'flex', alignItems: 'center', gap: '8px', padding: '8px 0' }}>
          <Spinner size={12} />
          <span style={{ fontFamily: FONT.sans, fontSize: '12px', color: COLORS.textMuted }}>Reading the register…</span>
        </div>
      )}

      {instances.map((inst) => <MachineRow key={inst.name} inst={inst} />)}

      {list && others.length === 0 && (
        <p style={{
          margin: '10px 0 0', fontFamily: FONT.sans, fontSize: '11.5px',
          lineHeight: 1.6, color: COLORS.textMuted,
        }}>
          Only this machine has opened the vault. To open it on another: export a recovery kit
          under Recovery kit, import it there and tick <em>the vault is still running on another
          machine</em> — or copy the vault file across. The other machine appears here after its
          first pull. A folder on this machine's disk stays this machine's: files with parts on it
          are greyed out there and say to read them here.
        </p>
      )}

      <div style={{
        display: 'flex', alignItems: 'center', gap: '8px', justifyContent: 'space-between',
        marginTop: '14px',
      }}>
        <span style={{ fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textMuted }}>
          {list?.last_sync ? `Last pulled ${ago(list.last_sync)}` : 'Not pulled since unlocking'}
        </span>
        <div style={{ display: 'flex', gap: '8px' }}>
          <Button variant="ghost" onClick={onClose}>Close</Button>
          <Button
            variant="primary"
            disabled={pulling || status?.manifest_backup === false}
            onClick={pull}
            title="Read the copies of the index on the clouds now and fold in what the other machines wrote"
          >
            {pulling ? <Spinner size={13} color={COLORS.bg} /> : null}
            {pulling ? 'Pulling…' : 'Pull now'}
          </Button>
        </div>
      </div>
    </Modal>
  )
}

/* What a pull came to, in one sentence. */
function pullSentence(report) {
  const counts = []
  if (report.added) counts.push(`${report.added} file${report.added === 1 ? '' : 's'} added`)
  if (report.removed) counts.push(`${report.removed} removed`)
  if (report.updated) counts.push(`${report.updated} changed`)
  const renamed = report.renamed?.length
    ? ` ${report.renamed.length} file${report.renamed.length === 1 ? ' was' : 's were'} renamed because both machines stored a file at the same path.`
    : ''
  switch (report.outcome) {
    case 'adopted':
      return `Took the newer index from the clouds: ${counts.join(', ') || 'nothing in this folder changed'}.${renamed}`
    case 'merged':
      return `Combined this machine's index with the other machine's: ${counts.join(', ') || 'no rows differed'}.${renamed}`
    case 'ahead':
      return 'This machine is ahead of the copies on the clouds; the next push brings them up to date.'
    default:
      if (report.asked === 0) {
        return 'Every account this vault knows of lives on another machine, so there is nothing '
          + 'here to pull from. Connect a cloud on this machine too, and the two share the index through it.'
      }
      return report.answered === 0
        ? `No copy of the index could be read from the ${report.asked} account${report.asked === 1 ? '' : 's'} asked.`
        : 'Nothing new on the clouds — the index here is current.'
  }
}

function MachineRow({ inst }) {
  const accounts = inst.accounts?.length || 0
  return (
    <div style={{
      display: 'flex', alignItems: 'flex-start', gap: '10px',
      padding: '9px 11px', marginBottom: '8px',
      background: COLORS.bg, border: `1px solid ${COLORS.border}`,
      borderLeft: `3px solid ${inst.here ? COLORS.accent : COLORS.borderBright}`,
      borderRadius: '6px',
    }}>
      <span aria-hidden="true" style={{ fontSize: '14px', lineHeight: 1.3 }}>🖥️</span>
      <div style={{ flex: 1, minWidth: 0 }}>
        <div style={{
          fontFamily: FONT.mono, fontSize: '12px', color: COLORS.text,
          overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
        }}>
          {inst.name}
          {inst.here && <span style={{ color: COLORS.textMuted }}> — this machine</span>}
        </div>
        <div style={{ marginTop: '3px', fontFamily: FONT.mono, fontSize: '9.5px', color: COLORS.textMuted, lineHeight: 1.5 }}>
          {`reaches ${accounts} account${accounts === 1 ? '' : 's'}`}
          {inst.last_seen && !inst.last_seen.startsWith('0001') ? ` · last wrote ${ago(inst.last_seen)}` : ''}
        </div>
      </div>
    </div>
  )
}
