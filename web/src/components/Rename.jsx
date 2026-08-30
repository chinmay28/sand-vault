import React, { useEffect, useRef, useState } from 'react'
import { COLORS, FONT } from '../theme'
import { api, joinPath, parentPath } from '../api'
import { Banner, Button, Modal, Spinner } from './ui'

/* Renaming a file or a folder.

   The cheapest thing in the vault. A name is a field in the encrypted index,
   and the objects a file's parts are stored as are named after its random
   archive ID rather than after the file — so renaming rewrites one field and
   contacts no account. A folder is the same fact one level up: renaming one is
   moving it to a new name beside itself, which carries everything beneath it in
   a single index write.

   Which is why this is the same pair of calls the move dialog makes, with the
   folder left alone instead of the name. */
export default function RenameDialog({ kind, name, file, path, vault = '', onClose, onDone }) {
  const [value, setValue] = useState(name)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const input = useRef(null)

  /* Opens with the stem selected rather than the whole name, the way every
     file manager does: renaming is nearly always rewording the name and
     keeping ".mkv", and a selection over the extension means typing the
     replacement removes it. A folder has no extension, so it takes the lot. */
  useEffect(() => {
    const el = input.current
    if (!el) return
    el.focus()
    const dot = kind === 'file' ? name.lastIndexOf('.') : -1
    el.setSelectionRange(0, dot > 0 ? dot : name.length)
  }, [kind, name])

  const wanted = value.trim()
  // A name is one segment. Anything else is a move, which is a different
  // dialog and says so rather than quietly making folders.
  const separators = wanted.includes('/') || wanted.includes('\\')
  const ready = wanted !== '' && wanted !== name && !separators

  const submit = async (e) => {
    e.preventDefault()
    if (!ready || busy) return

    setBusy(true)
    setError(null)
    try {
      if (kind === 'folder') await api.moveFolder(path, joinPath(parentPath(path), wanted), vault)
      else await api.moveFile(file.id, '', wanted)
      onDone?.()
      onClose()
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }

  return (
    <Modal
      title={kind === 'folder' ? 'Rename folder' : 'Rename file'}
      subtitle={kind === 'folder' ? path : name}
      onClose={busy ? undefined : onClose}
      width={420}
    >
      <form onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}

        <p style={{
          margin: '0 0 12px',
          fontFamily: FONT.sans, fontSize: '12px', color: COLORS.textMuted, lineHeight: 1.6,
        }}>
          {kind === 'folder'
            ? 'Everything inside it comes along. Nothing is transferred — a folder is a path in the encrypted index, so this rewrites that path and leaves every part where it is.'
            : 'Nothing is transferred. A name is a field in the encrypted index, and a file’s parts are named after the file rather than after its name, so they never move.'}
        </p>

        <input
          ref={input}
          value={value}
          onChange={(e) => setValue(e.target.value)}
          aria-label={kind === 'folder' ? 'New folder name' : 'New file name'}
          spellCheck={false}
          autoCapitalize="off"
          autoCorrect="off"
          style={{
            width: '100%',
            padding: '10px 12px',
            background: COLORS.bg,
            border: `1px solid ${separators ? COLORS.error : COLORS.border}`,
            borderRadius: '6px',
            color: COLORS.text,
            fontFamily: FONT.mono,
            fontSize: '13px',
            outline: 'none',
            boxSizing: 'border-box',
          }}
        />

        <div style={{
          minHeight: '18px', margin: '6px 0 14px',
          fontFamily: FONT.mono, fontSize: '10.5px', color: separators ? COLORS.error : COLORS.textMuted,
        }}>
          {separators && 'A name is one segment — use Move to another folder to change where it sits.'}
        </div>

        <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end' }}>
          <Button type="button" variant="ghost" onClick={onClose} disabled={busy}>Cancel</Button>
          <Button type="submit" variant="primary" disabled={!ready || busy}>
            {busy ? <Spinner size={10} color={COLORS.bg} /> : null}Rename
          </Button>
        </div>
      </form>
    </Modal>
  )
}
