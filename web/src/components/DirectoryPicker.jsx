import React, { useEffect, useRef, useState } from 'react'
import { COLORS, FONT } from '../theme'
import { api } from '../api'
import { useIsMobile } from '../hooks'
import { Banner, Button, Input, Modal, Spinner } from './ui'

/* Picking a folder on the machine SAND runs on.

   The backends that take a path — a local folder, a Proton Drive sync folder —
   used to ask for one typed in full, which means knowing it by heart and
   getting every character right. On the phone the vault is usually driven
   from, that path is not even on the same machine as the keyboard.

   So this walks the server's own folders. It shows names and nothing else: the
   endpoint behind it lists folders, never files, and the vault's own contents
   are somewhere else entirely.

   A folder that is not there yet is still a valid answer — connecting creates
   it — so the picker opens at the nearest folder that does exist and keeps the
   rest as a name to create underneath it. */

export default function DirectoryPicker({ value = '', title = 'Choose a folder', onPick, onClose }) {
  const mobile = useIsMobile()

  const [listing, setListing] = useState(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState(null)
  const [showHidden, setShowHidden] = useState(false)

  // The tail of the typed path that does not exist yet, which stays the
  // picker's answer until the user navigates somewhere else.
  const [newFolder, setNewFolder] = useState('')
  const seeded = useRef(false)

  const load = async (path, { seed = false } = {}) => {
    setLoading(true)
    setError(null)
    try {
      const resp = await api.systemFolders(path)
      setListing(resp)
      // A folder that cannot be read still answers, with its parent and the
      // roots — so the picker says why and stays somewhere you can leave.
      if (resp.error) setError(resp.error)
      if (seed && !resp.exists) setNewFolder(missingTail(resp))
    } catch (err) {
      setError(err.message)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    if (seeded.current) return
    seeded.current = true
    load(value, { seed: true })
  }, [value])

  const separator = listing?.separator || '/'
  const chosen = joinPath(listing?.path || '', newFolder, separator)

  const goTo = (path) => {
    setNewFolder('')
    load(path)
  }

  const folders = (listing?.folders || []).filter((folder) => showHidden || !folder.hidden)
  const hiddenCount = (listing?.folders || []).length - folders.length

  return (
    <Modal
      title={title}
      subtitle="Folders on the machine running SAND. Pick one, or name a new one to create inside it."
      onClose={onClose}
      width={560}
      zIndex={120}
    >
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

      <Crumbs path={listing?.path || ''} separator={separator} onNavigate={goTo} />

      {listing?.roots?.length > 0 && (
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: '6px', marginBottom: '12px' }}>
          {listing.roots.map((root) => (
            <button
              key={root.path}
              type="button"
              onClick={() => goTo(root.path)}
              style={{
                padding: '5px 10px',
                minHeight: mobile ? '34px' : undefined,
                background: COLORS.bg,
                border: `1px solid ${COLORS.border}`,
                borderRadius: '20px',
                color: COLORS.textDim,
                fontFamily: FONT.mono,
                fontSize: '11px',
                cursor: 'pointer',
              }}
            >{root.label}</button>
          ))}
        </div>
      )}

      <div style={{
        border: `1px solid ${COLORS.border}`,
        borderRadius: '8px',
        background: COLORS.bg,
        // Tall enough to scroll through, short enough that the chosen path and
        // its button stay on screen next to the phone keyboard.
        height: mobile ? '38vh' : '260px',
        overflowY: 'auto',
        marginBottom: '12px',
      }}>
        {loading && (
          <div style={{ display: 'flex', justifyContent: 'center', padding: '28px 0' }}>
            <Spinner />
          </div>
        )}

        {!loading && listing?.parent && (
          <FolderRow
            glyph="↰"
            name={baseName(listing.parent, separator) || listing.parent}
            hint="Parent folder"
            onSelect={() => goTo(listing.parent)}
          />
        )}

        {!loading && folders.map((folder) => (
          <FolderRow
            key={folder.path}
            glyph="🗀"
            name={folder.name}
            dim={folder.hidden}
            onSelect={() => goTo(folder.path)}
          />
        ))}

        {!loading && folders.length === 0 && (
          <div style={{
            padding: '26px 16px',
            textAlign: 'center',
            fontFamily: FONT.sans,
            fontSize: '12px',
            color: COLORS.textMuted,
            lineHeight: 1.5,
          }}>
            {listing?.error
              ? 'SAND cannot read this folder. Try one of the shortcuts above.'
              : 'No folders in here. It can still hold the parts — choose it as it is.'}
          </div>
        )}

        {!loading && listing?.truncated && (
          <div style={{
            padding: '10px 16px',
            fontFamily: FONT.sans,
            fontSize: '11px',
            color: COLORS.textMuted,
            borderTop: `1px solid ${COLORS.border}`,
          }}>
            Only the first {folders.length} folders are shown. Type the rest of the path instead.
          </div>
        )}
      </div>

      <div style={{
        display: 'flex', alignItems: 'center', justifyContent: 'space-between',
        gap: '10px', marginBottom: '12px',
      }}>
        <label style={{
          display: 'flex', alignItems: 'center', gap: '7px',
          fontFamily: FONT.sans, fontSize: '11.5px', color: COLORS.textMuted, cursor: 'pointer',
        }}>
          <input
            type="checkbox"
            checked={showHidden}
            onChange={(e) => setShowHidden(e.target.checked)}
            style={{ accentColor: COLORS.accent }}
          />
          Show hidden folders{hiddenCount > 0 ? ` (${hiddenCount})` : ''}
        </label>
      </div>

      <Input
        label="New folder inside it (optional)"
        value={newFolder}
        onChange={(e) => setNewFolder(e.target.value)}
        /* The picker is portaled out of the connect form, so Enter here has
           nothing to submit unless it is given something. */
        onKeyDown={(e) => { if (e.key === 'Enter' && chosen) { e.preventDefault(); onPick(chosen) } }}
        placeholder="sand"
        help="Created when the account connects."
      />

      <div style={{
        padding: '10px 12px',
        marginBottom: '14px',
        background: COLORS.bg,
        border: `1px solid ${COLORS.border}`,
        borderRadius: '6px',
        fontFamily: FONT.mono,
        fontSize: '12px',
        color: COLORS.text,
        wordBreak: 'break-all',
      }}>{chosen || '—'}</div>

      <div style={{
        display: 'flex',
        flexDirection: mobile ? 'column-reverse' : 'row',
        gap: '8px',
        justifyContent: 'flex-end',
      }}>
        <Button
          type="button"
          variant={mobile ? 'default' : 'ghost'}
          onClick={onClose}
          style={mobile ? { justifyContent: 'center' } : null}
        >Cancel</Button>
        <Button
          type="button"
          variant="primary"
          disabled={!chosen}
          onClick={() => onPick(chosen)}
          style={mobile ? { justifyContent: 'center' } : null}
        >Use this folder</Button>
      </div>
    </Modal>
  )
}

