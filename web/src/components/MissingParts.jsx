import React, { useCallback, useEffect, useState } from 'react'
import { COLORS, FONT, accountColor, fileIcon, formatBytes } from '../theme'
import { api } from '../api'
import { useIsMobile } from '../hooks'
import { Banner, Button, Empty, Modal, Spinner } from './ui'
import { RelocateClouds, schemeName } from './CloudSelect'

/* The files behind "4 files missing a spare part".

   That line sat under the storage figures for a long time saying something
   true and unreachable. A part goes missing when the account meant to hold it
   was not answering at the moment the file was scattered: the upload succeeds,
   the file is readable, nothing anywhere turns red — and the file is one cloud
   worse off than it asked to be, permanently, because nothing ever goes back
   to finish it. The count named that; it did not say which files, and there was
   no way from it to the one thing that fixes them.

   This is that list, and the fix beside each row. The fix is the relocation
   dialog every file row already offers (see RelocateClouds) — pointed at a file
   that is short, which is the one case where it cannot carry parts across and
   cuts the file again instead, writing a full set (vault.planFileRelocation).
   What was missing was the door, and knowing which clouds the file is already
   on, which is what makes choosing a different one an informed choice rather
   than a guess.

   Paged, because the cause is not one file at a time. An account down for an
   afternoon leaves every file uploaded that afternoon short, so this list is
   normally zero rows or a great many, and rarely three. */

/* How many rows a page holds. Each carries its file's whole placement, and the
   page is read out of the index rather than from any account, so this is about
   what fits on a screen rather than about what the request costs. */
const PAGE_SIZE = 25

