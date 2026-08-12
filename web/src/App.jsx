import React, { useCallback, useEffect, useState } from 'react'
import { COLORS, FONT } from './theme'
import { useIsMobile } from './hooks'
import { api } from './api'
import LockScreen from './components/LockScreen'
import AccountsPanel from './components/AccountsPanel'
import FileBrowser from './components/FileBrowser'
import PreviewModal, { ShardInspector } from './components/PreviewModal'
import { Brand, DevMark } from './components/Brand'
import { Banner, Button } from './components/ui'

/* SAND — a file browser over storage you do not fully trust.
   Files are compressed, split into three encrypted parts and scattered across
   separate cloud accounts. Opening one gathers the parts back and rebuilds it. */
export default function App() {
  const [status, setStatus] = useState(null)
  const [path, setPath] = useState('/')
  const [listing, setListing] = useState(null)
  const [providers, setProviders] = useState([])
  const [loadingList, setLoadingList] = useState(false)
  const [loadingProviders, setLoadingProviders] = useState(false)
  const [error, setError] = useState(null)
  const [preview, setPreview] = useState(null)
  const [inspecting, setInspecting] = useState(null)
  const [accountsOpen, setAccountsOpen] = useState(false)

  const mobile = useIsMobile()
  const unlocked = !!status?.unlocked

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
        // The folder went away underneath us — fall back to the root.
        setPath('/')
      } else {
        setError(err.message)
      }
    } finally {
      setLoadingList(false)
    }
  }, [path])

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

  const lock = async () => {
    await api.lock().catch(() => {})
    setStatus((s) => ({ ...s, unlocked: false, stats: null }))
    setListing(null)
    setProviders([])
    setPreview(null)
    setInspecting(null)
    setAccountsOpen(false)
    setPath('/')
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
          gap: mobile ? '8px' : '14px',
          padding: mobile ? '8px 12px' : '10px 20px',
          borderBottom: `1px solid ${COLORS.border}`,
          background: COLORS.surface,
          flexShrink: 0,
        }}>
          {mobile && (
            <Button
              size="sm"
              variant="ghost"
              aria-label="Connected clouds"
              onClick={() => setAccountsOpen(true)}
              style={{ fontSize: '15px', padding: '4px 8px' }}
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
            }}>multi-cloud encrypted file store</span>
          )}

          <span style={{ flex: 1 }} />

          {/* Narrow enough and the labels are dropped; the glyphs carry the
              meaning and the accessible name comes off aria-label. */}
          <Button size="sm" variant="ghost" onClick={refreshAll}
            title="Refresh" aria-label="Refresh">⟳{mobile ? '' : ' Refresh'}</Button>
          <Button size="sm" onClick={lock}
            title="Lock vault" aria-label="Lock vault">🔒{mobile ? '' : ' Lock vault'}</Button>
          {/* No room for the developer mark up here on a phone — it moves to
              the foot of the accounts drawer instead. */}
          {!mobile && <DevMark />}
        </header>

        <div style={{ flex: 1, display: 'flex', minHeight: 0, position: 'relative' }}>
          <AccountsPanel
            providers={providers}
            loading={loadingProviders}
            stats={status.stats}
            mobile={mobile}
            open={accountsOpen}
            onClose={() => setAccountsOpen(false)}
            onRefresh={refreshProviders}
            onChanged={refreshAll}
          />

          <FileBrowser
            path={path}
            listing={listing}
            loading={loadingList}
            error={error}
            providers={providers}
            mobile={mobile}
            onNavigate={setPath}
            onRefresh={refreshAll}
            onPreview={setPreview}
            onInspect={setInspecting}
            onError={setError}
          />
        </div>
      </div>

      {preview && <PreviewModal file={preview} onClose={() => setPreview(null)} />}
      {inspecting && <ShardInspector file={inspecting} onClose={() => setInspecting(null)} />}
    </Shell>
  )
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
        /* The cross-fade stays — it isn't motion — but the scale doesn't. */
        @media (prefers-reduced-motion: reduce) {
          .sand-dev-lockup { animation: none !important; }
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
        body { margin: 0; background: ${COLORS.bg}; }
        /* iOS zooms the whole page in when it focuses a field smaller than
           16px. Every input here is styled inline, so this is one of the few
           places that has to shout to win. */
        @media (max-width: 860px) {
          input, textarea, select { font-size: 16px !important; }
        }
        /* A fingertip is a far blunter instrument than a mouse pointer, so
           give every control a real target on touch screens. */
        @media (pointer: coarse) {
          button,
          a[href],
          input:not([type="radio"]):not([type="checkbox"]) { min-height: 40px; }
        }
        ::-webkit-scrollbar { width: 10px; height: 10px; }
        ::-webkit-scrollbar-track { background: ${COLORS.bg}; }
        ::-webkit-scrollbar-thumb { background: ${COLORS.border}; border-radius: 5px; }
        ::-webkit-scrollbar-thumb:hover { background: ${COLORS.borderBright}; }
      `}</style>
      {children}
    </div>
  )
}