/* The path as a row of steps, each one a way back up. The leading separator is
   a step of its own so "/" itself is reachable, and the row scrolls sideways
   rather than wrapping a deep path into a paragraph. */
function Crumbs({ path, separator, onNavigate }) {
  const steps = pathSteps(path, separator)

  return (
    <div style={{
      display: 'flex',
      alignItems: 'center',
      gap: '2px',
      marginBottom: '12px',
      padding: '2px 0',
      overflowX: 'auto',
      whiteSpace: 'nowrap',
    }}>
      {steps.map((step, i) => (
        <React.Fragment key={step.path}>
          {/* The root step is the separator, so the step after it does not
              need one in front of it. */}
          {i > 0 && steps[i - 1].path !== separator && (
            <span style={{ color: COLORS.textMuted, fontFamily: FONT.mono, fontSize: '11px' }}>{separator}</span>
          )}
          <button
            type="button"
            onClick={() => onNavigate(step.path)}
            style={{
              flexShrink: 0,
              padding: '4px 6px',
              background: 'none',
              border: 'none',
              borderRadius: '4px',
              color: i === steps.length - 1 ? COLORS.text : COLORS.textDim,
              fontFamily: FONT.mono,
              fontSize: '11.5px',
              cursor: 'pointer',
            }}
          >{step.label}</button>
        </React.Fragment>
      ))}
    </div>
  )
}

function FolderRow({ glyph, name, hint, dim, onSelect }) {
  const [active, setActive] = useState(false)

  return (
    <button
      type="button"
      onClick={onSelect}
      onPointerEnter={() => setActive(true)}
      onPointerLeave={() => setActive(false)}
      onFocus={() => setActive(true)}
      onBlur={() => setActive(false)}
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: '10px',
        width: '100%',
        minHeight: '44px',
        padding: '8px 12px',
        background: active ? COLORS.surfaceHover : 'transparent',
        border: 'none',
        borderBottom: `1px solid ${COLORS.border}`,
        color: dim ? COLORS.textMuted : COLORS.text,
        textAlign: 'left',
        fontFamily: FONT.mono,
        fontSize: '12.5px',
        cursor: 'pointer',
      }}
    >
      <span style={{ width: '16px', textAlign: 'center', color: COLORS.textMuted, flexShrink: 0 }}>{glyph}</span>
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

/* --- paths, in whichever shape the server's machine uses -------------------
   The browser may be a phone and the server a Windows box, so nothing here
   assumes a separator: the listing says which one it is. */

export function pathSteps(path, separator = '/') {
  if (!path) return []

  // A Unix path starts at the separator itself; a Windows one starts at the
  // drive, which is already the first segment.
  const rooted = path.startsWith(separator)
  const parts = path.split(separator).filter(Boolean)
  const steps = rooted ? [{ label: separator, path: separator }] : []

  let prefix = rooted ? '' : null
  for (const part of parts) {
    if (prefix === null) {
      // A bare "C:" means "wherever that drive was last left", so a drive step
      // keeps its separator and stays an absolute path.
      prefix = part.endsWith(':') ? `${part}${separator}` : part
    } else {
      prefix = prefix.endsWith(separator) ? `${prefix}${part}` : `${prefix}${separator}${part}`
    }
    steps.push({ label: part, path: prefix })
  }
  return steps
}

export function joinPath(dir, name, separator = '/') {
  const trimmed = (name || '').trim().replace(/^[/\\]+|[/\\]+$/g, '')
  if (!trimmed) return dir
  if (!dir) return trimmed
  return dir.endsWith(separator) ? `${dir}${trimmed}` : `${dir}${separator}${trimmed}`
}

export function baseName(path, separator = '/') {
  if (!path || path === separator) return path
  const parts = path.split(separator).filter(Boolean)
  return parts[parts.length - 1] || path
}

/* What was typed, minus the part of it that is really there — the folders
   connecting would have to create. */
export function missingTail({ requested = '', path = '', separator = '/' } = {}) {
  if (!requested || !path || !requested.startsWith(path)) return ''
  return requested.slice(path.length).replace(/^[/\\]+/, '')
}
