import React, { useEffect, useState } from 'react'
import { COLORS, FONT, accountColor, fileIcon, formatBytes, formatDate, formatDay, isPlayable } from '../theme'
import { api } from '../api'
import { useDownload } from '../download'
import { FolderZip } from './FolderZip'
import { SaveSheet } from './SaveSheet'
import { MachineTransfer, fileItem } from './MachineTransfer'
import { Thumb } from './Thumb'
import { startErase } from '../erase'
import { ActionSheet, Banner, Button, ConfirmDialog, IconButton, Modal } from './ui'
import StreamLink from './StreamLink'
import ConvertFile from './ConvertFile'
import { PreviousKeyPill } from './Rekey'
import { onPreviousKey } from '../keys'
import { RelocateClouds, fileScheme, schemeName, storedParts } from './CloudSelect'
import { ReplicaBadges, ReplicaDots, ReplicateFolderDialog } from './Replication'
import MoveToFolder from './MoveToFolder'
import FolderArtPicker from './FolderArt'
import RenameDialog from './Rename'

/* One file or one folder, drawn as a row or as a tile.

   The two views differ in shape and in nothing else: the same file offers the
   same things to do with it either way. So everything that is not layout —
   which dialogs a file can open, what it is allowed to do in the state it is
   in, what the menu says — lives in useFileActions below, and a row and a tile
   are two arrangements of what it hands back. */

/* Name, size, modified, parts, actions. The four fixed columns come to nearly
   600px, which is why the phone layout stacks instead of shrinking them.

   The last of them ends in a ⋯ that opens the same sheet a phone gets, which
   is what stops this column growing by a button every time a file learns to do
   something new: the quick controls are the three or four things worth a click
   from the row, and everything else lives one click deeper. */
export const COLUMNS = 'minmax(0,1fr) 92px 150px 132px 180px'

/* The same again in a folder that has asked for film details, where a video
   row carries one control more. The column grows by exactly one button rather
   than the ones already there being crowded together — and it grows for the
   whole table, because a heading has to sit over the column it names. */
export const FILM_COLUMNS = 'minmax(0,1fr) 92px 150px 132px 216px'

/* Everything a file row or tile can do, and every dialog it can open.

   Held in one place because a file's state decides most of it: a file stored
   before chunking cannot be opened or streamed until it is converted, and one
   down to a single part cannot be rebuilt at all. Working that out twice —
   once for rows and once for tiles — is how the two views drift apart. */
