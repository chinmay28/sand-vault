import React, { useEffect, useState } from 'react'
import { COLORS, FONT, accountColor, fileIcon, formatBytes, formatDate, isPlayable } from '../theme'
import { api } from '../api'
import { useDownload } from '../download'
import { ActionSheet, ConfirmDialog, IconButton } from './ui'
import StreamLink from './StreamLink'
import ConvertFile from './ConvertFile'
import { RelocateClouds, fileScheme, schemeName, storedParts } from './CloudSelect'
import MoveToFolder from './MoveToFolder'
import FolderArtPicker from './FolderArt'

/* One file or one folder, drawn as a row or as a tile.

   The two views differ in shape and in nothing else: the same file offers the
   same things to do with it either way. So everything that is not layout —
   which dialogs a file can open, what it is allowed to do in the state it is
   in, what the menu says — lives in useFileActions below, and a row and a tile
   are two arrangements of what it hands back. */

/* Name, size, modified, parts, actions. The four fixed columns come to nearly
   550px, which is why the phone layout stacks instead of shrinking them. */
export const COLUMNS = 'minmax(0,1fr) 92px 150px 132px 144px'

/* The same again in a folder that has asked for film details, where a video
   row carries one control more. The column grows by exactly one button rather
   than the ones already there being crowded together — and it grows for the
   whole table, because a heading has to sit over the column it names. */
export const FILM_COLUMNS = 'minmax(0,1fr) 92px 150px 132px 180px'

/* Everything a file row or tile can do, and every dialog it can open.

   Held in one place because a file's state decides most of it: a file stored
   before chunking cannot be opened or streamed until it is converted, and one
   down to a single part cannot be rebuilt at all. Working that out twice —
   once for rows and once for tiles — is how the two views drift apart. */
