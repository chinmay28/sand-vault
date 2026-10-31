/* The deletes running on the machine, as seen from the page.

   A delete used to be a request this page held open: erasing a folder of a
   few hundred films is a round trip to every account per file, and the dialog
   sat on "Deleting…" for as long as that took, with the tab pinned open under
   it. Now every delete the page asks for is handed to the machine (see
   erase_runs.go), answered at once with a run, and followed from here.

   One poller for the whole page, however many things are drawing from it.
   The strip at the top of the browser, a bulk-delete dialog watching its own
   run, a row that just asked for one — all read the same list, and the list
   is asked for once a second while anything is going and only now and then
   when nothing is, so an idle page is not a request a second. A dialog that
   is drawing a run claims it, so the strip does not draw it twice. */

import { useEffect, useState } from 'react'
import { api } from './api'

/* How often the machine is asked while a delete is going, and while not. The
   answer is a list in memory either way; the idle cadence exists for another
   tab's delete to turn up here before its files vanish from the listing. */
const BUSY_POLL_MS = 1000
const IDLE_POLL_MS = 15000

let runs = []
let asked = false
const listeners = new Set()
const claimed = new Set()
let timer = null
let asking = false

function publish(next) {
  runs = next
  for (const listener of listeners) listener(runs)
}

async function ask() {
  if (asking) return
  asking = true
  try {
    const resp = await api.eraseRuns()
    asked = true
    publish(resp.runs || [])
  } catch {
    // A poll that failed says nothing about the deletes, which are running on
    // the machine either way. Leave the last answer up and ask again later.
  } finally {
    asking = false
    schedule()
  }
}

function schedule() {
  clearTimeout(timer)
  timer = null
  if (listeners.size === 0) return
  const busy = runs.some((run) => !run.done)
  timer = setTimeout(ask, busy ? BUSY_POLL_MS : IDLE_POLL_MS)
}

function subscribe(listener) {
  listeners.add(listener)
  if (listeners.size === 1) ask()
  return () => {
    listeners.delete(listener)
    if (listeners.size === 0) {
      clearTimeout(timer)
      timer = null
    }
  }
}

/* Every delete waiting, running or lately finished, kept current for as long
   as something on the page is reading it. */
export function useEraseRuns() {
  const [current, setCurrent] = useState(runs)
  useEffect(() => subscribe(setCurrent), [])
  return current
}

/* Whether the machine has answered at least once since the page loaded — what
   separates "nothing is running" from "nobody has asked yet". */
export function eraseRunsLoaded() {
  return asked
}

/* Hand a selection to the machine. `targets` is `{ id }` for a file or
   `{ path, vault }` for a folder (a path naming a file is a file), the shape
   a relocation names its selection in. Answers with the run, or null when
   nothing picked was there to delete — a selection deleted twice, which is
   not an error. */
export async function startErase(targets) {
  const resp = await api.erase({ targets, detach: true })
  if (!resp.run) return null
  publish([...runs.filter((run) => run.id !== resp.run.id), resp.run])
  schedule()
  return resp.run
}

/* Stop a waiting or running delete, or dismiss a finished one's result — the
   same request, because they are the same gesture. Nothing is lost: what was
   erased is gone, and everything not reached is where it was. */
export async function stopErase(id) {
  const resp = await api.stopErase(id)
  publish(resp.runs || [])
  schedule()
  return resp.runs || []
}

/* A dialog drawing a run claims it, so the strip at the top of the page does
   not draw the same bar twice; closing the dialog releases it, and the strip
   takes over. */
export function claimEraseRun(id) {
  if (!id) return
  claimed.add(id)
  publish(runs)
}

export function releaseEraseRun(id) {
  if (!claimed.delete(id)) return
  publish(runs)
}

export function isEraseRunClaimed(id) {
  return claimed.has(id)
}

/* What a run has done so far, in a sentence: files erased out of files
   picked, folders counted by what they held. */
export function eraseProgressText(run) {
  const at = run?.at || {}
  const total = at.total || 0
  if (!total) return ''
  return `${at.done || 0} of ${total} file${total === 1 ? '' : 's'}`
}

/* What a finished run came to, in a sentence — the part that goes in a
   banner before any warnings. The label is a path when one thing was picked
   and "12 items" when several were, and the sentence is shaped to each. */
export function eraseOutcomeText(run) {
  const report = run?.report || {}
  const files = report.deleted || 0
  const folders = report.folders || 0
  const label = run?.label || ''
  const selection = /^\d+ items?$/.test(label)
  const counted = []
  if (files) counted.push(`${files} file${files === 1 ? '' : 's'}`)
  if (folders && selection) counted.push(`${folders} folder${folders === 1 ? '' : 's'}`)
  const what = counted.length ? counted.join(' and ') : 'nothing'

  if (run?.error) {
    return `Deleting ${label} failed after ${what}: ${run.error}`
  }
  if (run?.cancelled) {
    const total = run.at?.total || 0
    const left = Math.max(0, total - files)
    return `Stopped deleting ${label}: ${what} gone, ${left === 0 ? 'nothing' : `${left} file${left === 1 ? '' : 's'}`} left exactly where ${left === 1 ? 'it was' : 'they were'}.`
  }
  if (!files && !folders) {
    return `${label}: nothing to delete — it was already gone.`
  }
  if (selection || folders) {
    return `Deleted ${label}: ${what}.`
  }
  return `Deleted ${label}.`
}