export function useFileActions({
  file, providers, film, vault = '', subVaults = [], replicate = null,
  onPreview, onInspect, onFilm, onRefresh, onError,
}) {
  const [busy, setBusy] = useState(false)
  const [download, downloading, pendingSave, dismissSave, downloadProgress] = useDownload(onError)
  /* What the row's menu says while a download runs: the share of it that
     has landed, once any has. Before that the clouds are still being asked,
     and there is no honest number. */
  const downloadPct = downloading && downloadProgress?.received > 0 && file.size > 0
    ? Math.min(100, Math.round((downloadProgress.received / file.size) * 100))
    : null
  const [menu, setMenu] = useState(false)
  const [confirming, setConfirming] = useState(false)
  /* null, or 'play' when the stream dialog should reach for VLC on the way in
     and 'link' when it should just show the address. */
  const [streaming, setStreaming] = useState(null)
  const [converting, setConverting] = useState(null)
  const [relocating, setRelocating] = useState(false)
  const [moving, setMoving] = useState(false)
  const [renaming, setRenaming] = useState(false)
  const [assigning, setAssigning] = useState(false)
  const [sending, setSending] = useState(false)

  /* Where this could be sent. Standing in the main vault that is every open
     sub vault; standing inside one it is the way back out. A locked sub vault
     is not offered — the move has to write into its index. */
  const assignTargets = vault
    ? [{ id: '', label: 'the main vault' }]
    : subVaults.filter((s) => s.unlocked).map((s) => ({ id: s.id, label: s.label }))

  /* A file stored before chunked storage existed. It cannot be read at an
     offset, so nothing opens or streams it until it has been converted — the
     row says so rather than letting a click fail. */
  const legacy = !file.chunk_count
  /* Counted against the file's own scheme: a 4-of-6 file with five shards is
     whole with a spare, where a 2-of-3 one with five would be impossible. */
  const degraded = storedParts(file.shards) < fileScheme(file).total
  const dead = storedParts(file.shards) < fileScheme(file).data
  // Only what a player is any use for gets offered one.
  const playable = isPlayable(file.mime, file.name)

  /* Handed to the machine rather than waited on: the answer is a run, drawn
     above the file list while the parts go, and the row leaves the listing
     when it is done. A file that was already gone refreshes the listing,
     which is what has the row still in it. */
  const remove = async () => {
    setBusy(true)
    try {
      const run = await startErase([{ id: file.id }])
      setConfirming(false)
      if (!run) onRefresh()
    } catch (err) {
      onError(err.message)
      setConfirming(false)
    } finally {
      setBusy(false)
    }
  }

  /* Whether this file gets a second copy: its own say if it has one, and its
     folder's otherwise. Main vault only — a replica is a main-vault placement,
     so inside a sub vault the menu says nothing about it. */
  const replicated = file.replicate ?? !!replicate?.enabled
  const filePath = `${file.dir === '/' ? '' : file.dir}/${file.name}`
  const setReplicate = async (enabled) => {
    setBusy(true)
    try {
      await api.setReplicate(filePath, enabled)
      onRefresh()
    } catch (err) {
      onError(err.message)
    } finally {
      setBusy(false)
    }
  }
  const dropReplica = async () => {
    setBusy(true)
    try {
      const resp = await api.dropReplica(file.id)
      if (resp.warnings?.length) onError(resp.warnings.join('\n'))
      onRefresh()
    } catch (err) {
      onError(err.message)
    } finally {
      setBusy(false)
    }
  }

  const open = legacy ? () => setConverting('open') : onPreview
  const openTitle = dead
    ? 'Too few parts remain to rebuild this file'
    : legacy ? 'Stored in the old format — convert it before it can be opened' : 'Open'

  const dialogs = (
    <>
      {/* A home-screen app holds a rebuilt file for the share sheet rather
          than saving it, and this is where it is offered. Null elsewhere. */}
      <SaveSheet pending={pendingSave} onDone={dismissSave} />
      {menu && (
        <ActionSheet
          title={file.name}
          subtitle={`${formatBytes(file.size)} · ${formatDate(file.modified_at)}`}
          onClose={() => setMenu(false)}
          items={[
            legacy ? {
              key: 'convert',
              glyph: '◈',
              label: 'Convert to chunks',
              hint: 'Stored in the old format, which cannot be opened or streamed until converted',
              disabled: dead,
              onSelect: () => setConverting('open'),
            } : {
              key: 'open',
              glyph: '◱',
              label: 'Open',
              hint: dead ? 'Too few parts remain to rebuild this file' : 'Gather the parts and rebuild it here',
              disabled: dead,
              onSelect: onPreview,
            },
            // Sits under Open because it is the same intent aimed elsewhere:
            // watch this, but in the player that can actually seek it.
            playable && !legacy && {
              key: 'stream',
              glyph: '▶',
              label: 'Stream in VLC',
              hint: dead ? 'Too few parts remain to rebuild this file' : 'Open it in VLC and start playing',
              disabled: dead,
              onSelect: () => setStreaming('play'),
            },
            !legacy && {
              key: 'copy-link',
              glyph: '⧉',
              label: 'Copy the address',
              hint: 'A link any player or app can open',
              disabled: dead,
              onSelect: () => setStreaming('link'),
            },
            !legacy && {
              key: 'download',
              glyph: '↓',
              // The sheet closes on the way out and the fetch carries on
              // behind it — a home-screen app has no tab to park a download
              // in, so nothing here may navigate.
              label: downloading
                ? (downloadPct === null ? 'Downloading…' : `Downloading… ${downloadPct}%`)
                : 'Download',
              hint: 'Save the rebuilt, decrypted file',
              disabled: downloading,
              onSelect: () => download(file),
            },
            /* Under Download because it is the same thing aimed elsewhere:
               the file leaves the vault whole and readable, but lands on a
               machine you have a login on instead of in this browser. It goes
               clouds → SAND → machine, so a phone can send a film it could
               never hold. */
            !legacy && {
              key: 'send',
              glyph: '⇡',
              label: 'Send to a machine',
              hint: dead
                ? 'Too few parts remain to rebuild this file'
                : 'Copy it onto a machine you have a login on — in the clear',
              disabled: dead,
              onSelect: () => setSending(true),
            },
            // Only where it could mean something: a folder that has asked for
            // film details, and a file a player would take. Everywhere else it
            // would be a row in every menu in the vault that answers "no".
            onFilm && {
              key: 'film',
              glyph: '🎬',
              label: film ? 'Film details' : 'Look up the film',
              hint: film
                ? [film.title, film.year].filter(Boolean).join(' · ')
                : 'Match it against the film database and keep the poster',
              onSelect: onFilm,
            },
            {
              key: 'parts',
              glyph: '◈',
              label: 'Where the shards live',
              hint: describeParts(file),
              onSelect: onInspect,
            },
            /* The second copy: on or off for this one file, over whatever its
               folder says, and the replica itself gone now rather than at the
               next sweep. */
            !vault && {
              key: 'replicate',
              glyph: '⧉',
              label: replicated ? 'Stop replicating this file' : 'Replicate this file',
              hint: replicated
                ? (file.replica ? 'Its replica stays until it is dropped' : 'Whatever its folder says')
                : 'A second copy on the replication clouds, whatever its folder says',
              onSelect: () => setReplicate(!replicated),
            },
            !vault && file.replica && {
              key: 'drop-replica',
              glyph: '⧉',
              label: 'Drop the replica',
              hint: 'Erase the second copy now; the parts are untouched',
              danger: true,
              onSelect: dropReplica,
            },
            /* Rename and the two moves, together: all three change where a
               file answers from and none of them touch what is stored. */
            {
              key: 'rename',
              glyph: '✎',
              label: 'Rename',
              hint: 'Only the index changes — the parts are not named after the file',
              onSelect: () => setRenaming(true),
            },
            {
              key: 'move',
              glyph: '→',
              label: 'Move to another folder',
              hint: 'Only the index changes — the parts stay on the same clouds',
              onSelect: () => setMoving(true),
            },
            {
              key: 'relocate',
              glyph: '⇄',
              label: 'Move to other clouds',
              hint: 'Only the parts that have to move are copied',
              onSelect: () => setRelocating(true),
            },
            // Only where there is somewhere to send it. A vault with no sub
            // vaults should not be offered a move into one.
            assignTargets.length > 0 && {
              key: 'assign',
              glyph: '🔒',
              label: vault ? 'Move out of this sub vault' : 'Move into a sub vault',
              hint: vault
                ? 'Back into the main vault, where a mounted drive can see it again'
                : 'Sealed under its own password, and never on a mounted drive',
              onSelect: () => setAssigning(true),
            },
            {
              key: 'delete',
              glyph: '✕',
              label: 'Delete',
              hint: 'Erases every part, everywhere',
              danger: true,
              onSelect: () => setConfirming(true),
            },
          ]}
        />
      )}

      {streaming && (
        <StreamLink
          file={file}
          autoplay={streaming === 'play'}
          onClose={() => setStreaming(null)}
        />
      )}

      {converting && (
        <ConvertFile
          file={file}
          onClose={() => setConverting(null)}
          onConverted={onRefresh}
        />
      )}

      {confirming && (
        <ConfirmDialog
          title={`Delete ${file.name}?`}
          busy={busy}
          onConfirm={remove}
          onClose={() => !busy && setConfirming(false)}
        >
          Every part is erased from the accounts holding it, on the machine — you can keep
          working, and the row goes when it is done. This cannot be undone.
        </ConfirmDialog>
      )}

      {relocating && (
        <RelocateClouds
          target={{ id: file.id, vault }}
          title={`Move ${file.name}`}
          subtitle={`${formatBytes(file.size)} — ${schemeName(fileScheme(file))}; pick the clouds its shards should live on`}
          /* Already selected: where the parts are now, so the dialog opens on
             the truth and a swap is one click rather than four. */
          current={file.shards.map((s) => s.provider_id)}
          providers={providers}
          /* A rebuild from the replica is offered where there is one — and
             opened on it when the parts are past reading, since moving
             shards that cannot be gathered is not a way back. */
          replica={!vault && !!file.replica}
          source={!vault && file.replica && dead ? 'replica' : 'parts'}
          onClose={() => setRelocating(false)}
          onDone={onRefresh}
        />
      )}

      {moving && (
        <MoveToFolder
          items={[{ kind: 'file', name: file.name, file }]}
          vault={vault}
          onClose={() => setMoving(false)}
          onDone={onRefresh}
        />
      )}

      {assigning && (
        <AssignToVault
          label={file.name}
          target={file.id}
          from={vault}
          targets={assignTargets}
          onClose={() => setAssigning(false)}
          onDone={() => { setAssigning(false); onRefresh() }}
          onError={onError}
        />
      )}

      {renaming && (
        <RenameDialog
          kind="file"
          name={file.name}
          file={file}
          onClose={() => setRenaming(false)}
          onDone={onRefresh}
        />
      )}

      {sending && (
        <MachineTransfer
          /* Opened from the file, so it stands where the file does: the
             picker, should the choice be changed, starts in its folder. */
          path={file.dir || '/'}
          vault={vault}
          mode="export"
          preset={[fileItem(file)]}
          onClose={() => setSending(false)}
          onChanged={onRefresh}
        />
      )}
    </>
  )

  return {
    busy, downloading, download, legacy, degraded, dead, playable, open, openTitle, dialogs,
    openMenu: () => setMenu(true),
    convert: () => setConverting('open'),
    stream: (mode) => setStreaming(mode),
    confirmDelete: () => setConfirming(true),
    relocate: () => setRelocating(true),
    moveTo: () => setMoving(true),
    rename: () => setRenaming(true),
    send: () => setSending(true),
  }
}

