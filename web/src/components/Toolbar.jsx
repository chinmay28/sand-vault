import React, { useState } from 'react'
import { COLORS, FONT } from '../theme'
import { SORT_KEYS, naturalDirection, sortDirectionLabel } from '../view'
import { ActionSheet, Button, IconButton, Spinner } from './ui'

/* Everything above the listing: where you are, how you got there, how it is
   drawn, and what a handful of picked rows can be told to do.

   A file browser is walked rather than read, so the controls that move you
   around are the ones that have to be in reach at all times — which is why
   they lead the toolbar on a phone as well as on a desk, ahead of search and
   ahead of uploading. */

/* Back, Forward and Up. Sized to the fingertip on a phone, because a disabled
   button is still a button somebody will aim at. */
export function NavCluster({ nav, mobile }) {
  const size = mobile ? 44 : 32

  const step = (glyph, label, where, enabled, onClick, hint) => (
    <IconButton
      glyph={glyph}
      label={label}
      title={enabled ? `${label} — ${where}` : hint}
      size={size}
      disabled={!enabled}
      onClick={onClick}
      style={{
        fontSize: mobile ? '15px' : '13px',
        opacity: enabled ? 1 : 0.32,
        cursor: enabled ? 'pointer' : 'default',
      }}
    />
  )

  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: mobile ? '2px' : '1px', flexShrink: 0 }}>
      {step('◀', 'Back', nav.behind, nav.canBack, nav.back, 'Nowhere to go back to yet')}
      {step('▶', 'Forward', nav.ahead, nav.canForward, nav.forward, 'Nowhere to go forward to')}
      {step('▲', 'Up', nav.parent, nav.canUp, nav.up, 'Already at the top of the vault')}
    </div>
  )
}

/* The trail. Every crumb is a folder you can land on, and the first one is the
   root of the vault. */
export function Breadcrumbs({ path, mobile, onNavigate }) {
  const segments = path === '/' ? [] : path.slice(1).split('/')

  return (
    <nav
      aria-label="Folder trail"
      style={{
        flex: 1, minWidth: mobile ? '100%' : '160px', display: 'flex', alignItems: 'center',
        gap: '5px', fontFamily: FONT.mono, fontSize: '12px', flexWrap: 'wrap',
      }}
    >
      <Crumb label="▣ /" mobile={mobile} onClick={() => onNavigate('/')} active={path === '/'} />
      {segments.map((segment, i) => {
        const target = '/' + segments.slice(0, i + 1).join('/')
        return (
          <React.Fragment key={target}>
            <span style={{ color: COLORS.textMuted }}>/</span>
            <Crumb label={segment} mobile={mobile} onClick={() => onNavigate(target)} active={i === segments.length - 1} />
          </React.Fragment>
        )
      })}
    </nav>
  )
}

function Crumb({ label, mobile, onClick, active }) {
  return (
    <button
      onClick={onClick}
      style={{
        background: 'none',
        border: 'none',
        // Walking back up the tree is a tap like any other, so the trail gets
        // room to be tapped rather than being treated as decoration.
        minHeight: mobile ? '44px' : 0,
        minWidth: mobile ? '44px' : 0,
        padding: mobile ? '4px 10px' : '2px 4px',
        borderRadius: '6px',
        cursor: 'pointer',
        fontFamily: FONT.mono,
        fontSize: mobile ? '13px' : '12px',
        color: active ? COLORS.accent : COLORS.textDim,
        fontWeight: active ? 700 : 400,
      }}
    >{label}</button>
  )
}

/* The phone's toolbar: a heading for the folder you are standing in, and a
   strip of everything you can do to it.

   The desk's row of arrows, trail and view icons does not survive a 390px
   screen — half of it is empty, the trail gets a line to itself to render a
   slash, and none of the six icons sharing the top line is worth aiming at. The
   answer is not to squeeze that row but to put something in it worth the space:
   the folder's name, and under it the one line only this app can write. A file
   here is three encrypted parts on three different accounts, so "23 files ·
   1.4 GB · 3 clouds" is the vault saying what it is holding and where — which
   is the thing a file list cannot tell you, and the reason the empty band above
   the listing was worth filling rather than shrinking.

   Uploading is the one action set beside the heading, because it is the one
   people came to do. The rest — search, the view, the order, selecting, and a
   new folder — sit on a hairline strip below it, each at a full target.

   Walking back up is the heading itself: tapping it drops the trail as a list,
   which is readable four folders deep where crumbs are not, and carries
   Forward when there is somewhere to go forward to. */
