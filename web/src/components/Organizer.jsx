import React, { useEffect, useMemo, useRef, useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { api } from '../api'
import { useIsMobile } from '../hooks'
import { ActionSheet, Banner, Button, IconButton, Modal, Spinner } from './ui'
import { BulkDelete, Progress, useRun } from './BulkActions'
import { DuplicatesTool } from './Duplicates'
import { AutomationSettings, describeCadence } from './FolderAutomation'
import { FolderRepos } from './FolderRepos'

/* Everything you do to a folder rather than to a row in it.

   Everything else in this app is about one file: upload it, move it, give it a
   poster, spread it over other clouds. What is under this menu is about the
   folder — the jobs nobody does one row at a time because doing them one row at
   a time is the reason they never get done.

   Five of them tidy the tree as it stands:

     · Flatten     — bring every file under this folder up into it.
     · Empty folders — remove the ones holding nothing, however deep.
     · Remove by type — erase every .txt, or every .nfo, under this folder.
     · Select by type — pick them all instead, and use the selection bar.
     · Duplicates  — the copies of things, by their bytes, their size or their
                     names. See Duplicates.jsx.

   Two are standing instructions rather than one-off jobs, and that is the whole
   difference between the halves: the five above happen when you press them, and
   these two keep happening afterwards.

     · Look after it — check the parts of every file on a schedule, or the
                       repositories kept here, and put back what has gone. See
                       FolderAutomation.jsx.
     · Repositories — the git repositories stored under this folder, each one a
                      bundle holding its whole history. See FolderRepos.jsx.

   They live together because they answer the same question — "what can I do to
   this folder?" — and splitting them across three buttons on a phone toolbar
   only meant three places to look. The first five are planned from one read — api.survey for the first four, and the
   duplicate question's own walk for the last, since hashes are the whole of
   what it needs and none of the other four wants them per file — and then run
   over endpoints that already existed: move a file, delete a file, remove a
   folder, one item at a time, from here. That is the same bargain the bulk
   actions make: a run that stalls on the fortieth of two hundred has moved
   thirty-nine things and says so, and there is no new endpoint that could
   half-succeed with no way to report which half. It is also why every one of
   these dialogs shows what it is about to do, counted, before there is a button
   to do it with.

   Nothing here touches a cloud account. Moving a file between folders and
   removing an empty folder are both rewrites of the encrypted index, so a
   flatten of four hundred films is as fast as a rename. Deleting is the
   exception and always was: erasing a file erases its parts from every account
   holding them, which is why the three tools that delete hand over to the same
   confirmation the delete button uses. */

/* The button, beside the film one, because both are things done to the folder
   rather than to anything in it.

   It carries the one piece of state that used to have a button of its own. A
   folder looking after itself should say so at a glance, and a folder whose
   last sweep found something should say that louder — that was the whole point
   of the clock icon, and folding the dialog into this menu must not lose it. So
   the glyph goes amber when the last run found something and accent when there
   is simply a policy, and the tooltip says which. */
export function OrganizerButton({ automation, mobile, onOpen }) {
  const on = !!automation?.enabled
  const trouble = !!automation?.trouble

  const tint = trouble ? COLORS.warn : on ? COLORS.accent : undefined
  const summary = !automation
    ? 'nothing standing'
    : !on
      ? 'a policy, switched off'
      : `${describeCadence(automation)}${trouble ? ', and the last run found something' : ''}`

  return (
    <IconButton
      glyph="🗂"
      label="Organize and automate this folder"
      title={`Flatten it, clear out the empty folders, find the duplicates, act on every file of a kind — or have it looked after on a schedule (${summary})`}
      size={mobile ? 44 : 32}
      onClick={onOpen}
      style={{ fontSize: mobile ? '15px' : '13px', color: tint }}
    />
  )
}

/* Which of them. A sheet rather than a menu: they are separate jobs with
   nothing to configure at this level, and on a phone the sheet is already how
   everything else in the toolbar asks a question.

   The two standing rows go last and carry their own state in the hint, because
   that is the difference worth drawing: the five above are things you are about
   to do, and these two are things already happening — or not, which is equally
   worth being told at the moment you are looking for them. */
export function OrganizerMenu({ path, automation, repoCount = 0, onClose, onPick }) {
  const here = path === '/' ? 'the vault' : path
  const trouble = !!automation?.trouble

  return (
    <ActionSheet
      title="Organize and automate"
      subtitle={`Five ways to tidy ${here} and everything under it, each counting what it would do before it does any of it — and two standing instructions that keep going afterwards.`}
      onClose={onClose}
      items={[
        {
          key: 'flatten',
          glyph: '⇤',
          label: 'Flatten into this folder',
          hint: 'Bring every file below up to here, then drop the folders they came from',
          onSelect: () => onPick('flatten'),
        },
        {
          key: 'prune',
          glyph: '⌫',
          label: 'Remove empty folders',
          hint: 'Every folder under here holding no file at all, however deep',
          onSelect: () => onPick('prune'),
        },
        {
          key: 'purge',
          glyph: '✕',
          label: 'Remove files by type',
          hint: 'Erase every file of the kinds you pick — .nfo, .txt, whatever is down there',
          onSelect: () => onPick('purge'),
        },
        {
          key: 'pick',
          glyph: '✓',
          label: 'Select files by type',
          hint: 'Tick them all instead, and move, download or scatter them from the selection bar',
          onSelect: () => onPick('pick'),
        },
        {
          key: 'dupes',
          glyph: '⧉',
          label: 'Find duplicates',
          hint: 'The same file twice — by its bytes, by its size, or by a name a copy marker apart',
          onSelect: () => onPick('dupes'),
        },
        {
          key: 'automate',
          glyph: '⏱',
          label: 'Look after this folder',
          hint: describeStanding(automation),
          tint: trouble ? COLORS.warn : automation?.enabled ? COLORS.accent : undefined,
          onSelect: () => onPick('automate'),
        },
        {
          key: 'repos',
          glyph: '⑂',
          label: 'Repositories kept here',
          hint: repoCount === 1
            ? 'One, stored as a bundle — ask its upstream whether it has moved'
            : repoCount
              ? `${repoCount}, stored as bundles — ask their upstreams whether they have moved`
              : 'Keep a copy of a git repository: one bundle holding its whole history',
          tint: repoCount ? COLORS.accent : undefined,
          onSelect: () => onPick('repos'),
        },
      ]}
    />
  )
}

/* What the standing row says about itself, which is the sentence the clock
   button's tooltip used to carry. */
function describeStanding(automation) {
  if (!automation) {
    return 'On a schedule, check every part of every file — or the repositories — and put back what has gone'
  }
  if (!automation.enabled) {
    return 'There is a policy here, switched off'
  }
  const what = automation.task === 'git' ? 'repositories' : 'parts of files'
  const found = automation.trouble ? ' — the last run found something' : ''
  return `${describeCadence(automation)}, over the ${what}${found}`
}

/* Whatever was chosen, over one reading of the folder.

   The survey is taken here rather than in each of the four tools that plan from
   it, so that all four are the same request and the same failure, and so that
   opening one is a spinner in a dialog rather than a dialog that appears
   already wrong. The duplicate finder is handed over whole: it reads a
   different question and draws its own spinner over it. */
export function OrganizerTool({ tool, path, vault, onClose, onDone, onSelect }) {
  const [survey, setSurvey] = useState(null)
  const [error, setError] = useState(null)

  /* Three of them do not want a survey at all, and taking one for them would be
     a request whose answer is thrown away.

     The duplicate finder asks a different question of the index — which files
     carry the same hash — and takes its own read rather than a survey carrying
     a hash per file that only it would ever look at. The two standing tools ask
     nothing about the shape of the tree: one reads the folder's policy and the
     other the repositories under it. Everything downstream of all three is
     shared: the same delete confirmation, the same selection bar, the same
     refresh of the listing behind. */
  const dupes = tool === 'dupes'
  const standing = tool === 'automate' || tool === 'repos'

  useEffect(() => {
    if (dupes || standing) return undefined
    let live = true
    setSurvey(null)
    setError(null)
    api.survey(path, vault)
      .then((resp) => { if (live) setSurvey(resp) })
      .catch((err) => { if (live) setError(err.message) })
    return () => { live = false }
  }, [path, vault, tool, dupes, standing])

  /* A sweep can rebuild files and a refresh writes a new bundle into this
     folder, so in both cases what is on screen is out of date the moment the
     dialog finishes — the same reason the five tidying tools call onDone. */
  if (tool === 'automate') {
    return (
      <AutomationSettings path={path} vault={vault} onClose={onClose} onChanged={onDone} />
    )
  }

  if (tool === 'repos') {
    return (
      <FolderRepos path={path} vault={vault} onClose={onClose} onChanged={onDone} />
    )
  }

  if (dupes) {
    return (
      <DuplicatesTool
        path={path}
        vault={vault}
        onClose={onClose}
        onDone={onDone}
        onSelect={onSelect}
      />
    )
  }

  if (error || !survey) {
    return (
      <Modal title={TITLES[tool]} subtitle={path} onClose={onClose} width={460}>
        {error
          ? <Banner tone="error">{error}</Banner>
          : <div style={{ padding: '28px', textAlign: 'center' }}><Spinner size={18} /></div>}
      </Modal>
    )
  }

  const shared = { survey, vault, onClose, onDone }
  if (tool === 'flatten') return <Flatten {...shared} />
  if (tool === 'prune') return <PruneEmpty {...shared} />
  if (tool === 'purge') return <ByType {...shared} mode="remove" />
  return <ByType {...shared} mode="select" onSelect={onSelect} />
}

const TITLES = {
  flatten: 'Flatten this folder',
  prune: 'Remove empty folders',
  purge: 'Remove files by type',
  pick: 'Select files by type',
}

/* --- Flatten ---------------------------------------------------------- */

/* Everything below, brought up to here.

   Two files with the same name is not an edge case in this one, it is the
   normal case: a folder per camera, a folder per season, a folder per disc, and
   IMG_0001.jpg in every one of them. So the names are planned before anything
   moves — either numbered the way a desktop file manager numbers a collision,
   or written out with the folders they came from, which is the difference
   between three files called IMG_0001.jpg and one called
   "2023 - Corfu - IMG_0001.jpg". The plan is built against the names already
   here as well as against each other, so a moved file never lands on one that
   was in this folder all along.

   The emptied folders go afterwards, in the same run, deepest first — they are
   only empty because the moves in front of them worked, and one that is not
   empty refuses to be removed rather than taking anything with it. */
function Flatten({ survey, vault, onClose, onDone }) {
  const [prefix, setPrefix] = useState(false)
  const [prune, setPrune] = useState(true)
  /* Whether the plan is on screen. Off to begin with, because the answer is
     usually a number and occasionally two hundred rows — but one click away,
     because the number is a promise and the rows are the thing itself. */
  const [showing, setShowing] = useState(false)
  const [started, setStarted] = useState(null)

  const base = survey.path
  const moves = useMemo(() => planMoves(survey, prefix), [survey, prefix])
  const bytes = moves.reduce((sum, m) => sum + m.size, 0)
  const renamed = moves.filter((m) => m.to !== m.file).length
  // The folders the files are actually coming out of, which is not the same as
  // the folders under here — some of those are already empty.
  const from = new Set(moves.map((m) => m.from)).size

  const items = useMemo(() => ([
    ...moves,
    ...(prune ? deepestFirst(survey.folders).map((f) => ({
      kind: 'folder',
      path: f.path,
      name: relative(f.path, base),
    })) : []),
  ]), [moves, prune, survey.folders, base])

  if (started) {
    return (
      <Run
        title="Flatten this folder"
        subtitle={base}
        items={started}
        verb="Moving"
        done="tidied"
        vault={vault}
        base={base}
        onClose={onClose}
        onDone={onDone}
      />
    )
  }

  return (
    <Modal
      title="Flatten this folder"
      subtitle={base === '/' ? 'The root of the vault' : base}
      onClose={onClose}
      width={480}
    >
      {moves.length === 0 ? (
        <>
          <Banner tone="info">
            {survey.folders.length === 0
              ? 'There are no folders under this one — it is already flat.'
              : 'Every file under this folder is already in it. The folders below hold nothing.'}
          </Banner>
          <Buttons onClose={onClose} />
        </>
      ) : (
        <>
          <Count
            lines={[
              [`${moves.length} file${moves.length === 1 ? '' : 's'}`, `come up from ${from} folder${from === 1 ? '' : 's'}`],
              [formatBytes(bytes), 'none of which travels — a file records the folder it is in, and its parts stay where they are'],
            ]}
          />

          <Choice
            checked={prefix}
            onChange={setPrefix}
            label="Name each file after the folders it came from"
            hint={prefix
              ? `Like “${sample(moves)}” — nothing is numbered unless two files still collide`
              : `${renamed} name${renamed === 1 ? '' : 's'} would be numbered to avoid a collision`}
          />
          <Choice
            checked={prune}
            onChange={setPrune}
            label="Remove the folders left behind"
            hint="Emptied by the moves above, so nothing is in them to lose. One that still holds something is refused rather than taken."
          />

          {/* Under the naming rather than over it: the plan is what the two
              choices above produce, so it is worth reading after they have been
              made — and it redraws under the tick, which is the clearest thing
              either choice could say about itself. */}
          <div style={{ marginBottom: showing ? '10px' : '16px' }}>
            <Button size="sm" variant="ghost" onClick={() => setShowing(!showing)}>
              {showing ? '▾ Hide the moves' : `▸ Show all ${moves.length} move${moves.length === 1 ? '' : 's'}`}
            </Button>
          </div>

          {showing && <Moves moves={moves} folders={prune ? survey.folders.length : 0} />}

          <Actions>
            <Button variant="primary" onClick={() => setStarted(items)}>
              Flatten {moves.length} file{moves.length === 1 ? '' : 's'}
            </Button>
            <Button variant="ghost" onClick={onClose}>Cancel</Button>
          </Actions>
        </>
      )}
    </Modal>
  )
}

/* Where each file would land, worked out before a single one moves.

   Shallowest first, which is the order the survey comes in, so the file that
   was already nearest the top keeps its name and the deeper duplicate is the
   one that gets a number. */
function planMoves(survey, prefix) {
  const base = survey.path
  // Every name this folder already holds, so a file arriving from below can
  // never land on one that has been sitting here all along.
  const taken = new Set(survey.files.filter((f) => f.depth === 0).map((f) => f.name.toLowerCase()))
  const moves = []

  for (const file of survey.files) {
    if (file.depth === 0) continue
    /* A prefix is joined with a separator rather than with the slash it had:
       a name is a leaf, and the server keeps only the leaf of anything with a
       slash in it — which would silently undo the naming. */
    const wanted = prefix
      ? `${relative(file.dir, base).split('/').join(' - ')} - ${file.name}`
      : file.name
    const to = unique(wanted, taken)
    taken.add(to.toLowerCase())
    moves.push({
      kind: 'file',
      id: file.id,
      from: file.dir,
      file: file.name,
      // What the progress line reads: where it is coming from, which is the
      // only thing that tells two files of the same name apart.
      name: relative(`${file.dir}/${file.name}`, base),
      to,
      size: file.size,
    })
  }
  return moves
}

/* A name nothing else has claimed, numbered the way a desktop file manager
   numbers one. Compared without case, which is stricter than the index is — a
   vault that can hold both a.jpg and A.jpg is not a folder anybody wants to
   look at. */
function unique(name, taken) {
  if (!taken.has(name.toLowerCase())) return name

  const dot = name.lastIndexOf('.')
  const stem = dot > 0 ? name.slice(0, dot) : name
  const ext = dot > 0 ? name.slice(dot) : ''
  for (let i = 2; i < 1000; i++) {
    const candidate = `${stem} (${i})${ext}`
    if (!taken.has(candidate.toLowerCase())) return candidate
  }
  return `${stem} (${Date.now()})${ext}`
}

function sample(moves) {
  const shown = moves[0]?.to || ''
  return shown.length > 46 ? `${shown.slice(0, 45)}…` : shown
}

/* How many rows of the plan are drawn before it stops being a plan and starts
   being a scroll. Whatever is past this is counted rather than hidden — a
   preview that quietly showed some of what was about to happen would be worse
   than one that showed none of it. */
const MOVES_SHOWN = 300

/* The plan itself: where each file is now, and what it will be called when it
   gets here.

   Both halves are needed and neither is enough. The source path is the only
   thing that tells three files called IMG_0001.jpg apart, and the new name is
   the whole question the naming choice above decides — a preview showing only
   the second column would be a list of names nobody could trace back, and only
   the first would be the survey again. Names the flatten had to change are lit,
   so what the collision rule did is legible at a glance rather than by
   comparing two columns line by line. */
function Moves({ moves, folders }) {
  // On a phone the two halves stack rather than share the line: a name is the
  // whole point of the row, and half a phone's width truncates most of them —
  // where the title attribute that saves a desk from the same fate is a
  // tooltip nothing can hover over.
  const mobile = useIsMobile()
  const shown = moves.slice(0, MOVES_SHOWN)
  const rest = moves.length - shown.length

  return (
    <>
      <div style={{
        maxHeight: '240px', overflowY: 'auto', marginBottom: '10px',
        border: `1px solid ${COLORS.border}`, borderRadius: '6px', background: COLORS.bg,
      }}>
        {shown.map((move) => {
          const renamed = move.to !== move.file
          return (
            <div
              key={move.id}
              style={{
                display: 'flex',
                flexDirection: mobile ? 'column' : 'row',
                alignItems: mobile ? 'stretch' : 'baseline',
                gap: mobile ? '2px' : '8px',
                padding: '7px 11px',
                borderBottom: `1px solid ${COLORS.border}`,
                fontFamily: FONT.mono, fontSize: '11.5px',
              }}
            >
              <span style={{
                flex: mobile ? undefined : '1 1 45%', minWidth: 0, color: COLORS.textDim,
                overflow: 'hidden', textOverflow: 'ellipsis',
                whiteSpace: mobile ? 'normal' : 'nowrap',
                wordBreak: mobile ? 'break-all' : undefined,
              }} title={move.name}>{move.name}</span>
              {!mobile && (
                <span aria-hidden="true" style={{ flexShrink: 0, color: COLORS.textMuted }}>→</span>
              )}
              <span style={{
                flex: mobile ? undefined : '1 1 40%', minWidth: 0,
                overflow: 'hidden', textOverflow: 'ellipsis',
                whiteSpace: mobile ? 'normal' : 'nowrap',
                wordBreak: mobile ? 'break-all' : undefined,
                color: renamed ? COLORS.accentBright : COLORS.textMuted,
              }} title={move.to}>{mobile ? `→ ${move.to}` : move.to}</span>
            </div>
          )
        })}
      </div>
      <p style={{
        margin: '0 0 16px', fontFamily: FONT.sans, fontSize: '11px',
        lineHeight: 1.5, color: COLORS.textMuted,
      }}>
        {rest > 0
          ? `The first ${MOVES_SHOWN} of ${moves.length}; the other ${rest} move the same way. `
          : ''}
        Every name is against the folder as it will be, so nothing here lands on anything
        else here. A lit name is one the flatten had to change.
        {folders > 0 && ` Then ${folders} folder${folders === 1 ? '' : 's'} go, deepest first.`}
      </p>
    </>
  )
}

/* --- Empty folders ---------------------------------------------------- */

/* The folders holding nothing at all.

   Nothing at all rather than nothing directly: a folder whose only contents are
   three more empty folders is empty too, and removing it is what somebody
   asking for this means. They go deepest first for the same reason — each is
   removed on its own, non-recursively, so the only way a parent can go is after
   its children have. A folder that turns out to be holding something is refused
   by the server rather than emptied, which is the guarantee that makes this
   safe to press without reading the list. */
function PruneEmpty({ survey, vault, onClose, onDone }) {
  const [started, setStarted] = useState(null)

  const base = survey.path
  const empty = useMemo(
    () => deepestFirst(survey.folders.filter((f) => f.total === 0)), [survey.folders])

  if (started) {
    return (
      <Run
        title="Remove empty folders"
        subtitle={base}
        items={started}
        verb="Removing"
        done="removed"
        vault={vault}
        base={base}
        onClose={onClose}
        onDone={onDone}
      />
    )
  }

  return (
    <Modal
      title="Remove empty folders"
      subtitle={base === '/' ? 'The root of the vault' : base}
      onClose={onClose}
      width={480}
    >
      {empty.length === 0 ? (
        <>
          <Banner tone="info">
            {survey.folders.length === 0
              ? 'There are no folders under this one.'
              : `All ${survey.folders.length} folder${survey.folders.length === 1 ? '' : 's'} under this one hold${survey.folders.length === 1 ? 's' : ''} something.`}
          </Banner>
          <Buttons onClose={onClose} />
        </>
      ) : (
        <>
          <Count
            lines={[
              [`${empty.length} of ${survey.folders.length} folder${survey.folders.length === 1 ? '' : 's'}`, 'hold no file at all, at any depth below them'],
            ]}
          />

          <List
            rows={empty.map((f) => ({ key: f.path, label: relative(f.path, base) }))}
            note="Deepest first, so a folder whose only contents were empty folders goes after them."
          />

          <Actions>
            <Button
              variant="primary"
              onClick={() => setStarted(empty.map((f) => ({
                kind: 'folder', path: f.path, name: relative(f.path, base),
              })))}
            >Remove {empty.length}</Button>
            <Button variant="ghost" onClick={onClose}>Cancel</Button>
          </Actions>
        </>
      )}
    </Modal>
  )
}

/* --- By type ---------------------------------------------------------- */

/* Every file of a kind, either erased or ticked.

   The same dialog for both, because picking the kinds is the whole of it and
   the two differ only in what happens to what was picked. Erasing hands the
   answer to the delete confirmation the rest of the app uses — a file's parts
   are on three accounts and going through the same dialog is what keeps that
   said out loud — and selecting hands it back to the file browser, where the
   selection bar already knows how to move, download, scatter or vault it. */
function ByType({ survey, mode, vault, onClose, onDone, onSelect }) {
  const [deep, setDeep] = useState(true)
  const [picked, setPicked] = useState(() => new Set())
  const [confirming, setConfirming] = useState(null)

  const scoped = useMemo(
    () => survey.files.filter((f) => deep || f.depth === 0), [survey.files, deep])
  const kinds = useMemo(() => census(scoped), [scoped])
  const matched = useMemo(
    () => scoped.filter((f) => picked.has(f.ext || '')), [scoped, picked])
  const bytes = matched.reduce((sum, f) => sum + f.size, 0)

  const toggle = (ext) => setPicked((current) => {
    const next = new Set(current)
    if (next.has(ext)) next.delete(ext)
    else next.add(ext)
    return next
  })

  if (confirming) {
    return (
      <BulkDelete
        items={confirming}
        vault={vault}
        onClose={onClose}
        onDone={onDone}
      />
    )
  }

  const remove = mode === 'remove'

  return (
    <Modal
      title={remove ? 'Remove files by type' : 'Select files by type'}
      subtitle={survey.path === '/' ? 'The root of the vault' : survey.path}
      onClose={onClose}
      width={480}
    >
      <Scope deep={deep} onChange={setDeep} here={survey.files.filter((f) => f.depth === 0).length} all={survey.files.length} />

      {kinds.length === 0 ? (
        <Banner tone="info">There are no files {deep ? 'under this folder' : 'in this folder'}.</Banner>
      ) : (
        <div style={{
          display: 'flex', flexDirection: 'column', gap: '5px',
          maxHeight: '260px', overflowY: 'auto', marginBottom: '16px',
        }}>
          {kinds.map((kind) => (
            <Kind
              key={kind.ext}
              kind={kind}
              chosen={picked.has(kind.ext)}
              onToggle={() => toggle(kind.ext)}
            />
          ))}
        </div>
      )}

      <div style={{
        marginBottom: '16px', fontFamily: FONT.sans, fontSize: '11.5px',
        lineHeight: 1.6, color: matched.length ? COLORS.textDim : COLORS.textMuted,
      }}>
        {matched.length === 0
          ? 'Pick a kind above.'
          : remove
            ? `${matched.length} file${matched.length === 1 ? '' : 's'}, ${formatBytes(bytes)}. Every part of every one of them is erased from each account holding it, and this cannot be undone.`
            : `${matched.length} file${matched.length === 1 ? '' : 's'}, ${formatBytes(bytes)}. They are ticked rather than touched — the selection bar is what acts on them.`}
      </div>

      <Actions>
        {remove ? (
          <Button
            variant="danger"
            disabled={matched.length === 0}
            onClick={() => setConfirming(matched.map(asItem))}
          >Delete {matched.length || ''}</Button>
        ) : (
          <Button
            variant="primary"
            disabled={matched.length === 0}
            onClick={() => { onSelect(matched); onClose() }}
          >Select {matched.length || ''}</Button>
        )}
        <Button variant="ghost" onClick={onClose}>Cancel</Button>
      </Actions>
    </Modal>
  )
}

/* What kinds are down there, most files first. A file with no extension at all
   is a kind of its own rather than being left out — it is exactly the file
   somebody is hunting for when they open this. */
function census(files) {
  const by = new Map()
  for (const file of files) {
    const ext = file.ext || ''
    const at = by.get(ext) || { ext, files: 0, bytes: 0 }
    at.files += 1
    at.bytes += file.size
    by.set(ext, at)
  }
  return [...by.values()].sort((a, b) => (
    b.files - a.files || a.ext.localeCompare(b.ext)))
}

function Kind({ kind, chosen, onToggle }) {
  return (
    <button
      type="button"
      role="checkbox"
      aria-checked={chosen}
      onClick={onToggle}
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: '10px',
        width: '100%',
        minHeight: '40px',
        padding: '8px 11px',
        textAlign: 'left',
        background: chosen ? COLORS.surfaceRaised : COLORS.bg,
        border: `1px solid ${chosen ? COLORS.accent : COLORS.border}`,
        borderRadius: '6px',
        color: COLORS.text,
        cursor: 'pointer',
      }}
    >
      <span aria-hidden="true" style={{
        width: '18px', height: '18px', flexShrink: 0, borderRadius: '4px',
        display: 'flex', alignItems: 'center', justifyContent: 'center',
        fontSize: '11px', fontWeight: 700,
        color: chosen ? COLORS.bg : 'transparent',
        background: chosen ? COLORS.accent : 'transparent',
        border: chosen ? 'none' : `1px solid ${COLORS.border}`,
      }}>✓</span>
      <span style={{
        flex: 1, minWidth: 0, fontFamily: FONT.mono, fontSize: '12px',
        overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
        color: kind.ext ? COLORS.text : COLORS.textDim,
      }}>{kind.ext || 'no extension'}</span>
      <span style={{
        flexShrink: 0, fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textMuted,
      }}>{kind.files} · {formatBytes(kind.bytes)}</span>
    </button>
  )
}

