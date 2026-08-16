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

/* The one control that reaches past the folder you are standing in. The index
   it queries only exists in the open vault, so this is also the only thing in
   the app that can answer "where did I put that?" at all. */
export function SearchField({ value, busy, mobile, onChange }) {
  return (
    <div style={{
      position: 'relative',
      display: 'flex',
      alignItems: 'center',
      // On a phone the field takes a row of its own, above the two actions.
      flex: mobile ? '1 0 100%' : '1 1 220px',
      minWidth: mobile ? '100%' : '150px',
    }}>
      <span style={{
        position: 'absolute', left: mobile ? '11px' : '9px', color: COLORS.textMuted,
        fontSize: '13px', pointerEvents: 'none',
      }}>⌕</span>
      <input
        type="search"
        value={value}
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

      {sorting && (
        <ActionSheet
          title="Sort by"
          subtitle="Folders lead whichever column is chosen — a folder in the index carries a name and nothing else to sort on."
          onClose={() => setSorting(false)}
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
      )}
    </div>
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