export default function MissingParts({ providers, onClose, onChanged }) {
  const mobile = useIsMobile()
  const [offset, setOffset] = useState(0)
  const [page, setPage] = useState(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState(null)
  const [repairing, setRepairing] = useState(null)

  const load = useCallback(async (at) => {
    setLoading(true)
    try {
      const answer = await api.degradedFiles({ offset: at, limit: PAGE_SIZE })
      /* A repair made while the dialog is open shortens the list under it, so
         the last page can empty out. Fall back a page rather than showing
         nothing over a "showing 51–75 of 50". */
      if (answer.files.length === 0 && at > 0 && answer.total > 0) {
        const back = Math.max(0, Math.floor((answer.total - 1) / PAGE_SIZE) * PAGE_SIZE)
        if (back !== at) { setOffset(back); return }
      }
      setPage(answer)
      setError(null)
    } catch (err) {
      setError(err.message)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { load(offset) }, [load, offset])

  /* A relocation rewrites the index, so both the list and the figures the
     panel behind this dialog is drawing are stale the moment one finishes. */
  const repaired = () => {
    load(offset)
    onChanged?.()
  }

  const total = page?.total || 0
  const shown = page?.files?.length || 0
  const first = total === 0 ? 0 : offset + 1
  const last = offset + shown
  const pages = Math.max(1, Math.ceil(total / PAGE_SIZE))
  const atPage = Math.floor(offset / PAGE_SIZE) + 1

  return (
    <Modal
      title="Files missing a part"
      subtitle={total > 0
        ? `${total} file${total === 1 ? '' : 's'} · ${formatBytes(page?.bytes || 0)} — each one short of the spread it was uploaded with`
        : undefined}
      onClose={onClose}
      width={680}
    >
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

      {page && total > 0 && (
        <Banner tone="warn">
          A part goes missing when the account meant to hold it was not answering as the
          file was scattered. The upload succeeded and the file still reads back — it just
          has less to lose than it asked for. The missing part sits on no cloud to be
          copied from, so putting it back means gathering the file and cutting it again:
          choosing clouds for a row does exactly that, and the whole file travels for it.
        </Banner>
      )}

      {/* Said separately rather than in place of the paragraph above, because
          the two are usually both true at once and only one of them is about
          something that can still be done. */}
      {page && page.unreadable > 0 && (
        <Banner tone="error">
          {page.unreadable} of these cannot be rebuilt from what the index records — too
          few parts were ever written. No arrangement of clouds brings one of those back,
          so they are listed and marked and nothing is offered for them.
        </Banner>
      )}

      {loading && !page && (
        <div style={{ display: 'flex', justifyContent: 'center', padding: '32px 0' }}>
          <Spinner />
        </div>
      )}

      {page && total === 0 && (
        <Empty icon="◈" title="Every file has its full set">
          Nothing in the vault is short a part. A file that loses one later shows up here.
        </Empty>
      )}

      {page && total > 0 && (
        <div style={{
          display: 'flex',
          flexDirection: 'column',
          gap: '6px',
          // Dimmed rather than replaced while the next page is on its way: the
          // rows underneath are about to be the same shape, and a spinner in
          // their place collapses the dialog and then reopens it.
          opacity: loading ? 0.5 : 1,
          transition: 'opacity 0.15s ease',
        }}>
          {page.files.map((file) => (
            <DegradedRow
              key={file.id}
              file={file}
              mobile={mobile}
              onRepair={() => setRepairing(file)}
            />
          ))}
        </div>
      )}

      {total > PAGE_SIZE && (
        <div style={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          gap: '10px',
          marginTop: '14px',
          paddingTop: '12px',
          borderTop: `1px solid ${COLORS.border}`,
        }}>
          <Button
            size="sm"
            disabled={offset === 0 || loading}
            onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}
          >‹ Newer</Button>

          <span style={{
            fontFamily: FONT.mono,
            fontSize: '10px',
            color: COLORS.textMuted,
            textAlign: 'center',
            lineHeight: 1.5,
          }}>
            {first}–{last} of {total}
            <br />
            page {atPage} of {pages}
          </span>

          <Button
            size="sm"
            disabled={last >= total || loading}
            onClick={() => setOffset(offset + PAGE_SIZE)}
          >Older ›</Button>
        </div>
      )}

      {repairing && (
        <RelocateClouds
          target={{ id: repairing.id }}
          title={`Move ${repairing.name}`}
          subtitle={`${formatBytes(repairing.size)} — ${schemeName({ data: repairing.data_shards, total: repairing.total_shards })}, ${repairing.stored} part${repairing.stored === 1 ? '' : 's'} stored; pick the clouds it should live on`}
          current={repairSelection(repairing, providers)}
          providers={providers}
          onClose={() => setRepairing(null)}
          onDone={repaired}
        />
      )}
    </Modal>
  )
}

/* The clouds the picker should open on for one short file.

   Not simply the ones it is on. A 2-of-3 file down to two clouds would open the
   picker on two, which is a 2-of-2 file — narrower than the one being repaired,
   and one press away from being stored that way for good. So the selection is
   what it has, topped up with accounts it is not on until there are as many as
   its own scheme asked for: the dialog opens on the spread the file was
   uploaded with, and the button under it puts the file back on one.

   Reachable accounts are drawn first, since a cloud that is not answering is
   how the part went missing in the first place. Whoever opened the dialog can
   still change any of it — this is where the picker starts, not what it does. */
function repairSelection(file, providers) {
  const connected = new Set(providers.map((p) => p.id))
  const chosen = [...new Set(file.shards.map((s) => s.provider_id))].filter((id) => connected.has(id))

  const spare = providers.filter((p) => !chosen.includes(p.id))
  for (const p of [...spare.filter((p) => p.online), ...spare.filter((p) => !p.online)]) {
    if (chosen.length >= file.total_shards) break
    chosen.push(p.id)
  }
  return chosen
}

/* One short file: what it is, where its parts got to, and the way to finish it.

   The parts are drawn the way the file list draws them — one square per part of
   the scheme, coloured by the account holding it and outlined where nothing
   does — because that is the picture somebody already knows how to read, and
   the empty square is the whole story of the row. */
function DegradedRow({ file, mobile, onRepair }) {
  const scheme = { data: file.data_shards, total: file.total_shards }

  return (
    <div style={{
      display: 'flex',
      alignItems: mobile ? 'stretch' : 'center',
      flexDirection: mobile ? 'column' : 'row',
      gap: mobile ? '8px' : '12px',
      padding: '10px 12px',
      background: COLORS.bg,
      border: `1px solid ${COLORS.border}`,
      borderLeft: `3px solid ${file.readable ? COLORS.warn : COLORS.error}`,
      borderRadius: '6px',
    }}>
      <div style={{ flex: 1, minWidth: 0 }}>
        <div style={{
          display: 'flex',
          alignItems: 'center',
          gap: '7px',
          fontFamily: FONT.mono,
          fontSize: '12px',
          color: COLORS.text,
        }}>
          <span style={{ flexShrink: 0 }}>{fileIcon(file.mime, file.name)}</span>
          <span
            title={file.path}
            style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
          >{file.name}</span>
        </div>

        <div
          title={file.path}
          style={{
            marginTop: '3px',
            fontFamily: FONT.mono,
            fontSize: '10px',
            color: COLORS.textMuted,
            overflow: 'hidden',
            textOverflow: 'ellipsis',
            whiteSpace: 'nowrap',
          }}
        >{file.dir === '/' ? '' : `${file.dir} · `}{formatBytes(file.size)} · {schemeName(scheme)}</div>
      </div>

      <div style={{
        display: 'flex',
        alignItems: 'center',
        gap: '10px',
        flexShrink: 0,
        justifyContent: mobile ? 'space-between' : undefined,
      }}>
        <span style={{ display: 'flex', alignItems: 'center', gap: '3px' }}>
          {Array.from({ length: scheme.total }, (_, i) => i + 1).map((part) => {
            const shard = file.shards.find((s) => s.part === part)
            return (
              <span
                key={part}
                title={shard
                  ? `Part ${part} on ${shard.provider_name}`
                  : `Part ${part} was never stored`}
                style={{
                  width: scheme.total > 3 ? '12px' : '19px',
                  height: '16px',
                  borderRadius: '3px',
                  fontFamily: FONT.mono,
                  fontSize: scheme.total > 3 ? '7.5px' : '9px',
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
        </span>

        <span style={{
          fontFamily: FONT.mono,
          fontSize: '9.5px',
          lineHeight: 1.5,
          color: file.readable ? COLORS.warn : COLORS.error,
          minWidth: mobile ? undefined : '104px',
        }}>
          {file.missing} part{file.missing === 1 ? '' : 's'} missing
          <br />
          <span style={{ color: COLORS.textMuted }}>
            {/* What the file has left, said as what it can still survive rather
                than as a count of parts: "one more cloud" is the number that
                decides whether this row is worth acting on today. */}
            {file.readable
              ? (file.spare === 0
                ? 'no cloud left to lose'
                : `${file.spare} more cloud${file.spare === 1 ? '' : 's'} to spare`)
              : 'cannot be rebuilt'}
          </span>
        </span>

        <Button
          size="sm"
          variant={file.readable ? 'default' : 'ghost'}
          onClick={onRepair}
          title={file.readable
            ? 'Choose the clouds this file should live on'
            : 'Too few parts were written for this file to be rebuilt'}
          disabled={!file.readable}
          style={{ flexShrink: 0 }}
        >⇄ Clouds</Button>
      </div>
    </div>
  )
}
