import React, { useEffect, useMemo, useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { api } from '../api'
import { Banner, Button, Modal, Spinner } from './ui'
import {
  CloudChoice, MIN_DATA, SelectionNote, SpaceNote, ThresholdChoice,
  defaultSchemeFor, schemeFor, schemeName, usableCut,
} from './CloudSelect'

/* Storing a kept repository afresh from its upstream, on clouds you choose.

   A repository in a vault is one file, a bundle, and a bundle whose shards
   have gone past reading is a file with no way back through them: a refresh
   reads the stored bundle to fetch only what changed, and a move carries its
   shards across, and neither has anything to work with. The upstream still
   holds the whole history. This mirrors it again — the whole repository, as
   the first copy was — and stores it over this file as a fresh set of shards
   on the clouds picked here, with the shards that are here never read. That
   is what makes it work whatever state they are in, and it is the same shape
   as a rebuild from the replica: a copy that is trusted, read whole, written
   out on clouds somebody chose. Only where the copy is differs.

   It is offered for a bundle that still reads too. A rebuild costs what the
   first copy cost, so it is not the way to keep one current — that is
   Refresh — but the upstream is as good a place as any to put it on other
   clouds from, and better than three accounts one of which is not answering.

   `file` is the row as the listing names it — id, name, size, dir, and the
   shards where there are any — and `url` the upstream where the caller knows
   it; a row does not, so the dialog looks it up to say what it is about to
   ask. `current` is where the shards are now, so the dialog opens on the
   truth where it can and a swap is one click rather than four. */
export function RebuildRepo({
  file, url = '', current = [], providers, vault = '', zIndex, onClose, onDone,
}) {
  const [selected, setSelected] = useState(() => (current || []).filter(
    (id) => providers.some((p) => p.id === id)))
  const [threshold, setThreshold] = useState(null)
  const [upstream, setUpstream] = useState(url)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  /* The repository as it now stands, once the rebuild has landed, with
     whatever the upload had to say about where it went. */
  const [result, setResult] = useState(null)

  /* The upstream, where the caller did not know it: read out of the index
     rather than asked of any cloud, and left blank if the question fails —
     the dialog works the same without it. */
  useEffect(() => {
    if (url) return undefined
    let live = true
    api.repos(file.dir || '/', vault)
      .then((resp) => {
        if (!live) return
        const repo = (resp.repos || []).find((r) => r.id === file.id)
        if (repo) setUpstream(repo.url)
      })
      .catch(() => {})
    return () => { live = false }
  }, [url, file.id, file.dir, vault])

  /* The clouds settle n; k is the suggested one until somebody moves it, and
     a k left over from a wider selection is pulled back inside the new one. */
  const scheme = useMemo(() => {
    const suggested = defaultSchemeFor(selected.length)
    if (!suggested) return null
    if (threshold === null) return suggested
    return { data: Math.min(Math.max(threshold, MIN_DATA), selected.length), total: selected.length }
  }, [selected.length, threshold])

  const named = scheme && (!schemeFor(scheme.total) || schemeFor(scheme.total).data !== scheme.data)
  const cut = named ? schemeName(scheme) : ''
  const enough = usableCut(selected.length, scheme)

  const submit = async () => {
    setBusy(true)
    setError(null)
    try {
      setResult(await api.rebuildRepo(file.id, { vault, accounts: selected, scheme: cut }))
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  /* The listing is told on the way out rather than the moment the rebuild
     lands. An overwrite is a new entry, so the row this opened from names an
     id that is gone, and a listing redrawn under the dialog would take the
     row — and this dialog with it, mid-sentence — before anyone had read
     what it came to. */
  const close = () => {
    if (result) onDone?.()
    onClose()
  }

  return (
    <Modal
      title={`Rebuild ${file.name} from upstream`}
      subtitle={`${formatBytes(file.size)} — mirrored again from ${upstream || 'its upstream'}; pick the clouds the fresh bundle should live on`}
      onClose={busy ? undefined : close}
      width={480}
      zIndex={zIndex}
    >
      {error && <Banner tone="error">{error}</Banner>}

      {result && (
        <Banner tone="info">
          Stored {result.repo?.path || file.name} afresh
          {result.repo ? ` — ${formatBytes(result.repo.size)}, ${result.repo.refs} ref${result.repo.refs === 1 ? '' : 's'}${
            result.repo.commits ? `, ${result.repo.commits} commit${result.repo.commits === 1 ? '' : 's'}` : ''}` : ''}.
          {result.warnings?.length ? ` ${result.warnings.join(' ')}` : ''}
        </Banner>
      )}

      <p style={{
        margin: '0 0 12px',
        fontFamily: FONT.sans,
        fontSize: '12px',
        color: COLORS.textMuted,
        lineHeight: 1.6,
      }}>
        The repository is mirrored again from its upstream — the whole history, as the first
        copy was — and stored over this file as a fresh set of shards on the clouds you pick.
        The shards here are never read, so this works whatever state they are in; the ones
        that can still be reached are erased afterwards. It costs what the first copy cost,
        so for a bundle that still reads and only needs to catch up, Refresh is the cheaper way.
      </p>

      <CloudChoice
        providers={providers}
        selected={selected}
        onChange={setSelected}
        cap={providers.length}
      />

      <ThresholdChoice scheme={scheme} onChange={(next) => setThreshold(next.data)} />

      <div style={{ marginTop: '14px' }}>
        <SelectionNote providers={providers} selected={selected} scheme={scheme} />
      </div>

      <SpaceNote providers={providers} selected={selected} scheme={scheme} bytes={file.size} />

      <p style={{
        margin: '14px 0 0',
        fontFamily: FONT.sans,
        fontSize: '11px',
        color: COLORS.textMuted,
        lineHeight: 1.5,
      }}>
        The whole history comes down to the machine and goes back up before this says it is
        done — keep the page open until it does. A large repository can take a while.
      </p>

      <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end', marginTop: '16px' }}>
        <Button type="button" variant="ghost" onClick={close} disabled={busy}>
          {result ? 'Done' : 'Cancel'}
        </Button>
        <Button
          type="button"
          variant="primary"
          onClick={submit}
          disabled={busy || !enough || !!result}
        >
          {busy ? (
            <>
              <Spinner size={10} color={COLORS.bg} />
              {' Rebuilding…'}
            </>
          ) : '↻ Rebuild from upstream'}
        </Button>
      </div>
    </Modal>
  )
}