export function FolderHeader({
  nav, path, stats, prefs, view, selecting, canUpload, search,
  onSelecting, onSearch, onNewFolder, onUpload,
}) {
  const [jumping, setJumping] = useState(false)
  const [sorting, setSorting] = useState(false)

  const segments = path === '/' ? [] : path.slice(1).split('/')
  const here = segments.length ? segments[segments.length - 1] : 'Vault'
  const grid = prefs.view === 'grid'
  // Nothing to drop at the root of an unwalked vault: no folders above this
  // one, and nowhere forward to go either.
  const canJump = segments.length > 0 || nav.canForward

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '8px' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: '6px' }}>
        <IconButton
          glyph="◀"
          label="Back"
          title={nav.canBack ? `Back — ${nav.behind}` : 'Nowhere to go back to yet'}
          size={44}
          disabled={!nav.canBack}
          onClick={nav.back}
          style={{
            fontSize: '16px',
            opacity: nav.canBack ? 1 : 0.32,
            cursor: nav.canBack ? 'pointer' : 'default',
          }}
        />

        <button
          onClick={() => canJump && setJumping(true)}
          disabled={!canJump}
          aria-label={canJump ? `${here} — open the folder trail` : 'The root of the vault'}
          title={path}
          style={{
            flex: 1,
            minWidth: 0,
            display: 'flex',
            flexDirection: 'column',
            alignItems: 'flex-start',
            gap: '3px',
            minHeight: '44px',
            justifyContent: 'center',
            padding: '2px 2px',
            background: 'none',
            border: 'none',
            cursor: canJump ? 'pointer' : 'default',
            textAlign: 'left',
          }}
        >
          <span style={{
            display: 'flex', alignItems: 'center', gap: '7px',
            maxWidth: '100%', minWidth: 0,
          }}>
            <span aria-hidden="true" style={{ fontSize: '14px', color: COLORS.accent, flexShrink: 0 }}>▣</span>
            <span style={{
              minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
              fontFamily: FONT.mono, fontSize: '17px', fontWeight: 700, color: COLORS.text,
            }}>{here}</span>
            {canJump && (
              <span aria-hidden="true" style={{ fontSize: '10px', color: COLORS.textMuted, flexShrink: 0 }}>▾</span>
            )}
          </span>
          {/* Blank until the index has been read, rather than a zero that would
              be wrong for the moment it is on screen. */}
          {stats && (
            <span style={{
              maxWidth: '100%', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
              fontFamily: FONT.mono, fontSize: '10.5px', color: COLORS.textMuted,
            }}>
              {stats.summary}
              {stats.clouds > 0 && (
                <span style={{ color: COLORS.accent }}> · {stats.clouds} cloud{stats.clouds === 1 ? '' : 's'}</span>
              )}
            </span>
          )}
        </button>

        <Button
          size="md"
          variant="primary"
          onClick={onUpload}
          disabled={!canUpload}
          title={canUpload ? `Upload into ${path}` : 'Connect a cloud account first'}
          style={{ flexShrink: 0, minHeight: '44px', fontSize: '13px' }}
        >↑ Upload</Button>
      </div>

      {/* Everything else, at a full target each. Set against the listing by a
          hairline that runs the width of the screen rather than a box, so the
          strip reads as the floor of the heading and not a second toolbar.

          Searching happens here rather than over the heading: the field takes
          the strip and the folder stays named above it, which is the whole
          question a result answers — "4 matches for IMG" means nothing without
          somewhere it was asked from. */}
      <div style={{
        display: 'flex',
        alignItems: 'center',
        gap: search ? '6px' : '2px',
        margin: '0 -12px -10px',
        padding: search ? '6px 12px' : '1px 8px',
        borderTop: `1px solid ${COLORS.border}`,
      }}>
        {search || <>
        <IconButton
          glyph="⌕"
          label="Search"
          title="Search files and folders"
          size={44}
          onClick={onSearch}
          style={{ fontSize: '22px' }}
        />
        <IconButton
          glyph={grid ? '▤' : '▦'}
          label={grid ? 'Show as a list' : 'Show as a grid'}
          size={44}
          onClick={() => view.setView(grid ? 'list' : 'grid')}
          style={{ fontSize: '18px' }}
        />
        <IconButton
          glyph="⇅"
          label="Sort"
          title={`Sorted by ${SORT_KEYS.find((s) => s.key === prefs.key)?.label.toLowerCase()} — ${sortDirectionLabel(prefs.key, prefs.dir).toLowerCase()}`}
          size={44}
          onClick={() => setSorting(true)}
          style={{ fontSize: '17px' }}
        />
        <IconButton
          glyph="✓"
          label={selecting ? 'Stop selecting' : 'Select files and folders'}
          size={44}
          onClick={() => onSelecting(!selecting)}
          style={{
            fontSize: '17px',
            // A mode rather than an action, so it says which one it is in.
            color: selecting ? COLORS.accent : undefined,
          }}
        />

        <span style={{ flex: 1 }} />

        <IconButton
          glyph="＋"
          label="New folder"
          title={`New folder inside ${path}`}
          size={44}
          onClick={onNewFolder}
          style={{ fontSize: '17px' }}
        />
        </>}
      </div>

      {jumping && (
        <ActionSheet
          title="Go to"
          subtitle="Every folder between the root of the vault and where you are standing."
          onClose={() => setJumping(false)}
          items={[
            nav.canForward && {
              key: 'forward',
              glyph: '▶',
              label: 'Forward',
              hint: nav.ahead,
              onSelect: nav.forward,
            },
            { key: '/', glyph: '▣', label: 'Vault', hint: 'The root', onSelect: () => nav.navigate('/') },
            ...segments.map((segment, i) => {
              const target = '/' + segments.slice(0, i + 1).join('/')
              const current = i === segments.length - 1
              return {
                key: target,
                glyph: current ? '◉' : '▸',
                label: segment,
                hint: current ? 'Where you are now' : target,
                disabled: current,
                onSelect: () => nav.navigate(target),
              }
            }),
          ]}
        />
      )}

      {sorting && <SortSheet prefs={prefs} view={view} onClose={() => setSorting(false)} />}
    </div>
  )
}