/* This folder, or everything under it. Two buttons rather than a checkbox: it
   is the question the whole dialog is answered against, and the counts on them
   are half the answer already. */
function Scope({ deep, here, all, onChange }) {
  const option = (on, label, count) => (
    <button
      type="button"
      onClick={() => onChange(on)}
      aria-pressed={deep === on}
      style={{
        flex: 1,
        minHeight: '38px',
        padding: '6px 10px',
        background: deep === on ? COLORS.surfaceRaised : COLORS.bg,
        border: `1px solid ${deep === on ? COLORS.accent : COLORS.border}`,
        borderRadius: '6px',
        color: deep === on ? COLORS.text : COLORS.textDim,
        fontFamily: FONT.mono,
        fontSize: '11.5px',
        cursor: 'pointer',
      }}
    >{label} · {count}</button>
  )

  return (
    <div style={{ display: 'flex', gap: '6px', marginBottom: '14px' }}>
      {option(false, 'This folder', here)}
      {option(true, 'Everything under it', all)}
    </div>
  )
}

/* A survey file, as the rows of a listing pass one around — so the delete
   confirmation, the folder picker and the download loop take one of these
   without knowing it did not come from a listing. It carries no shard
   placements, and nothing here needs them: what is being asked for is the
   file, by ID. */