/* The tick beside a row or tile. Only drawn once selecting has been turned on,
   which is also why a checkbox anywhere else in the app — the cloud picker's,
   say — is never confused for one of these. */
export function SelectBox({ checked, label, mobile, onChange }) {
  return (
    <label
      title={label}
      style={{
        display: 'flex', alignItems: 'center', justifyContent: 'center',
        // A tick is a few pixels of ink either way, so the label around it is
        // what a fingertip actually aims at.
        width: mobile ? '38px' : '24px',
        minHeight: mobile ? '44px' : '24px',
        flexShrink: 0,
        cursor: 'pointer',
      }}
    >
      <input
        type="checkbox"
        checked={checked}
        aria-label={label}
        onChange={(e) => onChange(e.target.checked, e.nativeEvent)}
        style={{
          accentColor: COLORS.accent,
          width: mobile ? '19px' : '15px',
          height: mobile ? '19px' : '15px',
          minHeight: 0,
          cursor: 'pointer',
        }}
      />
    </label>
  )
}

/* One line of the table. The tick, where there is one, sits outside the grid
   so that turning selection on does not move the columns underneath it. */
export function Row({ children, mobile, columns = COLUMNS, check, selected }) {
  const [hover, setHover] = useState(false)

  const inner = mobile
    // Too narrow for columns, so the row becomes a stack: the name and its
    // menu on the first line, the details underneath.
    ? { display: 'flex', flexDirection: 'column', rowGap: '2px' }
    : { display: 'grid', gridTemplateColumns: columns, gap: '12px', alignItems: 'center' }

  return (
    <div
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: check ? (mobile ? '4px' : '6px') : 0,
        // It is also roomier than the desktop row — a row a fingertip has to
        // hit needs the height more than the screen needs to hold one more file.
        padding: mobile ? '6px 10px 8px' : '9px 14px',
        borderBottom: `1px solid ${COLORS.border}22`,
        // A picked row keeps its highlight when the pointer leaves it, which is
        // the whole point of picking it.
        background: selected ? `${COLORS.accent}1f` : hover ? COLORS.surfaceHover : 'transparent',
        fontFamily: FONT.mono,
        fontSize: '11.5px',
        color: COLORS.textDim,
        minHeight: mobile ? '64px' : '38px',
      }}
    >
      {check}
      <div style={{ ...inner, flex: 1, minWidth: 0 }}>{children}</div>
    </div>
  )
}

/* The tappable name. On a phone it claims the whole first line and a 44px
   height, so opening a file means hitting the row rather than the glyph.
   `location` is the folder the row was found in, which only a search result
   has to say. */
function NameButton({ mobile, icon, label, location, chevron, disabled, title, onClick }) {
  return (
    <button
      onClick={onClick}
      title={title}
      disabled={disabled}
      style={{
        display: 'flex', alignItems: 'center', gap: '9px',
        flex: 1, minWidth: 0,
        minHeight: mobile ? '44px' : 0,
        background: 'none', border: 'none',
        padding: mobile ? '0 2px' : 0,
        borderRadius: '8px',
        cursor: disabled ? 'not-allowed' : 'pointer',
        fontFamily: FONT.mono,
        fontSize: mobile ? '13.5px' : '12.5px',
        color: disabled ? COLORS.error : COLORS.text,
        overflow: 'hidden', textAlign: 'left',
      }}
    >
      {chevron !== undefined
        ? <span style={{ color: COLORS.accent, flexShrink: 0 }}>{chevron}</span>
        /* Lines file names up under the folder rows' ▸ chevron. */
        : <span style={{ width: '12px', flexShrink: 0 }} />}
      <span style={{ flexShrink: 0 }}>{icon}</span>
      <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{label}</span>
      {location && (
        /* Shrinks before the name does: a truncated name is worse than a
           truncated path. */
        <span
          title={location}
          style={{
            minWidth: 0, flexShrink: 1,
            color: COLORS.textMuted, fontSize: mobile ? '11px' : '10.5px',
            overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
          }}
        >in {location}</span>
      )}
    </button>
  )
}

/* What a file's placement comes to, in one line: the scheme, and how much of
   it is actually stored. */
function describeParts(file) {
  const scheme = fileScheme(file)
  const parts = `${schemeName(scheme)} · ${storedParts(file.shards)} of ${scheme.total} shards stored`
  const r = file.replica
  if (!r) return parts
  /* The second placement in the same breath, so the hint says what the
     inspector is going to show: a whole copy, or a replica cut its own way. */
  const stored = new Set(r.shards.map((s) => s.part)).size
  const replica = r.total_shards === 1
    ? 'a whole replica on one cloud'
    : `replica ${r.data_shards}-of-${r.total_shards}, ${stored} of ${r.total_shards} stored`
  return `${parts} · ${replica}`
}

/* Which account holds each of the file's shards, as one coloured square each.

   A 2-of-3 file draws three and a 6-of-9 file draws nine, so the row says at a
   glance how widely the file is spread as well as whether anything is missing.
   The squares narrow past three so that nine of them still fit the column.
   This is the desktop row's and the tile's read-out; a phone row draws
   PartDots instead. */
function PartBadges({ file }) {
  const scheme = fileScheme(file)
  const degraded = storedParts(file.shards) < scheme.total
  const dead = storedParts(file.shards) < scheme.data
  const tight = scheme.total > 3

  return (
    <>
      {Array.from({ length: scheme.total }, (_, i) => i + 1).map((part) => {
        const shard = file.shards.find((s) => s.part === part)
        return (
          <span
            key={part}
            title={shard
              ? `Shard ${part} on ${shard.provider_name}`
              : `Shard ${part} not stored`}
            style={{
              width: tight ? '12px' : '19px',
              height: '15px',
              borderRadius: '3px',
              fontFamily: FONT.mono,
              fontSize: tight ? '7.5px' : '8.5px',
              fontWeight: 700,
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'center',
              flexShrink: 0,
              color: shard ? COLORS.bg : COLORS.textMuted,
              background: shard ? accountColor(shard.provider_id) : 'transparent',
              border: shard ? 'none' : `1px dashed ${COLORS.border}`,
            }}
          >{part}</span>
        )
      })}
      {degraded && (
        <span style={{ marginLeft: '3px', color: dead ? COLORS.error : COLORS.warn, fontSize: '11px' }}>
          {dead ? '✗' : '!'}
        </span>
      )}
      {/* The second copy, outlined, after a hairline. See Replication.jsx. */}
      <ReplicaBadges file={file} />
      {/* Still sealed under the key the previous password protected, waiting
          for the re-encryption to reach it. Beside the parts because it is a
          fact about how the parts are sealed, and quiet because it is not a
          fault. See Rekey.jsx. */}
      {onPreviousKey(file) && <PreviousKeyPill />}
    </>
  )
}