/* The one control that reaches past the folder you are standing in. The index
   it queries only exists in the open vault, so this is also the only thing in
   the app that can answer "where did I put that?" at all. */
export function SearchField({ value, busy, mobile, autoFocus, onChange }) {
  return (
    <div style={{
      position: 'relative',
      display: 'flex',
      alignItems: 'center',
      // On a phone the field is the toolbar while it is open, so it takes
      // whatever the row has left after the button that closes it — not the
      // whole width, which would put that button underneath its own clear ✕.
      flex: mobile ? '1 1 0' : '1 1 220px',
      minWidth: mobile ? 0 : '150px',
    }}>
      <span style={{
        position: 'absolute', left: mobile ? '11px' : '9px', color: COLORS.textMuted,
        fontSize: '13px', pointerEvents: 'none',
      }}>⌕</span>
      <input
        type="search"
        value={value}
        autoFocus={autoFocus}
        aria-label="Search files and folders"
        placeholder="Search files and folders"
        onChange={(e) => onChange(e.target.value)}
        onKeyDown={(e) => { if (e.key === 'Escape') onChange('') }}
        style={{
          width: '100%',
          // Tall enough to be a target in its own right on a phone, where the
          // clear button beside it also has to clear 44px.
          minHeight: mobile ? '46px' : 0,
          padding: mobile ? '8px 48px 8px 30px' : '6px 28px 6px 24px',
          background: COLORS.bg,
          border: `1px solid ${COLORS.border}`,
          borderRadius: '6px',
          color: COLORS.text,
          fontFamily: FONT.mono,
          fontSize: mobile ? '13px' : '12px',
          outline: 'none',
          boxSizing: 'border-box',
          // Otherwise Safari renders a search field as its own rounded pill
          // and ignores most of the above. (Its clear button is dropped in
          // App.jsx, which is where a pseudo-element can be reached.)
          WebkitAppearance: 'none',
        }}
      />
      {busy && (
        <span style={{ position: 'absolute', right: mobile ? '14px' : '9px', display: 'flex' }}>
          <Spinner size={mobile ? 13 : 11} />
        </span>
      )}
      {!busy && value && (
        <span style={{ position: 'absolute', right: mobile ? '2px' : '4px', display: 'flex' }}>
          <IconButton
            glyph="✕"
            label="Clear the search"
            tone="muted"
            size={mobile ? 44 : 20}
            onClick={() => onChange('')}
            style={{ fontSize: mobile ? '13px' : '11px' }}
          />
        </span>
      )}
    </div>
  )
}

/* How the listing is drawn: rows or tiles, in what order, and whether rows can
   be picked. Three buttons rather than three menus — the view is a toggle, the
   order opens a sheet because it is two questions at once (which column, which
   way round), and picking is a mode you are either in or not. */
