import React, { useCallback, useEffect, useRef, useState } from 'react'
import { COLORS, FONT, assignAccountColors, formatBytes } from './theme'
import { useIsMobile } from './hooks'
import { useNavigator } from './navigation'
import { api } from './api'
import LockScreen from './components/LockScreen'
import AccountsPanel from './components/AccountsPanel'
import FileBrowser from './components/FileBrowser'
import PreviewModal, { ShardInspector } from './components/PreviewModal'
import RecoverVault from './components/RecoverVault'
import FilmDetails from './components/FilmDetails'
import { Brand, DevMark } from './components/Brand'
import { Banner, Button } from './components/ui'

/* SAND — a file browser over storage you do not fully trust.
   Files are compressed, split into three encrypted parts and scattered across
   separate cloud accounts. Opening one gathers the parts back and rebuilds it. */
export default function App() {
  const [status, setStatus] = useState(null)
  /* Not a single path but the trail of folders walked through, so Back and
     Forward have somewhere to step. See navigation.js. */
  const nav = useNavigator()
  const path = nav.path
  const [listing, setListing] = useState(null)
  const [providers, setProviders] = useState([])
  const [loadingList, setLoadingList] = useState(false)
  const [loadingProviders, setLoadingProviders] = useState(false)
  const [error, setError] = useState(null)
  const [preview, setPreview] = useState(null)
  const [inspecting, setInspecting] = useState(null)
  const [filming, setFilming] = useState(null)
  const [accountsOpen, setAccountsOpen] = useState(false)
  const [recovery, setRecovery] = useState(null)
  const [recovering, setRecovering] = useState(false)

  const mobile = useIsMobile()
  const unlocked = !!status?.unlocked

  // A prompt that reappears every time the accounts are refreshed stops being a
  // prompt and starts being an obstacle. Held in a ref rather than in state so
  // that saying "not now" does not itself re-run the scan.
  const recoveryDismissed = useRef(false)

  // The recovery dialog connects clouds of its own, so the accounts it is
  // working through change underneath this component while it is open. It owns
  // its own copy of the scan from the moment it mounts; this ref is what stops
  // the scan out here being cleared from under it and unmounting it mid-flow.
  const recoveringRef = useRef(false)
  const openRecovery = (open) => { recoveringRef.current = open; setRecovering(open) }

  // Which account owns which colour can only be decided against the whole list,
  // and every pane below reads the answer back by id — so settle it here, in
  // the render that owns the list, before any of them draw a badge.
  assignAccountColors(providers)

  // On a phone the accounts panel is a drawer over the browser; once there is
  // room for both panes again it is simply always there, so drop the flag.
  useEffect(() => { if (!mobile) setAccountsOpen(false) }, [mobile])

  useEffect(() => {
    api.vaultStatus().then(setStatus).catch((err) => {
      setStatus({ initialized: false, unlocked: false })
      setError(err.message)
    })
  }, [])

  const refreshProviders = useCallback(async () => {
    setLoadingProviders(true)
    try {
      const resp = await api.providers()
      setProviders(resp.providers || [])
    } catch (err) {
      if (err.code === 'LOCKED') setStatus((s) => ({ ...s, unlocked: false }))
      else setError(err.message)
    } finally {
      setLoadingProviders(false)
    }
  }, [])

  const refreshListing = useCallback(async (target = path) => {
    setLoadingList(true)
    try {
      const resp = await api.list(target)
      setListing(resp)
      setError(null)
    } catch (err) {
      if (err.code === 'LOCKED') {
        setStatus((s) => ({ ...s, unlocked: false }))
      } else if (err.code === 'NOT_FOUND' && target !== '/') {
        // The folder went away underneath us — fall back to the root. It
        // replaces the step rather than adding one, because a folder that is
        // no longer there is not somewhere Back should lead.
        nav.replace('/')
      } else {
        setError(err.message)
      }
    } finally {
      setLoadingList(false)
    }
  }, [path, nav])

  const refreshAll = useCallback(async () => {
    const fresh = await api.vaultStatus().catch(() => null)
    if (fresh) setStatus(fresh)
    await Promise.all([refreshListing(), refreshProviders()])
  }, [refreshListing, refreshProviders])

  useEffect(() => {
    if (!unlocked) return
    refreshListing(path)
  }, [unlocked, path, refreshListing])

  useEffect(() => {
    if (!unlocked) return
    refreshProviders()
  }, [unlocked, refreshProviders])

  /* Disaster recovery starts here, without being asked for.

     Someone whose machine died reinstalls SAND, makes a fresh vault and
     reconnects their clouds — and those clouds are still carrying the index of
     the vault that is gone. The app can see that and they cannot, so it looks
     as soon as there is an account to look at, and says so.

     On an empty vault, because that is the state a reinstalled machine is in and
     the only state a whole vault can be adopted into — and on a vault carrying
     shard records that point at accounts it is not connected to, which is what
     a recovery run before every cloud was back leaves behind. Any other vault is
     simply in use, and is not asked. */
  const providerKey = providers.map((p) => p.id).sort().join(',')
  const fileCount = status?.stats?.files ?? 0
  const unresolved = status?.stats?.unresolved ?? 0

  useEffect(() => {
    if (recoveringRef.current) return
    if (!unlocked || providerKey === '' || (fileCount > 0 && unresolved === 0)) {
      setRecovery(null)
      return
    }
    let cancelled = false
    api.recoveryScan().then((scan) => {
      if (cancelled) return
      setRecovery(scan)
      // Only the disaster interrupts. Resuming is offered from the banner
      // instead: it is not news — the vault has been in that state since the
      // recovery that made it — and a modal on every load would be a nag.
      if (scan.available && !recoveryDismissed.current) openRecovery(true)
    }).catch(() => {
      // An account that will not answer is the accounts panel's story to tell;
      // it should not put an error over an otherwise working vault.
    })
    return () => { cancelled = true }
  }, [unlocked, providerKey, fileCount, unresolved])

  const lock = async () => {
    await api.lock().catch(() => {})
    setStatus((s) => ({ ...s, unlocked: false, stats: null }))
    setListing(null)
    setProviders([])
    setPreview(null)
    setInspecting(null)
    setFilming(null)
    setAccountsOpen(false)
    setRecovery(null)
    openRecovery(false)
    recoveryDismissed.current = false
    // The trail is a list of folder names, which is the file index — locking
    // the vault has to put that away with everything else.
    nav.reset()
  }

  if (!status) {
    return <Shell><div style={{ padding: '80px 24px', textAlign: 'center', color: COLORS.textMuted }}>Loading…</div></Shell>
  }

  if (!unlocked) {
    return (
      <Shell>
        <LockScreen
          status={status}
          onUnlocked={(next) => { setStatus(next); setError(null) }}
        />
      </Shell>
    )
  }

  return (
    <Shell>
      {/* dvh rather than vh: a phone's collapsing address bar counts towards
          100vh, which would leave the last row of files under the browser UI. */}
      <div style={{ height: 'var(--app-height)', display: 'flex', flexDirection: 'column' }}>
        <header style={{
          display: 'flex',
          alignItems: 'center',
          // Tighter on a phone than it looks like it wants to be: the three
          // header controls are now full 44px targets, and the room for them
          // comes out of the spacing between them.
          gap: mobile ? '6px' : '14px',
          padding: mobile ? '8px 10px' : '10px 20px',
          borderBottom: `1px solid ${COLORS.border}`,
          background: COLORS.surface,
          flexShrink: 0,
        }}>
          {mobile && (
            <Button
              size="sm"
              variant="ghost"
              aria-label="Connected clouds"
              data-icon-button="true"
              onClick={() => setAccountsOpen(true)}
              style={{ fontSize: '17px', padding: '4px 8px', minWidth: '44px', justifyContent: 'center' }}
            >☰</Button>
          )}

          <Brand />

          {/* The tagline is the first thing to go: on a phone it can only wrap
              into a column beside the wordmark. */}
          {!mobile && (
            <span style={{
              fontFamily: FONT.mono,
              fontSize: '10px',
              letterSpacing: '1.5px',
              textTransform: 'uppercase',
              color: COLORS.textMuted,
              marginTop: '4px',
            }}>
              {/* Two phrases, not one — the bullet is faded against the text
                  around it so it reads as a divider rather than a word of its
                  own. The spaces are written out because JSX drops whitespace
                  that wraps a line. */}
              {'Secure Archival '}
              <span style={{ opacity: 0.55 }}>•</span>
              {' Network Distribution'}
            </span>
          )}

          <span style={{ flex: 1 }} />

          {/* Narrow enough and the labels are dropped; the glyphs carry the
              meaning and the accessible name comes off aria-label. */}
          <Button size="sm" variant="ghost" onClick={refreshAll}
            data-icon-button={mobile || undefined}
            style={mobile ? { fontSize: '16px', padding: '4px 8px', minWidth: '44px', justifyContent: 'center' } : null}
            title="Refresh" aria-label="Refresh">⟳{mobile ? '' : ' Refresh'}</Button>
          <Button size="sm" onClick={lock}
            data-icon-button={mobile || undefined}
            style={mobile ? { fontSize: '15px', padding: '4px 8px', minWidth: '44px', justifyContent: 'center' } : null}
            title="Lock vault" aria-label="Lock vault">🔒{mobile ? '' : ' Lock vault'}</Button>
          {/* No room for the developer mark up here on a phone — it moves to
              the foot of the accounts drawer instead. */}
          {!mobile && <DevMark />}
        </header>

        {/* Dismissed, but not forgotten — and the standing offer for a recovery
            that has not finished, which no modal is going to nag about. */}
        {(recovery?.available || recovery?.resumable) && !recovering && (
          <div style={{ padding: mobile ? '10px 10px 0' : '12px 20px 0', flexShrink: 0 }}>
            <Banner tone="warn">
              <span style={{ display: 'flex', alignItems: 'center', gap: '10px', flexWrap: 'wrap' }}>
                <span>
                  {recoveryNotice(recovery)}
                </span>
                <Button size="sm" variant="ghost" onClick={() => openRecovery(true)}>
                  {recovery.available ? 'Attempt recovery' : 'Finish recovery'}
                </Button>
              </span>
            </Banner>
          </div>
        )}

        <div style={{ flex: 1, display: 'flex', minHeight: 0, position: 'relative' }}>
          <AccountsPanel
            providers={providers}
            loading={loadingProviders}
            stats={status.stats}
            webdav={status.webdav}
            mobile={mobile}
            open={accountsOpen}
            onClose={() => setAccountsOpen(false)}
            onRefresh={refreshProviders}
            onChanged={refreshAll}
          />

          <FileBrowser
            nav={nav}
            listing={listing}
            loading={loadingList}
            error={error}
            providers={providers}
            defaultAccounts={status.stats?.default_accounts || []}
            mobile={mobile}
            onRefresh={refreshAll}
            onPreview={(file, hasThumb, film) => setPreview({ file, hasThumb, film })}
            onInspect={setInspecting}
            onFilm={setFilming}
            onError={setError}
          />
        </div>
      </div>

      {preview && (
        <PreviewModal
          file={preview.file}
          hasThumb={preview.hasThumb}
          film={preview.film}
          onClose={() => setPreview(null)}
          /* A file that had no picture in the list has one now. */
          onThumbStored={() => refreshListing()}
          /* And a match made from in there changes the same listing. */
          onFilmChanged={() => refreshListing()}
        />
      )}
      {filming && (
        <FilmDetails
          file={filming}
          onClose={() => setFilming(null)}
          /* A match changes the row's caption and its picture, both of which
             the listing carries. */
          onChanged={() => refreshListing()}
        />
      )}
      {recovering && recovery && (
        <RecoverVault
          scan={recovery}
          onClose={() => { openRecovery(false); recoveryDismissed.current = true }}
          onRecovered={() => {
            openRecovery(false)
            setRecovery(null)
            refreshAll()
          }}
          /* The dialog connects clouds of its own, so the accounts panel has
             to hear about them the moment they land rather than when it
             closes. */
          onAccountsChanged={refreshProviders}
        />
      )}
      {inspecting && (
        <ShardInspector
          file={inspecting}
          providers={providers}
          onClose={() => setInspecting(null)}
          /* Moving the parts changes the badges the listing draws. */
          onChanged={refreshAll}
        />
      )}
    </Shell>
  )
}

