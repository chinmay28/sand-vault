import React, { useEffect, useMemo, useRef, useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { api, joinPath, parentPath } from '../api'
import { useIsMobile } from '../hooks'
import { Banner, Button, Modal, Spinner } from './ui'
import { Progress, useRun } from './BulkActions'

/* Moving files and folders around inside the vault.

   The other Move — RelocateClouds — changes which accounts hold the parts of a
   file, and copies bytes between clouds to do it. This one changes nothing at
   all about where the parts are. A file records the folder it is in, and its
   parts are named after the file rather than after the folder, so moving it
   across the vault is a rewrite of one field in the encrypted index: no account
   is contacted, nothing is decrypted, and a 4 GB film moves as fast as a note.

   Both are called "move" because both are, and the two are told apart by what
   they are moving something onto — a folder, or a set of clouds — rather than
   by inventing a word for one of them. */

/* What one picked thing would be moved to, and whether it can be.

   Every refusal is worked out here rather than left to the server, because a
   selection is moved one item at a time: finding out on the fourth of twelve
   that the destination was inside the folder being moved is finding out too
   late. The server refuses the same things — it is the only thing that can,
   once two browsers are open on one vault — but nothing bound to be refused is
   sent, and the dialog can say so before the button is pressed. */
function plan(item, dest) {
  if (item.kind === 'folder') {
    if (item.path === dest || dest.startsWith(`${item.path}/`)) {
      return { skip: `${item.name} cannot be moved inside itself` }
    }
    if (parentPath(item.path) === dest) return { skip: `${item.name} is already here` }
    return { to: joinPath(dest, item.name) }
  }
  if (item.file.dir === dest) return { skip: `${item.name} is already here` }
  return { to: joinPath(dest, item.name) }
}

/* Where the picker opens.

   On the folder the things being moved are in, rather than at the root: moving
   something usually means moving it next door, and starting anywhere else means
   walking back down to where you already were. A selection spanning two folders
   — which only a search result can be — has no such folder, so it opens at the
   root and says nothing about it. */
function origin(items) {
  const dirs = new Set(items.map((item) => (
    item.kind === 'folder' ? parentPath(item.path) : item.file.dir)))
  return dirs.size === 1 ? [...dirs][0] : '/'
}

/* `items` are the rows a listing hands around — { kind, name, path } for a
   folder, { kind, name, file } for a file — so one picked row and thirty are
   the same dialog. `vault` is which of the vaults inside the file they live
   in: a move never crosses that boundary, so the tree offered, the folder
   made from here and the folder moves all have to be that vault's — the main
   one has folders of its own, sometimes under the same names. */
export default function MoveToFolder({ items, vault = '', onClose, onDone }) {
  const mobile = useIsMobile()

  const [folders, setFolders] = useState(null)
  const [dest, setDest] = useState(() => origin(items))
  const [error, setError] = useState(null)
  const [creating, setCreating] = useState(false)
  /* The moves, frozen at the moment Move was pressed. The picker's answer
     changes with every click on a folder, so the run has to be handed a list
     that has stopped changing — and it is a separate component for exactly
     that reason: it takes its batch when it mounts. */
  const [started, setStarted] = useState(null)
  // A half-finished run must not be dismissed out from under itself, the way
  // a bulk delete cannot be.
  const [busy, setBusy] = useState(false)
  const [finished, setFinished] = useState(false)

  /* The whole tree in one request, so walking it costs nothing and the folder
     being looked for is a click away rather than a round-trip. Asked for once:
     the only thing that adds to it while the dialog is open is the new-folder
     field below, which knows what it created. */
  useEffect(() => {
    let live = true
    api.folders(vault)
      .then((resp) => { if (live) setFolders(resp.folders || ['/']) })
      .catch((err) => { if (live) setError(err.message) })
    return () => { live = false }
  }, [vault])

  /* What each picked thing would become, in the order it was picked. Worked out
     again on every change of destination, which is what keeps the count on the
     button and the reasons under it about the folder actually being looked at. */
  const plans = useMemo(() => items.map((item) => ({ ...item, ...plan(item, dest) })), [items, dest])
  const movable = plans.filter((p) => !p.skip)
  const skipped = plans.filter((p) => p.skip)

  /* The listing behind this is asked to refresh on the way out rather than the
     moment the run finishes, which is what RelocateClouds does and for the same
     reason: a refresh drops the row this dialog was opened from — the file is
     not in that folder any more — and taking the dialog with it would snatch
     away the report of what happened. */
  const close = () => {
    if (busy) return
    if (finished) onDone?.()
    onClose()
  }

  const files = items.filter((i) => i.kind !== 'folder')
  const bytes = files.reduce((sum, f) => sum + (f.file.size || 0), 0)
  const subtitle = items.length === 1 ? items[0].name : [
    items.length - files.length > 0 && `${items.length - files.length} folder(s)`,
    files.length > 0 && `${files.length} file(s), ${formatBytes(bytes)}`,
  ].filter(Boolean).join(' and ')

  return (
    <Modal
      title={items.length === 1 ? 'Move to another folder' : `Move ${items.length} items`}
      subtitle={subtitle}
      onClose={busy ? undefined : close}
      width={480}
    >
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

      {started ? (
        <MoveRun
          batch={started.batch}
          dest={started.dest}
          vault={vault}
          onBusy={setBusy}
          onFinished={() => setFinished(true)}
          onClose={close}
        />
      ) : (
        <>
          <p style={{
            margin: '0 0 12px',
            fontFamily: FONT.sans, fontSize: '12px', color: COLORS.textMuted, lineHeight: 1.6,
          }}>
            Nothing is uploaded or downloaded. Which folder something is in is a field in the
            encrypted index, so this rewrites that field and leaves every part exactly where it
            is — on the same clouds, under the same key.
          </p>

          <Crumbs path={dest} onNavigate={setDest} />

          <FolderList
            folders={folders}
            dest={dest}
            items={items}
            mobile={mobile}
            onNavigate={setDest}
          />

          {creating ? (
            <NewFolder
              dest={dest}
              vault={vault}
              onCancel={() => setCreating(false)}
              onCreated={(path) => {
                setFolders((current) => [...(current || []), path].sort())
                setDest(path)
                setCreating(false)
              }}
            />
          ) : (
            <div style={{ marginBottom: '12px' }}>
              <Button size="sm" variant="ghost" onClick={() => setCreating(true)}>
                + New folder here
              </Button>
            </div>
          )}

          <Destination dest={dest} movable={movable.length} skipped={skipped} />

          <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end', marginTop: '16px' }}>
            <Button type="button" variant="ghost" onClick={close}>Cancel</Button>
            <Button
              type="button"
              variant="primary"
              onClick={() => setStarted({ batch: movable, dest })}
              disabled={movable.length === 0}
              title={movable.length === 0
                ? 'Nothing picked would go anywhere by moving it here'
                : `Move into ${dest}`}
            >
              {/* The count only when it is not the whole selection — "move 3
                  here" out of three picked is a number nobody asked for, and
                  with nothing to move the button is disabled anyway. */}
              {movable.length > 0 && movable.length < items.length
                ? `→ Move ${movable.length} here`
                : '→ Move here'}
            </Button>
          </div>
        </>
      )}
    </Modal>
  )
}

/* Carrying out the moves, one at a time and in the order they were picked.

   One at a time because each is its own write of the index, and because a
   failure partway through should leave a list that reads as what happened:
   everything before it moved, everything after it still where it was. Started
   the moment this appears — pressing Move was the decision, and asking twice
   for the same thing is a step. */
function MoveRun({ batch, dest, vault, onBusy, onFinished, onClose }) {
  const run = useRun(batch, async (item) => {
    if (item.kind === 'folder') await api.moveFolder(item.path, item.to, vault)
    // An empty name keeps the one it has; only the folder is changing. The
    // file is named by ID, which the server resolves to whichever vault holds
    // it, so no vault travels with it.
    else await api.moveFile(item.file.id, dest, '')
    return null
  }, onFinished)

  const started = useRef(false)
  useEffect(() => {
    if (started.current) return
    started.current = true
    run.start()
    // The run is fixed for the life of this component; starting it again on a
    // render would move everything twice.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // What the dialog around this reads to decide whether it can be dismissed.
  useEffect(() => {
    onBusy(!run.done)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [run.done])

  if (!run.done) return <Progress items={batch} at={Math.max(0, run.at)} verb="Moving" />

  const failed = run.done.failures.length

  return (
    <>
      <Banner tone={failed ? 'warn' : 'success'}>
        {failed
          ? `${batch.length - failed} of ${batch.length} moved to ${dest}. The rest are untouched,
             exactly where they were.`
          : `${batch.length} moved to ${dest}.`}
      </Banner>
      {failed > 0 && (
        <div style={{ maxHeight: '180px', overflowY: 'auto', marginBottom: '4px' }}>
          <Banner tone="error">{run.done.failures.map((f, i) => <div key={i}>{f}</div>)}</Banner>
        </div>
      )}
      <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
        <Button variant="primary" onClick={onClose}>Done</Button>
      </div>
    </>
  )
}

/* The destination as a row of steps, each one a way back up it. The root is a
   step of its own, so the top of the vault is always one click away. */
function Crumbs({ path, onNavigate }) {
  const steps = [{ label: '/', path: '/' }]
  let built = ''
  for (const segment of path.split('/').filter(Boolean)) {
    built += `/${segment}`
    steps.push({ label: segment, path: built })
  }

  return (
    <div style={{
      display: 'flex', alignItems: 'center', gap: '2px',
      marginBottom: '10px', padding: '2px 0',
      overflowX: 'auto', whiteSpace: 'nowrap',
    }}>
      {steps.map((step, i) => (
        <React.Fragment key={step.path}>
          {i > 1 && (
            <span style={{ color: COLORS.textMuted, fontFamily: FONT.mono, fontSize: '11px' }}>/</span>
          )}
          <button
            type="button"
            onClick={() => onNavigate(step.path)}
            style={{
              flexShrink: 0,
              padding: '4px 6px',
              background: 'none', border: 'none', borderRadius: '4px',
              color: i === steps.length - 1 ? COLORS.text : COLORS.textDim,
              fontFamily: FONT.mono, fontSize: '11.5px',
              cursor: 'pointer',
            }}
          >{step.label}</button>
        </React.Fragment>
      ))}
    </div>
  )
}

/* The folders inside the one being looked at, drawn from the flat list of every
   folder in the vault. A folder being moved is still shown — leaving it out
   would make this tree disagree with the browser behind the dialog — but it
   cannot be walked into, because nothing can be moved inside itself. */
function FolderList({ folders, dest, items, mobile, onNavigate }) {
  const moving = useMemo(
    () => new Set(items.filter((i) => i.kind === 'folder').map((i) => i.path)), [items])

  const children = (folders || []).filter((path) => path !== '/' && parentPath(path) === dest)
  const up = parentPath(dest)

  return (
    <div style={{
      border: `1px solid ${COLORS.border}`,
      borderRadius: '8px',
      background: COLORS.bg,
      // Tall enough to walk a tree in, short enough to leave the destination
      // and its button on screen beside a phone keyboard.
      height: mobile ? '32vh' : '200px',
      overflowY: 'auto',
      marginBottom: '12px',
    }}>
      {folders === null ? (
        <div style={{ display: 'flex', justifyContent: 'center', padding: '28px 0' }}><Spinner /></div>
      ) : (
        <>
          {dest !== '/' && (
            <FolderChoice
              glyph="↰"
              name={up === '/' ? '/' : up.split('/').pop()}
              label={`Up to ${up}`}
              hint="Up"
              onSelect={() => onNavigate(up)}
            />
          )}

          {children.map((path) => (
            <FolderChoice
              key={path}
              glyph="📁"
              name={path.split('/').pop()}
              /* Named by path rather than by name: two folders called "2024"
                 are one keystroke apart in a tree, and the spoken name of a
                 destination should say which one it is. */
              label={`Into ${path}`}
              hint={moving.has(path) ? 'Being moved' : undefined}
              dim={moving.has(path)}
              onSelect={moving.has(path) ? undefined : () => onNavigate(path)}
            />
          ))}

          {children.length === 0 && (
            <div style={{
              padding: '26px 16px', textAlign: 'center',
              fontFamily: FONT.sans, fontSize: '12px', color: COLORS.textMuted, lineHeight: 1.5,
            }}>
              No folders in here. Move into it as it is, or make one below.
            </div>
          )}
        </>
      )}
    </div>
  )
}

function FolderChoice({ glyph, name, label, hint, dim, onSelect }) {
  const [active, setActive] = useState(false)

  return (
    <button
      type="button"
      aria-label={label}
      onClick={onSelect}
      disabled={!onSelect}
      onPointerEnter={() => setActive(true)}
      onPointerLeave={() => setActive(false)}
      onFocus={() => setActive(true)}
      onBlur={() => setActive(false)}
      style={{
        display: 'flex', alignItems: 'center', gap: '10px',
        width: '100%', minHeight: '44px',
        padding: '8px 12px',
        background: active && onSelect ? COLORS.surfaceHover : 'transparent',
        border: 'none',
        borderBottom: `1px solid ${COLORS.border}`,
        color: dim ? COLORS.textMuted : COLORS.text,
        textAlign: 'left',
        fontFamily: FONT.mono, fontSize: '12.5px',
        cursor: onSelect ? 'pointer' : 'default',
      }}
    >
      <span style={{ width: '16px', textAlign: 'center', flexShrink: 0 }}>{glyph}</span>
      <span style={{ minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis' }}>{name}</span>
      {hint && (
        <span style={{
          marginLeft: 'auto', flexShrink: 0,
          fontFamily: FONT.sans, fontSize: '10.5px', color: COLORS.textMuted,
        }}>{hint}</span>
      )}
    </button>
  )
}

/* Making the folder to move into, from inside the dialog that wanted it.
   Without this, moving three files somewhere new means closing this, making the
   folder, finding the three files again and picking them again. */
function NewFolder({ dest, vault, onCancel, onCreated }) {
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  const create = async () => {
    const trimmed = name.trim()
    if (!trimmed) return
    setBusy(true)
    setError(null)
    try {
      const resp = await api.createFolder(joinPath(dest, trimmed), vault)
      onCreated(resp?.path || joinPath(dest, trimmed))
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }

  return (
    <div style={{ marginBottom: '12px' }}>
      {error && <Banner tone="error">{error}</Banner>}
      <div style={{ display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' }}>
        <input
          autoFocus
          value={name}
          onChange={(e) => setName(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') { e.preventDefault(); create() }
            // Escape belongs to the field while it is open — the dialog behind
            // it would otherwise close on the way out of a typo.
            if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); onCancel() }
          }}
          placeholder={`New folder in ${dest}`}
          aria-label={`New folder in ${dest}`}
          style={{
            flex: 1, minWidth: '160px',
            padding: '9px 11px',
            background: COLORS.bg,
            border: `1px solid ${COLORS.border}`,
            borderRadius: '6px',
            color: COLORS.text,
            fontFamily: FONT.mono, fontSize: '12.5px',
            outline: 'none',
            boxSizing: 'border-box',
          }}
        />
        <Button size="sm" variant="primary" onClick={create} disabled={busy || !name.trim()}>
          {busy ? <Spinner size={10} color={COLORS.bg} /> : null}Create
        </Button>
        <Button size="sm" variant="ghost" onClick={onCancel}>Cancel</Button>
      </div>
    </div>
  )
}

/* Where things are going, and what will not be going there. A reason per thing
   rather than a count: "2 of 3 would move" leaves the third one a mystery, and
   the only two reasons there are — already here, and inside itself — are each
   one short line. */
function Destination({ dest, movable, skipped }) {
  return (
    <div style={{
      padding: '11px 13px',
      background: COLORS.bg,
      border: `1px solid ${COLORS.border}`,
      borderRadius: '6px',
      fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textDim, lineHeight: 1.7,
      wordBreak: 'break-all',
    }}>
      <div style={{ color: COLORS.text }}>{dest}</div>
      <div style={{ color: COLORS.textMuted }}>
        {movable === 0
          ? 'Nothing to move here'
          : `${movable} item${movable === 1 ? '' : 's'} would move here`}
      </div>
      {skipped.slice(0, 4).map((p, i) => (
        <div key={i} style={{ color: COLORS.textMuted }}>· {p.skip}</div>
      ))}
      {skipped.length > 4 && (
        <div style={{ color: COLORS.textMuted }}>· and {skipped.length - 4} more already here</div>
      )}
    </div>
  )
}