export function ViewControls({ mobile, prefs, view, selecting, onSelecting }) {
  const [sorting, setSorting] = useState(false)
  const size = mobile ? 44 : 32
  const grid = prefs.view === 'grid'

  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: mobile ? '2px' : '1px', flexShrink: 0 }}>
      <IconButton
        glyph={grid ? '▤' : '▦'}
        label={grid ? 'Show as a list' : 'Show as a grid'}
        size={size}
        onClick={() => view.setView(grid ? 'list' : 'grid')}
        style={{ fontSize: mobile ? '16px' : '14px' }}
      />
      <IconButton
        glyph={prefs.dir === 'asc' ? '↑' : '↓'}
        label="Sort"
        title={`Sorted by ${SORT_KEYS.find((s) => s.key === prefs.key)?.label.toLowerCase()} — ${sortDirectionLabel(prefs.key, prefs.dir).toLowerCase()}`}
        size={size}
        onClick={() => setSorting(true)}
        style={{ fontSize: mobile ? '15px' : '13px' }}
      />
      {/* ✓ rather than a ballot box: a glyph a system font is certain to have,
          which the tidier ☑ turns out not to be everywhere. */}
      <IconButton
        glyph="✓"
        label={selecting ? 'Stop selecting' : 'Select files and folders'}
        size={size}
        onClick={() => onSelecting(!selecting)}
        style={{
          fontSize: mobile ? '16px' : '14px',
          // The one control here that is a mode rather than an action, so it
          // says out loud which one it is in.
          background: selecting ? COLORS.accent : undefined,
          borderColor: selecting ? COLORS.accent : undefined,
          color: selecting ? COLORS.bg : undefined,
        }}
      />

      {sorting && <SortSheet prefs={prefs} view={view} onClose={() => setSorting(false)} />}
    </div>
  )
}

/* Which column, and which way round — two questions, so a sheet rather than a
   menu. Opened from the desk's ⇅ button and from the phone's ⋯ alike. */
function SortSheet({ prefs, view, onClose }) {
  return (
    <ActionSheet
      title="Sort by"
      subtitle="Folders lead whichever column is chosen — a folder in the index carries a name and nothing else to sort on."
      onClose={onClose}
      items={SORT_KEYS.map((spec) => ({
        key: spec.key,
        glyph: prefs.key === spec.key ? (prefs.dir === 'asc' ? '↑' : '↓') : '·',
        label: spec.label,
        hint: prefs.key === spec.key
          ? `${sortDirectionLabel(spec.key, prefs.dir)} — choose again to reverse it`
          // What choosing it would do, which is its natural direction
          // rather than whichever way the current column happens to face.
          : sortDirectionLabel(spec.key, naturalDirection(spec.key)),
        // The sheet stays open when the same column is chosen again, so
        // reversing the order is one tap and the arrow moves under it.
        keepOpen: prefs.key === spec.key,
        onSelect: () => view.setSort(spec.key),
      }))}
    />
  )
}

/* What is picked, and what can be done with it. Only on screen while something
   is being selected, which is why it can afford to be a whole row of its own. */
export function SelectionBar({
  mobile, count, total, files, allSelected, busy,
  onAll, onNone, onDone, onDownload, onMove, onDelete,
}) {
  const nothing = count === 0

  return (
    <div style={{
      display: 'flex',
      alignItems: 'center',
      gap: mobile ? '6px' : '10px',
      flexWrap: 'wrap',
      padding: mobile ? '8px 12px' : '9px 20px',
      background: `${COLORS.accent}14`,
      borderBottom: `1px solid ${COLORS.border}`,
    }}>
      <span style={{
        fontFamily: FONT.mono, fontSize: mobile ? '12px' : '11.5px', color: COLORS.text,
        flexShrink: 0,
      }}>
        {nothing ? 'Nothing selected' : `${count} of ${total} selected`}
      </span>

      <Button
        size={mobile ? 'md' : 'sm'}
        variant="ghost"
        onClick={allSelected ? onNone : onAll}
        disabled={total === 0}
      >{allSelected ? 'Select none' : 'Select all'}</Button>

      <span style={{ flex: 1, minWidth: mobile ? 0 : '8px' }} />

      {/* Only files can be handed back as files. A folder in the selection is
          not an error — it is simply not part of what this button does, and
          the count says so rather than the button silently skipping it. */}
      <Button
        size={mobile ? 'md' : 'sm'}
        onClick={onDownload}
        disabled={busy || files === 0}
        title={files === 0 ? 'Downloading is for files; folders are not rebuilt as one' : `Download ${files} file(s)`}
      >↓ Download{files > 1 ? ` ${files}` : ''}</Button>
      <Button
        size={mobile ? 'md' : 'sm'}
        onClick={onMove}
        disabled={busy || nothing}
        title="Move the parts of everything selected onto other clouds"
      >⇄ Move</Button>
      <Button
        size={mobile ? 'md' : 'sm'}
        variant="danger"
        onClick={onDelete}
        disabled={busy || nothing}
        title="Erase everything selected, everywhere"
      >✕ Delete</Button>
      <Button size={mobile ? 'md' : 'sm'} variant="ghost" onClick={onDone}>Done</Button>
    </div>
  )
}
