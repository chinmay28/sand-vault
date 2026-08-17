import React, { useEffect, useRef, useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { api } from '../api'
import { downloadFile } from '../download'
import { Banner, Button, Modal, Spinner } from './ui'

/* What a handful of picked rows can be told to do at once.

   All of it is a loop over things the vault already does one at a time, run
   from the browser rather than added to the API. That is deliberate: erasing
   twelve files is twelve deletions whichever end of the wire the loop is on,
   and keeping it here means a stall on the fourth one is visible, the three
   that already went are still gone, and nothing needs a new endpoint that
   could half-succeed with no way to say which half.

   Hence one file at a time rather than all at once: a dozen parallel deletions
   against the same account is a way to be rate-limited, and a dozen parallel
   rebuilds is a way to run a Raspberry Pi out of memory. */

/* A run of one-at-a-time operations, with somewhere to say how far it has got.
   Left in whatever state it finished in — a partial failure is a normal
   outcome here and worth reading, not something to close over.

   What it runs over is taken once, when the dialog opens, and never read
   again. A finished run clears the selection it came from, and a dialog still
   reading that selection would answer "0 deleted" the instant it had deleted
   twelve things — the batch is what was picked, not what is picked now. */
export function useRun(chosen, perform, onFinished) {
  const [items] = useState(() => chosen)
  const [at, setAt] = useState(-1)
  const [done, setDone] = useState(null)
  /* A dialog closed halfway through must not leave a loop writing into a
     component that is no longer on screen. Set on the way in as well as
     cleared on the way out: React's strict mode mounts, unmounts and mounts
     again, and a flag only ever cleared would stay cleared for the mount that
     actually runs. */
  const live = useRef(true)
  useEffect(() => {
    live.current = true
    return () => { live.current = false }
  }, [])

  const running = at >= 0 && !done

  const start = async () => {
    const failures = []
    const warnings = []

    for (let i = 0; i < items.length; i++) {
      if (!live.current) return
      setAt(i)
      try {
        const notes = await perform(items[i])
        if (notes?.length) warnings.push(...notes.map((w) => `${items[i].name}: ${w}`))
      } catch (err) {
        failures.push(`${items[i].name}: ${err.message}`)
      }
    }

    if (!live.current) return
    setDone({ failures, warnings })
    onFinished?.()
  }

  return { items, at, running, done, start }
}

export function Progress({ items, at, verb }) {
  const item = items[at]

  return (
    <div style={{ marginBottom: '16px' }}>
      <div style={{
        display: 'flex', alignItems: 'center', gap: '8px', marginBottom: '8px',
        fontFamily: FONT.mono, fontSize: '11.5px', color: COLORS.textDim,
      }}>
        <Spinner size={11} />
        <span>{verb} {at + 1} of {items.length}</span>
        <span style={{
          flex: 1, minWidth: 0, color: COLORS.textMuted,
          overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
        }}>{item?.name}</span>
      </div>
      <div style={{ height: '3px', background: COLORS.border, borderRadius: '2px', overflow: 'hidden' }}>
        <div style={{
          height: '100%',
          width: `${Math.max(4, ((at + 1) / items.length) * 100)}%`,
          background: COLORS.accent,
          transition: 'width 0.2s ease',
        }} />
      </div>
    </div>
  )
}

function Outcome({ done, total, verb }) {
  const failed = done.failures.length

  return (
    <>
      <Banner tone={failed ? 'warn' : 'success'}>
        {failed
          ? `${total - failed} of ${total} ${verb}. The rest are untouched — try them again once the accounts are answering.`
          : `${total} ${verb}.`}
      </Banner>
      {(done.failures.length > 0 || done.warnings.length > 0) && (
        <div style={{ maxHeight: '180px', overflowY: 'auto', marginBottom: '4px' }}>
          {done.failures.length > 0 && (
            <Banner tone="error">{done.failures.map((f, i) => <div key={i}>{f}</div>)}</Banner>
          )}
          {done.warnings.length > 0 && (
            <Banner tone="warn">{done.warnings.map((w, i) => <div key={i}>{w}</div>)}</Banner>
          )}
        </div>
      )}
    </>
  )
}

/* Erasing everything picked. A folder takes what is inside it, which is why
   the dialog counts the two kinds separately rather than saying "12 items" and
   leaving the folders to be discovered afterwards. */
export function BulkDelete({ items, onClose, onDone }) {
  const run = useRun(items, async (item) => {
    const resp = item.kind === 'folder'
      ? await api.deleteFolder(item.path, true)
      : await api.deleteFile(item.file.id)
    return resp?.warnings
  }, onDone)

  const batch = run.items
  const folders = batch.filter((i) => i.kind === 'folder')
  const files = batch.filter((i) => i.kind !== 'folder')
  const bytes = files.reduce((sum, f) => sum + (f.file.size || 0), 0)

  const close = () => { if (!run.running) onClose() }

  return (
    <Modal
      title={`Delete ${batch.length} item${batch.length === 1 ? '' : 's'}?`}
      subtitle={describe(folders.length, files.length, bytes)}
      onClose={run.running ? undefined : close}
      width={460}
    >
      {run.done ? (
        <>
          <Outcome done={run.done} total={batch.length} verb="deleted" />
          <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
            <Button variant="primary" onClick={close}>Done</Button>
          </div>
        </>
      ) : run.running ? (
        <Progress items={batch} at={run.at} verb="Deleting" />
      ) : (
        <>
          <div style={{
            marginBottom: '18px', fontFamily: FONT.sans, fontSize: '12.5px',
            lineHeight: 1.6, color: COLORS.textDim,
          }}>
            Every part of every file goes, erased from each account holding it.
            {folders.length > 0 && ' A folder takes everything inside it, however deep.'}
            {' '}This cannot be undone.
          </div>
          <div style={{ display: 'flex', gap: '10px', justifyContent: 'flex-end', flexWrap: 'wrap' }}>
            <Button variant="danger" onClick={run.start}>Delete {batch.length}</Button>
            <Button variant="ghost" onClick={close}>Cancel</Button>
          </div>
        </>
      )}
    </Modal>
  )
}

function describe(folders, files, bytes) {
  const parts = []
  if (folders) parts.push(`${folders} folder${folders === 1 ? '' : 's'}`)
  if (files) parts.push(`${files} file${files === 1 ? '' : 's'}, ${formatBytes(bytes)}`)
  return parts.join(' and ')
}

/* Handing several files back at once.

   Each one is gathered from its accounts and rebuilt before it can be saved,
   so this is minutes rather than seconds on anything large — and it starts the
   moment the dialog opens, because picking the files and pressing Download was
   already the decision. Nothing navigates: every file is saved as a blob under
   its own name, the way a single download is, so a vault opened from a phone's
   home screen stays where it is. */
export function BulkDownload({ items, onClose }) {
  // Folders are not rebuilt as one thing, so they are simply not part of this.
  const run = useRun(items.filter((i) => i.kind !== 'folder'),
    async (item) => { await downloadFile(item.file); return null })
  const files = run.items

  // Started here rather than behind a button: the dialog only opens because
  // Download was pressed, and asking twice for the same thing is a step.
  const started = useRef(false)
  useEffect(() => {
    if (started.current) return
    started.current = true
    run.start()
    // The run is stable for the life of the dialog; re-running it on a render
    // would download everything twice.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const bytes = files.reduce((sum, f) => sum + (f.file.size || 0), 0)

  return (
    <Modal
      title={`Download ${files.length} file${files.length === 1 ? '' : 's'}`}
      subtitle={`${formatBytes(bytes)} rebuilt one at a time — the parts of each are gathered from your accounts first`}
      onClose={run.done ? onClose : undefined}
      width={460}
    >
      {run.done ? (
        <>
          <Outcome done={run.done} total={files.length} verb="downloaded" />
          <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
            <Button variant="primary" onClick={onClose}>Done</Button>
          </div>
        </>
      ) : (
        <>
          <Progress items={files} at={Math.max(0, run.at)} verb="Rebuilding" />
          <p style={{
            margin: 0, fontFamily: FONT.sans, fontSize: '11.5px',
            color: COLORS.textMuted, lineHeight: 1.6,
          }}>
            Your browser may ask whether to allow several downloads at once. Leaving this dialog
            open is what keeps them coming.
          </p>
        </>
      )}
    </Modal>
  )
}
