import React, { useEffect, useState } from 'react'
import { COLORS, formatBytes } from '../theme'
import { api } from '../api'
import { Modal } from './ui'
import { Setting } from './VaultSettings'
import ReadStats from './ReadStats'
import TransferStats from './TransferStats'
import ThroughputStats, { formatMbps } from './ThroughputStats'
import StorageBreakdown, { storageTotals } from './StorageBreakdown'

/* What the clouds have been doing with the vault, in one place.

   This was a single tile, Read speed — now Read race — beside Vault settings.
   That is one question about the accounts — who answers a read first — and there are two more
   that somebody standing in front of their accounts asks just as often: how
   much has moved to and from each of them, and how much room is left on them
   all together. Three tiles would crowd the drawer the way the settings tiles
   once did — and a fourth, how fast all of it is — so it is one door and a
   list behind it, in the same shape as
   Vault settings: each line says what it is and the one figure worth knowing
   without opening it, and each opens over this list so closing it comes back
   here.

   Nothing on this list changes anything. */
export default function Statistics({ providers = [], stats, onClose }) {
  const [open, setOpen] = useState(null)
  const [reads, setReads] = useState(null)
  const [transfers, setTransfers] = useState(null)
  const [throughput, setThroughput] = useState(null)

  /* Today's figures, for the two status lines. Loopback reads of counters in
     memory, asked once when the list opens. */
  useEffect(() => {
    let cancelled = false
    api.readStats('today').then((r) => { if (!cancelled) setReads(r.reads) }).catch(() => {})
    api.transferStats('today').then((r) => { if (!cancelled) setTransfers(r.transfers) }).catch(() => {})
    api.throughput('today').then((r) => { if (!cancelled) setThroughput(r.throughput) }).catch(() => {})
    return () => { cancelled = true }
  }, [])

  const storage = storageTotals(providers)
  const close = () => setOpen(null)

  return (
    <Modal
      title="Statistics"
      subtitle="What the clouds have been doing with this vault"
      onClose={onClose}
      width={520}
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: '8px' }}>
        <Setting
          icon="⏱️"
          label="Read race"
          hint="Which cloud answers first when a file is rebuilt, and in how many ms"
          status={readStatus(reads)}
          tone={reads?.shortfalls > 0 ? COLORS.error : undefined}
          onClick={() => setOpen('reads')}
        />
        <Setting
          icon="⇅"
          label="Transfers"
          hint="How much has been sent to each cloud and fetched back off it"
          status={transferStatus(transfers)}
          onClick={() => setOpen('transfers')}
        />
        <Setting
          icon="⚡"
          label="Throughput"
          hint="Mbps for the vault, each kind of file and each cloud"
          status={throughputStatus(throughput)}
          onClick={() => setOpen('throughput')}
        />
        <Setting
          icon="🗄️"
          label="Storage breakdown"
          hint="Capacity, free space and what is using it, across every cloud"
          status={storage.capacity
            ? `${formatBytes(storage.free)} free`
            : `${formatBytes(storage.stored)} stored`}
          onClick={() => setOpen('storage')}
        />
      </div>

      {open === 'reads' && <ReadStats onClose={close} />}
      {open === 'transfers' && <TransferStats onClose={close} />}
      {open === 'throughput' && <ThroughputStats onClose={close} />}
      {open === 'storage' && <StorageBreakdown providers={providers} stats={stats} onClose={close} />}
    </Modal>
  )
}

function readStatus(reads) {
  if (!reads) return ''
  if (!reads.races) return 'none today'
  return `${reads.races.toLocaleString()} race${reads.races === 1 ? '' : 's'} today`
}

function transferStatus(transfers) {
  if (!transfers) return ''
  if (!transfers.bytes_in && !transfers.bytes_out) return 'none today'
  return `↑ ${formatBytes(transfers.bytes_in)} ↓ ${formatBytes(transfers.bytes_out)}`
}

function throughputStatus(t) {
  if (!t) return ''
  if (!t.open?.mbps && !t.store?.mbps) return 'none today'
  if (!t.open?.mbps) return `↑ ${formatMbps(t.store.mbps)}`
  return `↓ ${formatMbps(t.open.mbps)}`
}