/* What the banner says, which is two quite different pieces of news.

   The first is a discovery: this machine is new and those clouds are carrying a
   vault. The second is unfinished business — a recovery that ran while some
   accounts were still missing, where the distinction worth drawing is between
   files that cannot be opened at all and parts that were only ever the spare. */
function recoveryNotice(scan) {
  if (scan.available) {
    return `Sand files detected on your clouds — ${scan.parts} part${scan.parts === 1 ? '' : 's'} `
      + `(${formatBytes(scan.bytes)}) and an encrypted copy of a vault index this one did not write.`
  }
  if (scan.stranded > 0) {
    return `${scan.stranded} file${scan.stranded === 1 ? '' : 's'} in this vault cannot be opened: `
      + 'their parts are on clouds it is not connected to. Connect them to finish the recovery.'
  }
  return `${scan.unresolved} part${scan.unresolved === 1 ? '' : 's'} of your files sit on clouds `
    + 'this vault is not connected to. Your files still open, with nothing to spare.'
}

function Shell({ children }) {
  return (
    <div style={{
      minHeight: '100vh',
      background: COLORS.bg,
      color: COLORS.text,
      fontFamily: FONT.sans,
    }}>
      <style>{`
        @keyframes sand-spin { to { transform: rotate(360deg); } }
        @keyframes sand-dev-veil {
          0% { opacity: 0; }
          7% { opacity: 1; }
          82% { opacity: 1; }
          100% { opacity: 0; }
        }
        /* Lands with a small overshoot, then drifts out as the veil clears. */
        @keyframes sand-dev-badge {
          0% { transform: scale(0.82); }
          14% { transform: scale(1.02); }
          22% { transform: scale(1); }
          82% { transform: scale(1); }
          100% { transform: scale(1.06); }
        }
        /* Row menus rise from the edge they are anchored to, so it is obvious
           they belong to the thumb rather than to the middle of the screen. */
        @keyframes sand-sheet-up {
          from { transform: translateY(14px); opacity: 0; }
          to { transform: translateY(0); opacity: 1; }
        }
        .sand-sheet { animation: sand-sheet-up 0.18s ease-out; }
        /* The cross-fade stays — it isn't motion — but the scale doesn't. */
        @media (prefers-reduced-motion: reduce) {
          .sand-dev-lockup { animation: none !important; }
          .sand-sheet { animation: none !important; }
        }
        * { box-sizing: border-box; }
        :root {
          color-scheme: dark;
          /* The visible height of the viewport. Mobile browsers include their
             collapsing address bar in 100vh, so anything sized to fill the
             screen ends up taller than the screen; dvh tracks what is actually
             on show. Kept behind a variable so the vh fallback survives on
             browsers that never learned the unit. */
          --app-height: 100vh;
        }
        @supports (height: 100dvh) { :root { --app-height: 100dvh; } }
        html { -webkit-text-size-adjust: 100%; }
        /* WebKit draws its own clear button inside a search field, which would
           sit on top of the app's — and only appears in that one browser. */
        input[type="search"]::-webkit-search-cancel-button,
        input[type="search"]::-webkit-search-decoration { -webkit-appearance: none; }
        body { margin: 0; background: ${COLORS.bg}; }
        /* iOS zooms the whole page in when it focuses a field smaller than
           16px. Every input here is styled inline, so this is one of the few
           places that has to shout to win. */
        @media (max-width: 860px) {
          input, textarea, select { font-size: 16px !important; }
        }
        /* A fingertip is a far blunter instrument than a mouse pointer, so
           give every control a real target on touch screens: 44px is the size
           both Apple and Google publish as the smallest one worth aiming at.
           Glyph-only controls need the width as much as the height — an arrow
           is barely a dozen pixels across on its own. */
        @media (pointer: coarse) {
          button,
          a[href],
          input:not([type="radio"]):not([type="checkbox"]) { min-height: 44px; }
          [data-icon-button] { min-width: 44px; }
        }
        /* Stops the 300ms wait for a possible second tap, which otherwise
           reads as the app being slow to answer. */
        button, a[href], [role="button"] { touch-action: manipulation; }
        ::-webkit-scrollbar { width: 10px; height: 10px; }
        ::-webkit-scrollbar-track { background: ${COLORS.bg}; }
        ::-webkit-scrollbar-thumb { background: ${COLORS.border}; border-radius: 5px; }
        ::-webkit-scrollbar-thumb:hover { background: ${COLORS.borderBright}; }
      `}</style>
      {children}
    </div>
  )
}
