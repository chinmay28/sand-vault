import React, { useEffect, useMemo, useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { api } from '../api'
import { useIsMobile } from '../hooks'
import { Banner, Button, Modal, Spinner } from './ui'
import { BulkDelete } from './BulkActions'

/* The copies of things, found.

   A vault fills up the way a drawer does. The same photograph arrives from the
   phone and again from a camera-roll export; a folder is copied "just in case"
   before something is tried on it; a download that stalled is fetched again and
   the browser calls the second one "report (1).pdf". None of it shows in a
   listing, because the copies are never side by side — which is exactly why
   they survived.

   So the question is asked three ways, and all three are on screen at once
   because they are three different degrees of certainty about the same thing:

     · Identical      — one SHA-256, which is one file. Proof, not a guess.
     · Same size      — the same number of bytes, which is what somebody
                        actually says out loud, and which catches the pair whose
                        hash the vault never recorded. Weakest of the three.
     · Similar names  — names a copy marker, a separator or a typo apart. The
                        one that finds IMG_0001.jpg beside IMG_0001 (1).jpg, and
                        the one that can be wrong.

   Nothing here is ever only a guess, though, because a name group still says
   whether the bytes underneath it agree — a group marked "identical" is proven
   whichever question found it. That is the whole reason the three arrive
   together from one read: switching between them is how you use this, and a
   size match worth a second look should not cost another walk of the index.

   And nothing here deletes anything by itself. What is ticked goes to the same
   confirmation the delete button uses, one file at a time, or to the selection
   bar to be moved, downloaded or scattered instead — which is usually the
   better answer, because a copy you are not sure about belongs in a folder
   called something else rather than in no folder at all. */

/* Which of the three, and what is in each. Buttons rather than a dropdown: the
   counts on them are half the answer, and reading all three at once is the
   point. */
const WAYS = [
  {
    key: 'content',
    label: 'Identical',
    hint: 'The same bytes — one SHA-256, whatever they are called and wherever they sit. This is proof rather than a resemblance.',
  },
  {
    key: 'size',
    label: 'Same size',
    hint: 'The same number of bytes. Every group says whether the hashes back it up; the ones that do are in Identical too, and the ones that do not are two different files that happen to weigh the same.',
  },
  {
    key: 'name',
    label: 'Similar names',
    hint: 'Names a copy marker, a separator or a typo apart. Numbers have to match exactly — IMG_0001 and IMG_0002 are two photographs, not two copies — and an extension is never crossed, so a film is never grouped with its subtitles.',
  },
]

export function DuplicatesTool({ path, vault, onClose, onDone, onSelect }) {
  const [found, setFound] = useState(null)
  const [error, setError] = useState(null)

  useEffect(() => {
    let live = true
    api.duplicates(path, vault)
      .then((resp) => { if (live) setFound(resp) })
      .catch((err) => { if (live) setError(err.message) })
    return () => { live = false }
  }, [path, vault])

  if (error || !found) {
    return (
      <Modal title="Find duplicates" subtitle={path} onClose={onClose} width={560}>
        {error
          ? <Banner tone="error">{error}</Banner>
          : <div style={{ padding: '28px', textAlign: 'center' }}><Spinner size={18} /></div>}
      </Modal>
    )
  }

  return (
    <Found found={found} vault={vault} onClose={onClose} onDone={onDone} onSelect={onSelect} />
  )
}

function Found({ found, vault, onClose, onDone, onSelect }) {
  const mobile = useIsMobile()
  /* Which question is being read. It opens on whichever of the three found
     something, strongest first: landing on an empty "Identical" in a vault
     whose duplicates are all name matches would read as "there are none". */
  const [way, setWay] = useState(() => (
    WAYS.find((w) => found[w.key].groups.length > 0)?.key || 'content'))
  /* What is ticked, by file ID, across every question at once — so switching
     between them does not quietly drop what was chosen under the last one, and
     a file found twice is ticked once. It starts as every copy but the one each
     group suggests keeping, which is the answer for most of these. */
  const [ticked, setTicked] = useState(() => spares(found))
  const [confirming, setConfirming] = useState(null)

  const set = found[way]
  const groups = set.groups

  /* Every file the dialog knows about, by ID, so what is ticked can be turned
     into rows for the delete confirmation and the selection bar without
     hunting for it. */
  const byID = useMemo(() => {
    const out = new Map()
    for (const w of WAYS) {
      for (const group of found[w.key].groups) {
        for (const file of group.files) out.set(file.id, file)
      }
    }
    return out
  }, [found])

  const chosen = useMemo(
    () => [...ticked].map((id) => byID.get(id)).filter(Boolean), [ticked, byID])
  const bytes = chosen.reduce((sum, f) => sum + f.size, 0)

  /* Groups this question found where every copy is ticked. Allowed — somebody
     may genuinely want none of them — but never silent, because it is the one
     way this dialog could take the last copy of something. */
  const wholesale = groups.filter((g) => g.files.every((f) => ticked.has(f.id))).length

  const toggle = (id) => setTicked((current) => {
    const next = new Set(current)
    if (next.has(id)) next.delete(id)
    else next.add(id)
    return next
  })

  // Everything in this group, or nothing in it — the two answers worth a
  // button, since one at a time is what the rows already are.
  const skip = (group) => setTicked((current) => {
    const next = new Set(current)
    for (const f of group.files) next.delete(f.id)
    return next
  })
  const spare = (group) => setTicked((current) => {
    const next = new Set(current)
    for (const f of group.files) if (!f.keep) next.add(f.id)
    return next
  })

  if (confirming) {
    return <BulkDelete items={confirming} vault={vault} onClose={onClose} onDone={onDone} />
  }

  const asked = WAYS.find((w) => w.key === way)

  return (
    <Modal
      title="Find duplicates"
      subtitle={found.path === '/' ? 'The root of the vault' : found.path}
      onClose={onClose}
      width={620}
    >
      <Ways found={found} way={way} onChange={setWay} />

      <p style={{
        margin: '0 0 14px', fontFamily: FONT.sans, fontSize: '11px',
        lineHeight: 1.6, color: COLORS.textMuted,
      }}>{asked.hint}</p>

      {groups.length === 0 ? (
        <Banner tone="info">
          Nothing under this folder answers to that. {found.scanned} file
          {found.scanned === 1 ? ' was' : 's were'} looked at, at every depth
          below — try one of the other two.
        </Banner>
      ) : (
        <>
          <Tally set={set} scanned={found.scanned} />

          {set.partial && (
            <Banner tone="warn">
              There were too many names down here to compare every pair, so the
              list stops short of exhaustive. What it found is real; there may be
              more. Narrowing to a folder further in compares all of it.
            </Banner>
          )}

          {set.crowded > 0 && (
            <Banner tone="info">
              {set.crowded === 1 ? 'One run' : `${set.crowded} runs`} of names
              down here {set.crowded === 1 ? 'was' : 'were'} alike in bulk rather
              than in pairs — a naming scheme, where every name is a letter from
              a dozen others. Offering the whole run as one group of duplicates
              would be wrong, so {set.crowded === 1 ? 'it was' : 'they were'}
              {' '}broken back into the names that matched exactly.
            </Banner>
          )}

          <div style={{
            maxHeight: '320px', overflowY: 'auto', marginBottom: '12px',
            border: `1px solid ${COLORS.border}`, borderRadius: '6px', background: COLORS.bg,
          }}>
            {groups.map((group) => (
              <Group
                key={group.key}
                group={group}
                base={found.path}
                ticked={ticked}
                onToggle={toggle}
                onSkip={() => skip(group)}
                onSpare={() => spare(group)}
              />
            ))}
          </div>

          {wholesale > 0 && (
            <Banner tone="warn">
              {wholesale === 1 ? 'One group has' : `${wholesale} groups have`} every
              copy ticked, so nothing of {wholesale === 1 ? 'it' : 'them'} would be
              left. Untick a row, or use <strong>Keep one</strong> on the group.
            </Banner>
          )}
        </>
      )}

      <div style={{
        marginBottom: '16px', fontFamily: FONT.sans, fontSize: '11.5px',
        lineHeight: 1.6, color: chosen.length ? COLORS.textDim : COLORS.textMuted,
      }}>
        {chosen.length === 0
          ? 'Nothing is ticked. Tick the copies you are done with, under any of the three questions — a tick made under one is still there under the others.'
          : `${chosen.length} file${chosen.length === 1 ? '' : 's'} ticked, ${formatBytes(bytes)}, counted across all three questions rather than just this one.`}
      </div>

      <Footer mobile={mobile}>
        <Button
          variant="danger"
          disabled={chosen.length === 0}
          onClick={() => setConfirming(chosen.map(asItem))}
        >Delete {chosen.length || ''}</Button>
        <Button
          variant="primary"
          disabled={chosen.length === 0}
          onClick={() => { onSelect(chosen); onClose() }}
        >Select {chosen.length || ''}</Button>
        <Button variant="ghost" onClick={onClose}>Cancel</Button>
      </Footer>
    </Modal>
  )
}

/* Every copy but the one each group suggests keeping, under all three
   questions — which is what the dialog opens with ticked, and the answer for
   most of these. A file found under two questions is one tick.

   A file any group suggests keeping is never ticked to begin with, even where
   another question has it down as a spare: the two questions can disagree about
   which copy is the original, and the one thing this dialog must not do
   unprompted is open with every copy of something ticked. Ticking it is still a
   click away. */
function spares(found) {
  const keepers = new Set()
  for (const way of WAYS) {
    for (const group of found[way.key].groups) {
      for (const file of group.files) if (file.keep) keepers.add(file.id)
    }
  }

  const out = new Set()
  for (const way of WAYS) {
    for (const group of found[way.key].groups) {
      for (const file of group.files) {
        if (!file.keep && !keepers.has(file.id)) out.add(file.id)
      }
    }
  }
  return out
}

function Ways({ found, way, onChange }) {
  return (
    <div style={{ display: 'flex', gap: '6px', marginBottom: '10px' }}>
      {WAYS.map((w) => {
        const set = found[w.key]
        const on = way === w.key
        return (
          <button
            key={w.key}
            type="button"
            onClick={() => onChange(w.key)}
            aria-pressed={on}
            title={w.hint}
            style={{
              flex: 1,
              minHeight: '44px',
              padding: '5px 8px',
              display: 'flex', flexDirection: 'column', gap: '2px',
              alignItems: 'center', justifyContent: 'center',
              background: on ? COLORS.surfaceRaised : COLORS.bg,
              border: `1px solid ${on ? COLORS.accent : COLORS.border}`,
              borderRadius: '6px',
              color: on ? COLORS.text : COLORS.textDim,
              cursor: 'pointer',
            }}
          >
            <span style={{ fontFamily: FONT.sans, fontSize: '11.5px' }}>{w.label}</span>
            <span style={{
              fontFamily: FONT.mono, fontSize: '11px',
              color: set.extra > 0 ? COLORS.accentBright : COLORS.textMuted,
            }}>{set.extra > 0 ? `${set.extra} spare` : 'none'}</span>
          </button>
        )
      })}
    </div>
  )
}

/* What this question found, in figures. The number that matters is what is
   spare rather than what is in a group: a group of three copies is two copies
   too many, and putting three on a button that erases is how a tool like this
   frightens somebody. */
function Tally({ set, scanned }) {
  return (
    <div style={{
      display: 'flex', flexDirection: 'column', gap: '8px',
      padding: '12px 13px', marginBottom: '14px',
      background: COLORS.bg, border: `1px solid ${COLORS.border}`, borderRadius: '6px',
    }}>
      {[
        [`${set.extra} spare cop${set.extra === 1 ? 'y' : 'ies'}`,
          `in ${set.groups.length} group${set.groups.length === 1 ? '' : 's'}, out of ${scanned} file${scanned === 1 ? '' : 's'} at every depth under this folder`],
        [formatBytes(set.waste),
          'comes back if each group keeps one — nothing else here is touched'],
      ].map(([figure, note], i) => (
        <div key={i} style={{ display: 'flex', gap: '10px', alignItems: 'baseline' }}>
          <span style={{
            flexShrink: 0, fontFamily: FONT.mono, fontSize: '12.5px',
            fontWeight: 700, color: COLORS.accentBright,
          }}>{figure}</span>
          <span style={{
            fontFamily: FONT.sans, fontSize: '11.5px', lineHeight: 1.5, color: COLORS.textDim,
          }}>{note}</span>
        </div>
      ))}
    </div>
  )
}

/* One set of copies: what they are, where each one is, and which the dialog
   suggests surviving.

   The suggestion is the shallowest copy with the plainest name — "report.pdf"
   in the folder you are standing in beats "report (2).pdf" three folders down —
   and it is only a suggestion: every row is a tick of its own, and Keep one
   puts the ticks back the way they started if a group has been worked over. */
function Group({ group, base, ticked, onToggle, onSkip, onSpare }) {
  const mobile = useIsMobile()
  const keeper = group.files.find((f) => f.keep) || group.files[0]

  return (
    <div style={{ borderBottom: `1px solid ${COLORS.border}` }}>
      <div style={{
        display: 'flex', alignItems: 'center', gap: '8px', flexWrap: 'wrap',
        padding: '8px 11px', background: COLORS.surface,
      }}>
        <span style={{
          flex: '1 1 110px', minWidth: 0,
          fontFamily: FONT.mono, fontSize: '11.5px', color: COLORS.text,
          overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
        }} title={keeper.name}>{keeper.name}</span>

        {group.certain
          ? <Tag tone={COLORS.success}>identical</Tag>
          : <Tag tone={COLORS.warn}>alike</Tag>}

        <span style={{
          flexShrink: 0, fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textMuted,
        }}>{group.files.length} copies · {formatBytes(group.waste)} spare</span>

        <span style={{ display: 'flex', gap: '4px', flexShrink: 0 }}>
          <Button size="sm" variant="ghost" onClick={onSpare}>Keep one</Button>
          <Button size="sm" variant="ghost" onClick={onSkip}>Skip</Button>
        </span>
      </div>

      {group.files.map((file) => {
        const on = ticked.has(file.id)
        return (
          <button
            key={file.id}
            type="button"
            role="checkbox"
            aria-checked={on}
            onClick={() => onToggle(file.id)}
            style={{
              display: 'flex',
              alignItems: mobile ? 'flex-start' : 'center',
              gap: '10px',
              width: '100%',
              minHeight: '38px',
              padding: '7px 11px',
              textAlign: 'left',
              background: 'transparent',
              border: 'none',
              borderTop: `1px solid ${COLORS.border}`,
              color: COLORS.text,
              cursor: 'pointer',
            }}
          >
            <span aria-hidden="true" style={{
              width: '16px', height: '16px', flexShrink: 0, borderRadius: '4px',
              marginTop: mobile ? '2px' : 0,
              display: 'flex', alignItems: 'center', justifyContent: 'center',
              fontSize: '10px', fontWeight: 700,
              color: on ? COLORS.bg : 'transparent',
              background: on ? COLORS.error : 'transparent',
              border: on ? 'none' : `1px solid ${COLORS.border}`,
            }}>✓</span>

            {/* On a phone the path and the size stack, because a path four
                folders deep is most of what a phone's width holds and the
                title attribute that saves a desk from a truncation is a
                tooltip nothing can hover over. The tick stays beside them
                either way — it is what the row is for. */}
            <span style={{
              flex: 1, minWidth: 0,
              display: 'flex',
              flexDirection: mobile ? 'column' : 'row',
              alignItems: mobile ? 'flex-start' : 'center',
              gap: mobile ? '1px' : '10px',
            }}>
              <span style={{
                flex: mobile ? undefined : 1, minWidth: 0, maxWidth: '100%',
                fontFamily: FONT.mono, fontSize: '11.5px',
                color: on ? COLORS.text : COLORS.textDim,
                overflow: 'hidden', textOverflow: 'ellipsis',
                whiteSpace: mobile ? 'normal' : 'nowrap',
                wordBreak: mobile ? 'break-all' : undefined,
              }} title={where(file, base)}>{where(file, base)}</span>

              <span style={{
                flexShrink: 0, fontFamily: FONT.mono, fontSize: '11px',
                color: COLORS.textMuted,
              }}>
                {formatBytes(file.size)}
                {file.keep && !on ? ' · kept' : ''}
              </span>
            </span>
          </button>
        )
      })}
    </div>
  )
}

function Tag({ tone, children }) {
  return (
    <span style={{
      flexShrink: 0, padding: '1px 6px', borderRadius: '4px',
      fontFamily: FONT.mono, fontSize: '10px', letterSpacing: '0.04em',
      color: tone, border: `1px solid ${tone}55`, background: `${tone}14`,
    }}>{children}</span>
  )
}

/* Where a copy is, as it reads from the folder being looked at — the only thing
   that tells three files called IMG_0001.jpg apart. */
function where(file, base) {
  const full = `${file.dir === '/' ? '' : file.dir}/${file.name}`
  if (base === '/') return full.replace(/^\//, '')
  return full.startsWith(`${base}/`) ? full.slice(base.length + 1) : full
}

/* A survey-shaped row, so the delete confirmation and the selection bar take
   one of these without knowing it did not come from a listing. */
function asItem(file) {
  return { kind: 'file', key: `file:${file.id}`, name: file.name, file }
}

function Footer({ mobile, children }) {
  return (
    <div style={{
      display: 'flex',
      flexDirection: mobile ? 'column' : 'row',
      gap: '10px',
      justifyContent: 'flex-end',
    }}>{children}</div>
  )
}
