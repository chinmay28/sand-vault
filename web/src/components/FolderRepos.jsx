import React, { useEffect, useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { api } from '../api'
import { Banner, Button, Input, Modal, Spinner } from './ui'

/* The repositories a folder is keeping a copy of.

   A repository in a vault is one file — a git bundle holding the whole history,
   which "git clone" reads directly — so most of what this panel does is the one
   thing a bundle cannot do for itself: notice that its upstream has moved.

   Two things are said plainly here because both are surprising and both matter:

     · The first copy is expensive and every one after it is not. Storing a
       repository means its entire history coming down once; a refresh asks the
       upstream what refs it has, which is a few kilobytes, and fetches only
       when the answer has changed. That is why a weekly schedule over fifty
       repositories is a reasonable thing to ask for.
     · SAND borrows the git on this machine rather than carrying its own, so a
       repository it can reach is exactly one you can reach from here. That is
       what makes private repositories work at all — your keys, your credential
       helper — and it is why a machine with no git says so instead of failing
       at four in the morning. */

export function FolderRepos({ path, vault = '', onClose, onChanged }) {
  const [repos, setRepos] = useState(null)
  const [available, setAvailable] = useState(true)
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)
  const [url, setUrl] = useState('')
  const [working, setWorking] = useState('')

  const load = async () => {
    try {
      const resp = await api.repos(path, vault)
      setRepos(resp.repos || [])
      setAvailable(resp.available !== false)
    } catch (err) {
      setError(err.message)
      setRepos([])
    }
  }

  useEffect(() => { load() }, [path, vault])

  const track = async () => {
    if (!url.trim()) return
    setBusy(true)
    setError(null)
    setWorking('Mirroring — the first copy is the whole history, so this can take a while.')
    try {
      await api.trackRepo({ vault, path, url: url.trim() })
      setUrl('')
      await load()
      onChanged?.()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
      setWorking('')
    }
  }

  const refresh = async (repo) => {
    setBusy(true)
    setError(null)
    setWorking(`Asking ${repo.url} whether it has moved…`)
    try {
      const resp = await api.refreshRepo(repo.id, vault)
      setWorking(resp.updated ? '' : `${repo.name} was already up to date.`)
      await load()
      onChanged?.()
    } catch (err) {
      setError(err.message)
      setWorking('')
    } finally {
      setBusy(false)
    }
  }

  const untrack = async (repo) => {
    setBusy(true)
    setError(null)
    try {
      await api.untrackRepo(repo.id, vault)
      await load()
      onChanged?.()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      title="Repositories kept here"
      subtitle={path === '/' ? 'The whole vault, and everything under it' : path}
      onClose={onClose}
      width={640}
    >
      {error && <Banner tone="error">{error}</Banner>}
      {!available && (
        <Banner tone="warn">
          This machine has no git for SAND to borrow, so nothing can be mirrored
          from here. SAND uses the git you already have rather than carrying its
          own — which is what lets it reach the same private repositories you can.
        </Banner>
      )}

      {repos === null
        ? <Spinner />
        : repos.length === 0
          ? (
            <p style={{
              fontFamily: FONT.sans, fontSize: '12.5px', lineHeight: 1.6,
              color: COLORS.textDim, margin: '0 0 16px',
            }}>
              Nothing kept here yet. A repository is stored as a single bundle
              holding its whole history — every branch, every tag — which
              <code style={{ margin: '0 4px' }}>git clone</code>
              reads back directly, with no SAND involved.
            </p>
          )
          : repos.map((repo) => (
            <Repo
              key={repo.id}
              repo={repo}
              busy={busy}
              available={available}
              onRefresh={() => refresh(repo)}
              onUntrack={() => untrack(repo)}
            />
          ))}

      {working && (
        <div style={{
          fontFamily: FONT.sans, fontSize: '12px', color: COLORS.textDim,
          margin: '0 0 12px',
        }}>{working}</div>
      )}

      <div style={{ borderTop: `1px solid ${COLORS.border}`, paddingTop: '14px', marginTop: '4px' }}>
        <Input
          label="Keep a copy of"
          placeholder="https://github.com/owner/project.git"
          value={url}
          disabled={busy || !available}
          onChange={(e) => setUrl(e.target.value)}
          help="An https:// or ssh:// address, or the git@host:path form. The whole history comes down once; after that only what has changed."
        />
        <Button variant="primary" onClick={track} disabled={busy || !available || !url.trim()}>
          {busy ? 'Working…' : 'Store it'}
        </Button>
      </div>

      <div style={{
        marginTop: '16px', paddingTop: '12px',
        borderTop: `1px solid ${COLORS.border}`,
        fontFamily: FONT.sans, fontSize: '11px', lineHeight: 1.55, color: COLORS.textMuted,
      }}>
        <p style={{ margin: '0 0 8px' }}>
          To have these kept current on their own, give this folder a policy with
          the repositories task — asking is cheap enough that a weekly sweep over
          a shelf of projects costs almost nothing on the weeks nothing moved.
        </p>
        <p style={{ margin: 0 }}>
          A stored repository whose upstream stops answering is kept, not
          deleted. An outage, a rename and a revoked token all look the same from
          here, and only one of them is a reason to throw away a copy.
        </p>
      </div>
    </Modal>
  )
}

function Repo({ repo, busy, available, onRefresh, onUntrack }) {
  return (
    <div style={{
      padding: '10px 12px',
      background: COLORS.bg,
      border: `1px solid ${repo.gone ? COLORS.warn : COLORS.border}`,
      borderRadius: '6px',
      marginBottom: '10px',
    }}>
      <div style={{
        fontFamily: FONT.sans, fontSize: '13px', color: COLORS.text, marginBottom: '2px',
      }}>{repo.name}</div>
      <div style={{
        fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textMuted,
        overflowWrap: 'anywhere', marginBottom: '6px',
      }}>{repo.url}</div>

      <div style={{
        fontFamily: FONT.sans, fontSize: '11.5px', color: COLORS.textDim, marginBottom: '8px',
      }}>
        {formatBytes(repo.size)} · {repo.refs} ref{repo.refs === 1 ? '' : 's'}
        {repo.head ? ` · ${repo.head}` : ''}
        {repo.commits ? ` · ${repo.commits} commit${repo.commits === 1 ? '' : 's'}` : ''}
      </div>

      {repo.gone && (
        <div style={{
          fontFamily: FONT.sans, fontSize: '11.5px', color: COLORS.warn, marginBottom: '8px',
        }}>
          The upstream stopped answering. The copy here is untouched.
        </div>
      )}

      <div style={{ display: 'flex', gap: '8px' }}>
        <Button size="sm" onClick={onRefresh} disabled={busy || !available}>Refresh</Button>
        <Button size="sm" variant="ghost" onClick={onUntrack} disabled={busy}>Stop following</Button>
      </div>
    </div>
  )
}
