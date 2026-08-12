import React, { useCallback, useEffect, useState } from 'react'
import { COLORS, FONT } from './theme'
import { api } from './api'
import LockScreen from './components/LockScreen'
import AccountsPanel from './components/AccountsPanel'
import FileBrowser from './components/FileBrowser'
import PreviewModal, { ShardInspector } from './components/PreviewModal'
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

  const unlocked = !!status?.unlocked

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
    setPath('/')
  }

  if (!status) {
    return <Shell><div style={{ padding: '80px', textAlign: 'center', color: COLORS.textMuted }}>Loading…</div></Shell>
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
      <div style={{ height: '100vh', display: 'flex', flexDirection: 'column' }}>
        <header style={{
          display: 'flex',
          alignItems: 'center',
          gap: '14px',
          padding: '12px 20px',
          borderBottom: `1px solid ${COLORS.border}`,
          background: COLORS.surface,
        }}>
          <span style={{
            fontFamily: FONT.mono,
            fontSize: '16px',
            fontWeight: 700,
            letterSpacing: '5px',
            color: COLORS.accent,
          }}>▣ SAND</span>
          <span style={{
            fontFamily: FONT.mono,
            fontSize: '10px',
            letterSpacing: '1.5px',
            textTransform: 'uppercase',
            color: COLORS.textMuted,
          }}>multi-cloud encrypted file store</span>

          <span style={{ flex: 1 }} />

          <Button size="sm" variant="ghost" onClick={refreshAll}>⟳ Refresh</Button>
          <Button size="sm" onClick={lock}>🔒 Lock vault</Button>
        </header>

        <div style={{ flex: 1, display: 'flex', minHeight: 0 }}>
          <AccountsPanel
            providers={providers}
            loading={loadingProviders}
            stats={status.stats}
            onRefresh={refreshProviders}
            onChanged={refreshAll}
          />

          <FileBrowser
            path={path}
            listing={listing}
            loading={loadingList}
            error={error}
            providers={providers}
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
        * { box-sizing: border-box; }
        body { margin: 0; background: ${COLORS.bg}; }
        ::-webkit-scrollbar { width: 10px; height: 10px; }
        ::-webkit-scrollbar-track { background: ${COLORS.bg}; }
        ::-webkit-scrollbar-thumb { background: ${COLORS.border}; border-radius: 5px; }
        ::-webkit-scrollbar-thumb:hover { background: ${COLORS.borderBright}; }
      `}</style>
      {children}
    </div>
  )
}