/* The same question, answered in the space a phone row can spare: one small
   dot per shard in its account's colour, and the scheme spelled out beside
   the dots — "10-of-15" says how many dots there are, since nobody counts
   fifteen, and says what a bare count could not: how many are enough to
   rebuild. The numbers on the dots go — a phone's badges were never a
   control, and the inspector in the row's menu names every shard — so a
   15-cloud spread costs 118px where the squares wanted 236px.

   A missing shard is a hollow dot, and the stored count steps in front of the
   scheme as "stored/total" in the warning colour — or the error colour once
   too few remain to rebuild — so the live number leads and the scheme stands
   muted behind it as the floor it is falling toward. */
function PartDots({ file }) {
  const scheme = fileScheme(file)
  const stored = storedParts(file.shards)
  const degraded = stored < scheme.total
  const dead = stored < scheme.data

  return (
    <>
      <span style={{ display: 'flex', alignItems: 'center', flexWrap: 'wrap', gap: '2px' }}>
        {Array.from({ length: scheme.total }, (_, i) => i + 1).map((part) => {
          const shard = file.shards.find((s) => s.part === part)
          return (
            <span
              key={part}
              style={{
                width: '6px',
                height: '6px',
                borderRadius: '50%',
                flexShrink: 0,
                boxSizing: 'border-box',
                background: shard ? accountColor(shard.provider_id) : 'transparent',
                border: shard ? 'none' : `1px dashed ${COLORS.borderBright}`,
              }}
            />
          )
        })}
      </span>
      {degraded ? (
        <>
          <span style={{ fontSize: '10px', fontWeight: 700, color: dead ? COLORS.error : COLORS.warn }}>
            {stored}/{scheme.total}
          </span>
          <span style={{ fontSize: '10px', color: COLORS.textMuted }}>· {schemeName(scheme)}</span>
        </>
      ) : (
        <span style={{ fontSize: '10px', color: COLORS.textMuted }}>{schemeName(scheme)}</span>
      )}
      <ReplicaDots file={file} />
      {onPreviousKey(file) && <PreviousKeyPill tiny />}
    </>
  )
}