export function useFileActions({ file, providers, film, onPreview, onInspect, onFilm, onRefresh, onError }) {
  const [busy, setBusy] = useState(false)
  const [download, downloading] = useDownload(onError)
  const [menu, setMenu] = useState(false)
  const [confirming, setConfirming] = useState(false)
  /* null, or 'play' when the stream dialog should reach for VLC on the way in
     and 'link' when it should just show the address. */
  const [streaming, setStreaming] = useState(null)
  const [converting, setConverting] = useState(null)
  const [relocating, setRelocating] = useState(false)
  const [moving, setMoving] = useState(false)

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

  const remove = async () => {
    setBusy(true)
    try {
      const resp = await api.deleteFile(file.id)
      if (resp.warnings?.length) onError(resp.warnings.join('\n'))
      setConfirming(false)
      onRefresh()
    } catch (err) {
      onError(err.message)
      setConfirming(false)
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
              label: downloading ? 'Downloading…' : 'Download',
              hint: 'Save the rebuilt, decrypted file',
              disabled: downloading,
              onSelect: () => download(file),
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
            /* The two moves, next to each other and named for what they move
               it onto. This one is free and instant whatever the file weighs;
               the one under it copies bytes between clouds. */
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
          Every part is erased from the accounts holding it. This cannot be undone.
        </ConfirmDialog>
      )}

      {relocating && (
        <RelocateClouds
          target={{ id: file.id }}
          title={`Move ${file.name}`}
          subtitle={`${formatBytes(file.size)} — ${schemeName(fileScheme(file))}; pick the clouds its shards should live on`}
          /* Already selected: where the parts are now, so the dialog opens on
             the truth and a swap is one click rather than four. */
          current={file.shards.map((s) => s.provider_id)}
          providers={providers}
          onClose={() => setRelocating(false)}
          onDone={onRefresh}
        />
      )}

      {moving && (
        <MoveToFolder
          items={[{ kind: 'file', name: file.name, file }]}
          onClose={() => setMoving(false)}
          onDone={onRefresh}
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

/* The picture in front of a file's name. It is a stored thumbnail — a small
   JPEG the vault keeps a folder at a time — so drawing one costs nothing like
   rebuilding the file it came from.

   `size` is the edge in pixels: 52 on a phone, where the row is a stack and
   the tile is the left column of it, and 26 on a desktop, where it stands in
   for the emoji inside the Name column without changing the row's height. In
   the grid it is `fill` instead, and the picture is the tile.

   It falls back to that same emoji, and does so on any failure — a file
   uploaded before thumbnails existed, an account that has gone quiet, a pack
   that could not be read. The list has always been readable without pictures. */
export function Thumb({ id, icon, size, expected, fill }) {
  const [failed, setFailed] = useState(false)

  // A new file in the same row position must not inherit the old one's state.
  useEffect(() => { setFailed(false) }, [id])

  if (!expected || failed) {
    return (
      <span style={{ flexShrink: 0, fontSize: fill ? '34px' : size >= 40 ? '26px' : '15px' }}>{icon}</span>
    )
  }

  return (
    <img
      src={api.thumbURL(id)}
      alt=""
      width={fill ? undefined : size}
      height={fill ? undefined : size}
      loading="lazy"
      decoding="async"
      onError={() => setFailed(true)}
      style={fill ? {
        width: '100%', height: '100%', objectFit: 'cover', display: 'block',
      } : {
        width: `${size}px`,
        height: `${size}px`,
        flexShrink: 0,
        objectFit: 'cover',
        borderRadius: size >= 40 ? '6px' : '4px',
        background: COLORS.surfaceRaised,
        border: `1px solid ${COLORS.border}`,
      }}
    />
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
  return `${schemeName(scheme)} · ${storedParts(file.shards)} of ${scheme.total} shards stored`
}

/* Which account holds each of the file's shards, as one coloured square each.

   A 2-of-3 file draws three and a 6-of-9 file draws nine, so the row says at a
   glance how widely the file is spread as well as whether anything is missing.
   The squares narrow past three so that nine of them still fit the column. */
function PartBadges({ file, mobile }) {
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
              /* The badges share the phone's second line with the size and
                 the date now that the picture has taken the left column, so
                 they are the desktop's width there too — that is the
                 difference between reading the date and truncating it. */
              width: tight ? '12px' : '19px',
              height: mobile ? '16px' : '15px',
              borderRadius: '3px',
              fontFamily: FONT.mono,
              fontSize: tight ? '7.5px' : (mobile ? '9.5px' : '8.5px'),
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
    </>
  )
}

export function FileRow({
  file, location, mobile, providers, hasThumb, film, columns, selecting, selected,
  onSelect, onPreview, onInspect, onFilm, onRefresh, onError,
}) {
  const a = useFileActions({ file, providers, film, onPreview, onInspect, onFilm, onRefresh, onError })
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

  /* On a phone the badges are a read-out and nothing more. A third target in a
     row that already has a name and a menu would have to be either too small
     to hit or tall enough to push the next file off the screen — and the menu
     already offers the same inspector by name. On a desktop the badges stay
     the shortcut they have always been. */
  const parts = mobile ? (
    <span
      title={describeParts(file)}
      style={{ display: 'flex', alignItems: 'center', gap: '4px', flexShrink: 0 }}
    ><PartBadges file={file} mobile /></span>
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
    </span>
  )

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

              {/* This line carries the part badges as well as the size and the
                  date, and on a 390px screen it has 224px to do it in. Hence
                  no "·" separator, unlike everywhere else, and hence the
                  badges are pushed right with a margin rather than a spacer
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
                <span style={{ marginLeft: 'auto', display: 'flex', flexShrink: 0 }}>{parts}</span>
              </span>
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

/* Everything a folder row or tile can do. Far shorter than a file's, because a
   folder is a name in the index rather than something stored: it can be walked
   into, moved onto other clouds wholesale, or deleted with what is inside it. */
function useFolderActions({ name, path, art, providers, onNavigate, onRefresh, onError }) {
  const [menu, setMenu] = useState(false)
  const [confirming, setConfirming] = useState(false)
  const [relocating, setRelocating] = useState(false)
  const [moving, setMoving] = useState(false)
  const [picturing, setPicturing] = useState(false)
  const [busy, setBusy] = useState(false)

  const open = () => onNavigate(path)

  const remove = async () => {
    setBusy(true)
    try {
      const resp = await api.deleteFolder(path, true)
      if (resp.warnings?.length) onError(resp.warnings.join('\n'))
      setConfirming(false)
      onRefresh()
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
          onClose={() => setMenu(false)}
          items={[
            { key: 'open', glyph: '▸', label: 'Open folder', onSelect: open },
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

      {confirming && (
        <ConfirmDialog
          title={`Delete ${name}?`}
          subtitle={path}
          busy={busy}
          confirmLabel="Delete folder"
          onConfirm={remove}
          onClose={() => !busy && setConfirming(false)}
        >
          The folder and everything inside it goes: all parts are erased from every account. This cannot be undone.
        </ConfirmDialog>
      )}

      {relocating && (
        <RelocateClouds
          target={{ path }}
          title={`Move ${name}`}
          subtitle={`Everything under ${path} — pick the clouds its parts should live on`}
          /* A folder has no placement of its own; its files each have one. So
             the dialog opens on nothing chosen and the preview says, as soon as
             there is something to price, how much of the folder is already
             where it is being sent. */
          current={[]}
          providers={providers}
          onClose={() => setRelocating(false)}
          onDone={onRefresh}
        />
      )}

      {moving && (
        <MoveToFolder
          items={[{ kind: 'folder', name, path }]}
          onClose={() => setMoving(false)}
          onDone={onRefresh}
        />
      )}

      {picturing && (
        <FolderArtPicker
          path={path}
          name={name}
          onClose={() => setPicturing(false)}
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
    pickArt: () => setPicturing(true),
  }
}

export function FolderRow({
  name, path, location, art, mobile, providers, columns, selecting, selected,
  onSelect, onNavigate, onRefresh, onError,
}) {
  const a = useFolderActions({ name, path, art, providers, onNavigate, onRefresh, onError })

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
      {/* The three empty middle cells only exist to fill the grid. */}
      <span /><span /><span />
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
}) {
  const a = useFileActions({ file, providers, film, onPreview, onInspect, onFilm, onRefresh, onError })
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
}) {
  const a = useFolderActions({ name, path, art, providers, onNavigate, onRefresh, onError })

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