function asItem(file) {
  return { kind: 'file', key: `file:${file.id}`, name: file.name, file }
}

/* --- The run --------------------------------------------------------- */

/* Moving files up and removing folders, one at a time, with somewhere to say
   how far it has got and what refused.

   One run for both kinds because a flatten is both: the folders can only go
   once the files in them have come up, and splitting that into two progress
   bars would be two dialogs for one decision. */
function Run({ title, subtitle, items, verb, done, vault, base, onClose, onDone }) {
  const run = useRun(items, async (item) => {
    if (item.kind === 'folder') {
      const resp = await api.deleteFolder(item.path, false, vault)
      return resp?.warnings
    }
    await api.moveFile(item.id, base, item.to)
    return null
  }, onDone)

  const started = useRef(false)
  useEffect(() => {
    if (started.current) return
    started.current = true
    run.start()
    // Taken once when the dialog mounts; re-running it would move everything
    // a second time.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const batch = run.items

  return (
    <Modal
      title={title}
      subtitle={subtitle}
      onClose={run.done ? onClose : undefined}
      width={480}
    >
      {run.done ? (
        <>
          <Outcome done={run.done} total={batch.length} verb={done} />
          <Actions>
            <Button variant="primary" onClick={onClose}>Done</Button>
          </Actions>
        </>
      ) : (
        <>
          <Progress items={batch} at={Math.max(0, run.at)} verb={verb} />
          <p style={{
            margin: 0, fontFamily: FONT.sans, fontSize: '11.5px',
            color: COLORS.textMuted, lineHeight: 1.6,
          }}>
            Each one is a rewrite of the index and nothing more — no account is
            contacted, and nothing you close this on is left half done.
          </p>
        </>
      )}
    </Modal>
  )
}

/* What a finished run came to. The same shape the bulk actions report, kept
   here rather than shared because a partial flatten is worth a different
   sentence: what did not move is still where it was, and running it again picks
   up exactly that. */
function Outcome({ done, total, verb }) {
  const failed = done.failures.length

  return (
    <>
      <Banner tone={failed ? 'warn' : 'success'}>
        {failed
          ? `${total - failed} of ${total} ${verb}. The rest are untouched — organizing again picks up exactly what is left.`
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

/* --- Small pieces ----------------------------------------------------- */

/* What the tool would do, in figures, above whatever it is asking. Every one of
   these dialogs leads with this: none of them acts on something you picked, so
   the count is the only thing standing between a button and a tree. */
function Count({ lines }) {
  return (
    <div style={{
      display: 'flex', flexDirection: 'column', gap: '8px',
      padding: '12px 13px', marginBottom: '16px',
      background: COLORS.bg, border: `1px solid ${COLORS.border}`, borderRadius: '6px',
    }}>
      {lines.map(([figure, note], i) => (
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

function Choice({ checked, label, hint, onChange }) {
  return (
    <label style={{
      display: 'flex', gap: '10px', alignItems: 'flex-start',
      marginBottom: '14px', cursor: 'pointer',
    }}>
      <input
        type="checkbox"
        checked={checked}
        onChange={(e) => onChange(e.target.checked)}
        style={{ marginTop: '2px', accentColor: COLORS.accent, flexShrink: 0 }}
      />
      <span style={{ minWidth: 0 }}>
        <span style={{
          display: 'block', fontFamily: FONT.sans, fontSize: '12.5px', color: COLORS.text,
        }}>{label}</span>
        <span style={{
          display: 'block', marginTop: '3px', fontFamily: FONT.sans,
          fontSize: '11px', lineHeight: 1.5, color: COLORS.textMuted,
        }}>{hint}</span>
      </span>
    </label>
  )
}

/* What is about to happen to, listed. Only where the list is the point — the
   folders being removed are named because a folder is a thing somebody made on
   purpose, and a count of them says nothing about which. */
function List({ rows, note }) {
  return (
    <>
      <div style={{
        maxHeight: '190px', overflowY: 'auto', marginBottom: '10px',
        border: `1px solid ${COLORS.border}`, borderRadius: '6px', background: COLORS.bg,
      }}>
        {rows.map((row) => (
          <div key={row.key} style={{
            padding: '7px 11px',
            fontFamily: FONT.mono, fontSize: '11.5px', color: COLORS.textDim,
            overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
            borderBottom: `1px solid ${COLORS.border}`,
          }}>{row.label}</div>
        ))}
      </div>
      <p style={{
        margin: '0 0 16px', fontFamily: FONT.sans, fontSize: '11px',
        lineHeight: 1.5, color: COLORS.textMuted,
      }}>{note}</p>
    </>
  )
}

function Actions({ children }) {
  const mobile = useIsMobile()

  return (
    <div style={{
      display: 'flex',
      flexDirection: mobile ? 'column' : 'row',
      gap: '10px',
      justifyContent: 'flex-end',
    }}>{children}</div>
  )
}

function Buttons({ onClose }) {
  return (
    <Actions><Button variant="primary" onClick={onClose}>Close</Button></Actions>
  )
}

/* A path as it reads from the folder being organized, which is the only part of
   it anybody looking at this dialog cares about. */
function relative(full, base) {
  if (base === '/') return full.replace(/^\//, '')
  return full.startsWith(`${base}/`) ? full.slice(base.length + 1) : full
}

/* Children before parents. Every folder removal here is non-recursive, so this
   ordering is not a nicety — it is what makes removing a tree of empty folders
   possible at all without a call that could take a file with it. */
function deepestFirst(folders) {
  return [...folders].sort((a, b) => b.depth - a.depth || b.path.localeCompare(a.path))
}