export function FileRow({
  file, location, mobile, providers, hasThumb, film, columns, selecting, selected,
  onSelect, onPreview, onInspect, onFilm, onRefresh, onError,
  vault = '', subVaults = [], replicate = null,
}) {
  const a = useFileActions({
    file, providers, film, vault, subVaults, replicate, onPreview, onInspect, onFilm, onRefresh, onError,
  })
  const icon = fileIcon(file.mime, file.name)

  const check = selecting && (
    <SelectBox mobile={mobile} checked={selected} label={`Select ${file.name}`} onChange={onSelect} />
  )

  const name = (
    <NameButton
      mobile={mobile}
      /* On a desktop the picture stands exactly where the emoji did, so the
         Name column keeps its width and the row its height. */
      icon={<Thumb id={file.id} icon={icon} size={26} expected={hasThumb} />}
      label={file.name}
      location={location}
      disabled={a.dead}
      title={a.openTitle}
      onClick={a.open}
    />
  )

  /* On a phone the dots are a read-out and nothing more. A third target in a
     row that already has a name and a menu would have to be either too small
     to hit or tall enough to push the next file off the screen — and the menu
     already offers the same inspector by name. On a desktop the badges stay
     the shortcut they have always been. */
  const parts = mobile ? (
    <span
      title={describeParts(file)}
      style={{ display: 'flex', alignItems: 'center', gap: '4px', flexShrink: 0 }}
    ><PartDots file={file} /></span>
  ) : (
    <button
      onClick={onInspect}
      title="Where the shards live"
      aria-label="Where the shards live"
      style={{
        display: 'flex', alignItems: 'center', gap: '3px',
        background: 'none', border: 'none', padding: 0,
        borderRadius: '6px', cursor: 'pointer', flexShrink: 0,
      }}
    ><PartBadges file={file} /></button>
  )

  /* A pointer can pick between two 34px squares. A fingertip cannot, and one
     of them deletes the file everywhere, so the phone gets a single menu
     button instead and spells the choices out in a sheet. */
  const actions = mobile ? (
    <IconButton
      glyph="⋯"
      label={`Actions for ${file.name}`}
      onClick={a.openMenu}
      size={44}
      style={{ background: COLORS.surfaceRaised, border: `1px solid ${COLORS.border}`, fontSize: '18px' }}
    />
  ) : (
    <span style={{ display: 'flex', gap: '2px', justifyContent: 'flex-end', flexShrink: 0 }}>
      {/* Only in a folder that asked for film details, which is also the only
          folder whose rows have room for it — see FILM_COLUMNS. */}
      {onFilm && (
        <IconButton
          glyph="🎬"
          label={film ? `Film details for ${file.name}` : `Look up the film in ${file.name}`}
          title={film
            ? [film.title, film.year].filter(Boolean).join(' · ')
            : 'Match it against the film database'}
          tone={film ? 'dim' : 'muted'}
          onClick={onFilm}
          style={{ fontSize: '13px' }}
        />
      )}
      {/* Three controls is what the column has room for, so this one is on the
          rows it means something on: a player is no use on a PDF. */}
      {a.legacy ? (
        <IconButton
          glyph="◈"
          label={`Convert ${file.name}`}
          title="Stored in the old format — convert it to open or stream it"
          tone="muted"
          onClick={a.convert}
        />
      ) : a.playable && !a.dead && (
        <IconButton
          glyph="▶"
          label={`Stream ${file.name}`}
          title="Open in VLC, or copy the address"
          onClick={() => a.stream('play')}
        />
      )}
      {/* Every row carries this control, so the spoken name says which file it
          belongs to rather than repeating the same sentence down the list. */}
      <IconButton
        glyph={a.downloading ? '…' : '↓'}
        label={`Download ${file.name}`}
        title="Download the rebuilt, decrypted file"
        disabled={a.downloading}
        onClick={() => a.download(file)}
      />
      {/* Moving it to another folder, which every row carries for the same
          reason it carries Delete: it is something you do to a file wherever
          you happen to be looking at it, and it costs nothing to do.
          Moving the *parts* to other clouds is the other question and lives in
          the parts inspector, one click away through the badges — beside the
          read-out of where they are now, which is what it answers. */}
      <IconButton
        glyph="→"
        label={`Move ${file.name} to another folder`}
        title="Move it into another folder — the parts stay exactly where they are"
        onClick={a.moveTo}
      />
      <IconButton
        glyph={a.busy ? '…' : '✕'}
        label="Delete everywhere"
        tone="muted"
        disabled={a.busy}
        onClick={a.confirmDelete}
      />
      {/* Everything else a file can do, in the same sheet a phone gets. A row
          cannot grow a control per feature — renaming would have been the
          fifth — and the things that end up here are the ones worth a click
          rather than a glance: rename, the parts, the other move, the film. */}
      <IconButton
        glyph="⋯"
        label={`Actions for ${file.name}`}
        onClick={a.openMenu}
      />
    </span>
  )

  /* Whether the phone row's dots share the size-and-date line or take a line
     of their own underneath. Three is the cut: three dots and a count fit
     beside a date, and three is also what most files are — so the common row
     stays two lines and only a widely-spread file grows the third. */
  const wideSpread = fileScheme(file).total > 3

  if (mobile) {
    /* The picture is the row's left column and the two lines of text sit
       beside it, rather than the name being indented over a line of detail.
       Six pixels taller than the row it replaces, and the tile is inside the
       same tap target as the name — pointing at the photo is how anyone would
       expect to open it. */
    return (
      <Row mobile check={check} selected={selected}>
        <div style={{ display: 'flex', alignItems: 'center', gap: '10px' }}>
          <button
            onClick={a.open}
            disabled={a.dead}
            title={a.openTitle}
            style={{
              display: 'flex', alignItems: 'center', gap: '10px',
              flex: 1, minWidth: 0, minHeight: '52px',
              background: 'none', border: 'none', padding: '0 2px',
              borderRadius: '8px', textAlign: 'left',
              cursor: a.dead ? 'not-allowed' : 'pointer',
            }}
          >
            <Thumb id={file.id} icon={icon} size={52} expected={hasThumb} />

            <span style={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column', gap: '3px' }}>
              <span style={{
                fontFamily: FONT.mono, fontSize: '13.5px',
                color: a.dead ? COLORS.error : COLORS.text,
                overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
              }}>{file.name}</span>

              {/* This line carries the size and the date — and the dots too,
                  but only for a file on three clouds or fewer, which is what
                  the line can hold beside a date in the 224px a 390px screen
                  gives it; a wider spread gets the line below to itself.
                  Hence no "·" separator, unlike everywhere else, and hence
                  the dots are pushed right with a margin rather than a spacer
                  element — a spacer would cost a flex gap of its own, which is
                  the difference between reading the time and truncating it. */}
              <span style={{
                display: 'flex', alignItems: 'center', gap: '6px',
                fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textMuted,
              }}>
                <span style={{ whiteSpace: 'nowrap', flexShrink: 0 }}>{formatBytes(file.size)}</span>
                <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                  {formatDate(file.modified_at)}
                </span>
                {location && (
                  <span
                    title={location}
                    style={{ minWidth: 0, flexShrink: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
                  >in {location}</span>
                )}
                {!wideSpread && (
                  <span style={{ marginLeft: 'auto', display: 'flex', flexShrink: 0 }}>{parts}</span>
                )}
              </span>
              {wideSpread && parts}
            </span>
          </button>

          {actions}
        </div>
        {a.dialogs}
      </Row>
    )
  }

  return (
    <Row check={check} columns={columns} selected={selected}>
      {name}
      <span>{formatBytes(file.size)}</span>
      <span>{formatDate(file.modified_at)}</span>
      {parts}
      {actions}
      {a.dialogs}
    </Row>
  )
}

/* The three figures a folder's menu leads with: how much is under it, in how
   many files, and how many clouds those files' parts went to.

   Everything at or below the folder rather than what is directly in it, which
   is the `du -sh` reading and the only one worth showing — a folder of folders
   holds nothing directly and is not empty. Nothing is drawn until the answer
   arrives: figures that appear are read as figures, and figures that count up
   from zero are read as a folder that just emptied itself. */
function folderFigures(held) {
  if (!held) return []
  // A folder with nothing at all in it says so in the line below instead. Two
  // zeroes under two labels is a slower way to read the same word.
  if (held.files === 0 && held.folders === 0) return []
  return [
    { key: 'files', value: held.files, label: held.files === 1 ? 'file' : 'files' },
    { key: 'bytes', value: formatBytes(held.bytes), label: 'in here' },
    held.clouds?.length > 0 && {
      key: 'clouds',
      value: held.clouds.length,
      label: held.clouds.length === 1 ? 'cloud' : 'clouds',
      title: held.clouds.join(', '),
    },
  ]
}

/* The rest of it, in the one line under the figures.

   What the parts weigh is a different number from what the folder holds — the
   erasure coding sees to that, and it is the figure the accounts' free space is
   actually spent in, so a folder of 4 GB costing 6 GB of cloud should not have
   to be worked out from the placement policy. The folders inside and the last
   thing to change are here because they are the two questions a folder row
   leaves open and its own listing answers a level at a time. */
function folderNote(held) {
  if (!held) return null
  if (held.files === 0 && held.folders === 0) return 'Empty — nothing in here yet'

  const parts = []
  if (held.stored_bytes > 0) parts.push(`${formatBytes(held.stored_bytes)} across the clouds`)
  if (held.folders > 0) parts.push(`${held.folders} folder${held.folders === 1 ? '' : 's'} inside`)
  if (held.newest) parts.push(`newest ${formatDay(held.newest)}`)
  if (held.degraded > 0) {
    parts.push(`${held.degraded} file${held.degraded === 1 ? '' : 's'} short of a part`)
  }

  /* Each phrase kept whole. The line is long enough to wrap on a phone, and a
     wrap through the middle of "newest Aug 23" reads as two half-facts. */
  return parts.map((part, i) => (
    <React.Fragment key={part}>
      {i > 0 && ' · '}
      <span style={{ whiteSpace: 'nowrap' }}>{part}</span>
    </React.Fragment>
  ))
}

/* Everything a folder row or tile can do. Far shorter than a file's, because a
   folder is a name in the index rather than something stored: it can be walked
   into, moved onto other clouds wholesale, or deleted with what is inside it. */
function useFolderActions({
  name, path, art, providers, vault = '', subVaults = [], replicated = false, replicate = null,
  onNavigate, onRefresh, onError,
}) {
  const [menu, setMenu] = useState(false)
  const [confirming, setConfirming] = useState(false)
  const [replicating, setReplicating] = useState(false)
  const [relocating, setRelocating] = useState(false)
  const [moving, setMoving] = useState(false)
  const [renaming, setRenaming] = useState(false)
  const [picturing, setPicturing] = useState(false)
  const [assigning, setAssigning] = useState(false)
  const [zipping, setZipping] = useState(false)
  const [sending, setSending] = useState(false)

  const [busy, setBusy] = useState(false)

  /* What the folder is holding, fetched when the menu opens and not before.
     A folder row cannot say how big it is — the weight is in the levels below
     it, and finding it is a walk of the index per folder, which is not a price
     a listing of forty of them should pay on the chance somebody wonders. The
     menu is where somebody has wondered, so it is asked there and the header
     fills in a moment later.

     A failure is left silent on purpose. This is the answer to a question
     nobody typed; an error banner over an open menu would be louder than the
     thing it is reporting, and every choice below still works without it. */
  const [held, setHeld] = useState(null)

  useEffect(() => {
    if (!menu) return undefined
    let live = true
    setHeld(null)
    api.folderStats(path, vault)
      .then((stats) => { if (live) setHeld(stats) })
      .catch(() => {})
    return () => { live = false }
  }, [menu, path, vault])

  const assignTargets = vault
    ? [{ id: '', label: 'the main vault' }]
    : subVaults.filter((s) => s.unlocked).map((s) => ({ id: s.id, label: s.label }))

  const open = () => onNavigate(path)

  /* Handed to the machine, as a file's delete is: a folder of hundreds of
     files takes minutes to erase, and that is minutes nobody has to spend
     looking at a dialog. The run is drawn above the file list, counting the
     folder's files down, and the folder leaves the listing when it is done. */
  const remove = async () => {
    setBusy(true)
    try {
      const run = await startErase([{ path, vault }])
      setConfirming(false)
      if (!run) onRefresh()
    } catch (err) {
      onError(err.message)
      setConfirming(false)
    } finally {
      setBusy(false)
    }
  }

  const dialogs = (
    <>
      {menu && (
        <ActionSheet
          title={name}
          subtitle="Folder"
          figures={folderFigures(held)}
          note={folderNote(held)}
          onClose={() => setMenu(false)}
          items={[
            { key: 'open', glyph: '▸', label: 'Open folder', onSelect: open },
            /* The two ways a folder leaves the vault whole. Neither buffers
               it: the archive streams as it is built, and a send goes straight
               from the clouds onto the machine, a piece at a time. */
            {
              key: 'zip',
              glyph: '↓',
              label: 'Download as zip',
              hint: 'Everything inside it as one archive, built as it downloads',
              onSelect: () => setZipping(true),
            },
            {
              key: 'send',
              glyph: '⇡',
              label: 'Send to a machine',
              hint: 'Copy it onto a machine you have a login on — in the clear',
              onSelect: () => setSending(true),
            },
            /* The second copy, for the folder and everything under it. Main
               vault only: a replica is a main-vault placement. */
            !vault && {
              key: 'replicate',
              glyph: '⧉',
              label: replicated ? 'Stop replicating' : 'Replicate this folder',
              hint: replicated
                ? 'Replicas already made stay until they are dropped'
                : 'A second copy on the replication clouds, in a folder of their own',
              onSelect: () => setReplicating(true),
            },
            /* Only where there is something to choose from. A folder with
               nothing picturable inside it has nothing to offer, and the entry
               would open a dialog whose whole content is an apology. */
            art && {
              key: 'art',
              glyph: '🖼',
              label: 'Folder picture',
              hint: art.id
                ? 'Wearing one of the pictures inside it — pick another, or take it away'
                : 'Give it a picture of something inside it',
              onSelect: () => setPicturing(true),
            },
            {
              key: 'rename',
              glyph: '✎',
              label: 'Rename',
              hint: 'Everything inside it comes along, and nothing is transferred',
              onSelect: () => setRenaming(true),
            },
            {
              key: 'move',
              glyph: '→',
              label: 'Move to another folder',
              hint: 'It takes everything inside it, and nothing is transferred',
              onSelect: () => setMoving(true),
            },
            {
              key: 'relocate',
              glyph: '⇄',
              label: 'Move to other clouds',
              hint: 'Everything inside it, and only the parts that have to move',
              onSelect: () => setRelocating(true),
            },
            assignTargets.length > 0 && {
              key: 'assign',
              glyph: '🔒',
              label: vault ? 'Move out of this sub vault' : 'Move into a sub vault',
              hint: vault
                ? 'The folder and everything in it, back into the main vault'
                : 'The folder and everything in it, at the path it already has',
              onSelect: () => setAssigning(true),
            },
            {
              key: 'delete',
              glyph: '✕',
              label: 'Delete folder',
              hint: 'Erases everything inside it, everywhere',
              danger: true,
              onSelect: () => setConfirming(true),
            },
          ]}
        />
      )}

      {replicating && (
        <ReplicateFolderDialog
          path={path}
          name={name}
          held={held}
          replicated={replicated}
          onClose={() => setReplicating(false)}
          onChanged={onRefresh}
        />
      )}

      {confirming && (
        <ConfirmDialog
          title={`Delete ${name}?`}
          subtitle={path}
          busy={busy}
          confirmLabel="Delete folder"
          onConfirm={remove}
          onClose={() => !busy && setConfirming(false)}
        >
          The folder and everything inside it goes: all parts are erased from every account,
          on the machine — you can keep working, and the folder leaves the listing when it is
          done. This cannot be undone.
        </ConfirmDialog>
      )}

      {relocating && (
        <RelocateClouds
          /* The vault travels with the path: two vaults can each have a folder
             of this name, and the server can only tell them apart by being
             told which tree the move was aimed at. */
          target={{ path, vault }}
          title={`Move ${name}`}
          subtitle={`Everything under ${path} — pick the clouds its parts should live on`}
          /* A folder has no placement of its own; its files each have one. So
             the dialog opens on nothing chosen and the preview says, as soon as
             there is something to price, how much of the folder is already
             where it is being sent. */
          current={[]}
          providers={providers}
          /* Offered where the folder is replicated, in its own right or by
             a folder above it. The estimate says how many files inside it
             actually have a replica to rebuild from. */
          replica={!vault && (replicated || !!replicate?.enabled)}
          onClose={() => setRelocating(false)}
          onDone={onRefresh}
        />
      )}

      {moving && (
        <MoveToFolder
          items={[{ kind: 'folder', name, path }]}
          vault={vault}
          onClose={() => setMoving(false)}
          onDone={onRefresh}
        />
      )}

      {assigning && (
        <AssignToVault
          label={name}
          target={path}
          from={vault}
          targets={assignTargets}
          folder
          onClose={() => setAssigning(false)}
          onDone={() => { setAssigning(false); onRefresh() }}
          onError={onError}
        />
      )}

      {picturing && (
        <FolderArtPicker
          path={path}
          name={name}
          vault={vault}
          onClose={() => setPicturing(false)}
          onDone={onRefresh}
        />
      )}

      {zipping && (
        <FolderZip
          path={path}
          name={name}
          vault={vault}
          onClose={() => setZipping(false)}
        />
      )}

      {sending && (
        <MachineTransfer
          path={path}
          vault={vault}
          mode="export"
          preset={[{ kind: 'folder', path, name }]}
          onClose={() => setSending(false)}
          onChanged={onRefresh}
        />
      )}

      {renaming && (
        <RenameDialog
          kind="folder"
          name={name}
          path={path}
          vault={vault}
          onClose={() => setRenaming(false)}
          onDone={onRefresh}
        />
      )}
    </>
  )

  return {
    open,
    dialogs,
    openMenu: () => setMenu(true),
    confirmDelete: () => setConfirming(true),
    relocate: () => setRelocating(true),
    moveTo: () => setMoving(true),
    rename: () => setRenaming(true),
    pickArt: () => setPicturing(true),
  }
}

export function FolderRow({
  name, path, location, art, mobile, providers, columns, selecting, selected,
  onSelect, onNavigate, onRefresh, onError,
  vault = '', subVaults = [], previousKey = 0, replicated = false, replicate = null,
}) {
  const a = useFolderActions({
    name, path, art, providers, vault, subVaults, replicated, replicate, onNavigate, onRefresh, onError,
  })

  const check = selecting && (
    <SelectBox mobile={mobile} checked={selected} label={`Select ${name}`} onChange={onSelect} />
  )

  const nameButton = (
    <NameButton
      mobile={mobile}
      /* The picture of something inside it, where the icon was — the same
         swap a file's row made when thumbnails arrived, and the same fall
         back to the icon when there is nothing to draw. */
      icon={<Thumb id={art?.id} icon="📁" size={mobile ? 34 : 26} expected={!!art?.id} />}
      label={name}
      location={location}
      chevron="▸"
      title="Open folder"
      onClick={a.open}
    />
  )

  const actions = mobile ? (
    <IconButton
      glyph="⋯"
      label={`Actions for ${name}`}
      onClick={a.openMenu}
      size={44}
      style={{ background: COLORS.surfaceRaised, border: `1px solid ${COLORS.border}`, fontSize: '18px' }}
    />
  ) : (
    <span style={{ display: 'flex', gap: '2px', justifyContent: 'flex-end' }}>
      {/* Only where there is something to choose from: an absent entry means
          nothing inside has a picture, so there is nothing to choose between. */}
      {art && (
        <IconButton
          glyph="🖼"
          label={`Choose the picture for ${name}`}
          title={art.id
            ? 'Which of the pictures inside it this folder is drawn with'
            : 'Give this folder a picture of something inside it'}
          tone={art.id ? 'dim' : 'muted'}
          onClick={a.pickArt}
          style={{ fontSize: '13px' }}
        />
      )}
      <IconButton
        glyph="→"
        label={`Move ${name} to another folder`}
        title="Move the folder, and everything in it, somewhere else in the vault"
        onClick={a.moveTo}
      />
      <IconButton
        glyph="⇄"
        label={`Move ${name} to other clouds`}
        title="Move every file in this folder to other clouds"
        onClick={a.relocate}
      />
      <IconButton glyph="✕" label="Delete folder" tone="muted" onClick={a.confirmDelete} />
      <IconButton glyph="⋯" label={`Actions for ${name}`} onClick={a.openMenu} />
    </span>
  )

  if (mobile) {
    return (
      <Row mobile check={check} selected={selected}>
        <div style={{ display: 'flex', alignItems: 'center', gap: '8px', minHeight: '48px' }}>
          {nameButton}
          {actions}
        </div>
        {a.dialogs}
      </Row>
    )
  }

  return (
    <Row check={check} columns={columns} selected={selected}>
      {nameButton}
      {/* The middle cells only exist to fill the grid — except the parts
          column, which carries how many files under this folder are still on
          the previous key, the folder's answer to the marker a file row wears. */}
      <span /><span />
      <span style={{ display: 'flex', alignItems: 'center', minWidth: 0 }}>
        {previousKey > 0 && (
          <span
            title={`${previousKey} file${previousKey === 1 ? '' : 's'} in this folder ${previousKey === 1 ? 'is' : 'are'} still sealed under the key your previous password protected`}
            style={{
              fontFamily: FONT.mono, fontSize: '9px', color: COLORS.textDim, whiteSpace: 'nowrap',
              border: `1px dashed ${COLORS.borderBright}`, borderRadius: '999px', padding: '1px 6px',
            }}
          >{previousKey} on prev key</span>
        )}
      </span>
      {actions}
      {a.dialogs}
    </Row>
  )
}

/* The shape of a tile's picture. A square suits a folder of photographs, which
   arrive in both orientations and crop to one about equally badly.

   A folder that has asked for film details gets two-by-three instead, because
   that is the shape every poster ever printed is, and a square crop of one eats
   the title band at the foot. It is the whole grid rather than the matched rows
   alone: one shape per view keeps the rows aligned, and a folder somebody has
   declared to be films is a folder whose stray photograph can afford the crop.
   Plex and Jellyfin land in the same place for the same reason. */
export const TILE_SQUARE = '1 / 1'
export const TILE_POSTER = '2 / 3'

/* The tile the two views really differ over: a picture with the name under it.
   Worth having because a folder of photographs or films is a folder whose file
   names say nothing at all — the thumbnail is the only part of the row anybody
   was reading. */
function Tile({ children, selected, check, menu }) {
  const [hover, setHover] = useState(false)

  return (
    <div
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      style={{
        position: 'relative',
        display: 'flex',
        flexDirection: 'column',
        borderRadius: '8px',
        overflow: 'hidden',
        background: selected ? `${COLORS.accent}1f` : COLORS.surface,
        border: `1px solid ${selected ? COLORS.accent : hover ? COLORS.borderBright : COLORS.border}`,
        transition: 'border-color 0.12s ease',
      }}
    >
      {children}
      {/* Both float over the picture rather than taking a line of their own:
          the tile is already as small as a thumbnail can usefully be. */}
      {check && (
        <span style={{
          position: 'absolute', top: '4px', left: '4px',
          borderRadius: '6px', background: 'rgba(10, 14, 23, 0.72)',
        }}>{check}</span>
      )}
      <span style={{ position: 'absolute', top: '4px', right: '4px' }}>{menu}</span>
    </div>
  )
}

function TileFace({ children, ...props }) {
  return (
    <button
      {...props}
      style={{
        display: 'block', width: '100%', padding: 0,
        background: 'none', border: 'none', textAlign: 'left',
        cursor: props.disabled ? 'not-allowed' : 'pointer',
        ...props.style,
      }}
    >{children}</button>
  )
}

/* A tile is about 130px across, which is room for a name and one line under it.
   So `location` — which only a search result has — takes a line of its own
   rather than fighting the size and the badges for that one, where all three
   would be reduced to an ellipsis apiece. */
function TileCaption({ name, title, meta, location, dead }) {
  return (
    <span style={{ display: 'block', padding: '7px 8px 8px' }}>
      <span title={title} style={{
        display: 'block',
        fontFamily: FONT.mono, fontSize: '11.5px',
        color: dead ? COLORS.error : COLORS.text,
        overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
      }}>{name}</span>
      <span style={{
        display: 'flex', alignItems: 'center', gap: '6px', marginTop: '4px',
        fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textMuted,
      }}>{meta}</span>
      {location && (
        <span
          title={location}
          style={{
            display: 'block', marginTop: '3px',
            fontFamily: FONT.mono, fontSize: '10px', color: COLORS.textMuted,
            overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
          }}
        >in {location}</span>
      )}
    </span>
  )
}

export function FileTile({
  file, location, mobile, providers, hasThumb, film, aspect = TILE_SQUARE,
  selecting, selected,
  onSelect, onPreview, onInspect, onFilm, onRefresh, onError,
  vault = '', subVaults = [], replicate = null,
}) {
  const a = useFileActions({
    file, providers, film, vault, subVaults, replicate, onPreview, onInspect, onFilm, onRefresh, onError,
  })
  const icon = fileIcon(file.mime, file.name)

  return (
    <Tile
      selected={selected}
      check={selecting && (
        <SelectBox mobile={mobile} checked={selected} label={`Select ${file.name}`} onChange={onSelect} />
      )}
      menu={(
        <IconButton
          glyph="⋯"
          label={`Actions for ${file.name}`}
          onClick={a.openMenu}
          size={mobile ? 44 : 30}
          style={{
            background: 'rgba(10, 14, 23, 0.72)',
            border: `1px solid ${COLORS.border}`,
            color: COLORS.text,
            fontSize: '16px',
          }}
        />
      )}
    >
      <TileFace onClick={a.open} disabled={a.dead} title={a.openTitle}>
        <span style={{
          display: 'flex', alignItems: 'center', justifyContent: 'center',
          width: '100%', aspectRatio: aspect, overflow: 'hidden',
          background: COLORS.bg, borderBottom: `1px solid ${COLORS.border}`,
        }}>
          <Thumb id={file.id} icon={icon} expected={hasThumb} fill />
        </span>
        {/* A matched film is captioned with the film's name rather than the
            file's. That is the whole point of a wall of posters: "Alien" over
            the poster, not "Alien.1979.2160p.UHD.BluRay.x265-GROUP.mkv". The
            file name is a breath away — the hover text here, and the subtitle
            of everything the tile opens. */}
        <TileCaption
          name={film?.title || file.name}
          title={film ? file.name : undefined}
          dead={a.dead}
          location={location}
          meta={(
            <>
              <span style={{ whiteSpace: 'nowrap' }}>
                {film?.year ? film.year : formatBytes(file.size)}
              </span>
              <span
                title={describeParts(file)}
                style={{ marginLeft: 'auto', display: 'flex', gap: '3px', flexShrink: 0 }}
              ><PartBadges file={file} /></span>
            </>
          )}
        />
      </TileFace>
      {a.dialogs}
    </Tile>
  )
}

export function FolderTile({
  name, path, location, art, mobile, providers, aspect = TILE_SQUARE, selecting, selected,
  onSelect, onNavigate, onRefresh, onError,
  vault = '', subVaults = [], replicated = false, replicate = null,
}) {
  const a = useFolderActions({
    name, path, art, providers, vault, subVaults, replicated, replicate, onNavigate, onRefresh, onError,
  })

  return (
    <Tile
      selected={selected}
      check={selecting && (
        <SelectBox mobile={mobile} checked={selected} label={`Select ${name}`} onChange={onSelect} />
      )}
      menu={(
        <IconButton
          glyph="⋯"
          label={`Actions for ${name}`}
          onClick={a.openMenu}
          size={mobile ? 44 : 30}
          style={{
            background: 'rgba(10, 14, 23, 0.72)',
            border: `1px solid ${COLORS.border}`,
            color: COLORS.text,
            fontSize: '16px',
          }}
        />
      )}
    >
      <TileFace onClick={a.open} title="Open folder">
        {/* The whole point of the tile, for a folder that has been given one: a
            folder of films drawn as one of its posters rather than as the same
            📁 as every other folder. */}
        <span style={{
          display: 'flex', alignItems: 'center', justifyContent: 'center',
          width: '100%', aspectRatio: aspect, overflow: 'hidden', fontSize: '34px',
          background: COLORS.bg, borderBottom: `1px solid ${COLORS.border}`,
        }}>
          <Thumb id={art?.id} icon="📁" expected={!!art?.id} fill />
        </span>
        <TileCaption name={name} location={location} meta={<span>Folder</span>} />
      </TileFace>
      {a.dialogs}
    </Tile>
  )
}

/* Moving something from one vault inside the file into another.

   The move itself is instant — the index changes and the parts stay exactly
   where they are — so this dialog is short. What it has to be honest about is
   the re-encryption running behind it: until that finishes, the vault the file
   came from can still read it, which is the same window a password change has. */
function AssignToVault({ label, target, from, targets, folder, onClose, onDone, onError }) {
  const [to, setTo] = useState(targets[0]?.id ?? '')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  const destination = targets.find((t) => t.id === to)

  const submit = async (e) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      const report = await api.assign({ target, from, to })
      if (report.warnings?.length) onError(report.warnings.join('\n'))
      /* The move has landed; the re-encryption onto the destination's own key
         runs behind it. Not awaited — that is what keeps assigning a folder of
         films instant — but it has to be started, because until it finishes the
         file can only be read while the vault it left is open. */
      api.migrateVault(to).catch(() => {})
      onDone()
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }

  return (
    <Modal
      title={from ? `Move ${label} out` : `Move ${label} into a sub vault`}
      subtitle={folder ? 'The folder and everything under it' : null}
      onClose={() => !busy && onClose()}
      width={440}
      zIndex={120}
    >
      <form onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}

        <p style={{
          fontFamily: FONT.mono, fontSize: '10px', lineHeight: 1.6,
          color: COLORS.textDim, margin: '0 0 12px',
        }}>
          Nothing is uploaded or downloaded — the path is kept and the parts stay
          where they are. The files are re-encrypted onto the destination’s own key
          afterwards; until that finishes, the vault it is leaving can still read
          them.
        </p>

        {targets.length > 1 ? (
          <select
            value={to}
            onChange={(e) => setTo(e.target.value)}
            style={{
              width: '100%', padding: '10px 12px', marginBottom: '14px',
              background: COLORS.bg, border: `1px solid ${COLORS.border}`, borderRadius: '6px',
              color: COLORS.text, fontFamily: FONT.mono, fontSize: '13px',
            }}
          >
            {targets.map((t) => <option key={t.id} value={t.id}>{t.label}</option>)}
          </select>
        ) : (
          <p style={{ fontFamily: FONT.mono, fontSize: '11px', color: COLORS.text, margin: '0 0 14px' }}>
            Into <strong>{destination?.label}</strong>.
          </p>
        )}

        <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end' }}>
          <Button type="button" variant="ghost" onClick={onClose} disabled={busy}>Cancel</Button>
          <Button type="submit" variant="primary" disabled={busy}>{busy ? 'Moving…' : 'Move'}</Button>
        </div>
      </form>
    </Modal>
  )
}
