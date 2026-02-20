import React, { useState, useRef, useCallback } from 'react'

/* -----------------------------------------------------------------------
   SAND Web GUI — A minimal, terminal-inspired interface
   for archiving and restoring files with SAND.
   ----------------------------------------------------------------------- */

const COLORS = {
  bg: '#0a0e17',
  surface: '#111827',
  surfaceHover: '#1a2332',
  border: '#1e2d3d',
  borderActive: '#d97706',
  text: '#e2e8f0',
  textMuted: '#64748b',
  accent: '#d97706',
  accentDim: '#92400e',
  error: '#ef4444',
  success: '#22c55e',
  white: '#ffffff',
}

const font = {
  mono: "'JetBrains Mono', 'Fira Code', monospace",
  sans: "'DM Sans', system-ui, sans-serif",
}

/* ---- Styles ---- */
const styles = {
  container: {
    minHeight: '100vh',
    backgroundColor: COLORS.bg,
    color: COLORS.text,
    fontFamily: font.sans,
    display: 'flex',
    flexDirection: 'column',
    alignItems: 'center',
    padding: '0',
    margin: 0,
  },
  header: {
    width: '100%',
    borderBottom: `1px solid ${COLORS.border}`,
    padding: '20px 0',
    textAlign: 'center',
  },
  logo: {
    fontFamily: font.mono,
    fontSize: '28px',
    fontWeight: 700,
    color: COLORS.accent,
    letterSpacing: '6px',
    margin: 0,
  },
  subtitle: {
    fontFamily: font.mono,
    fontSize: '11px',
    color: COLORS.textMuted,
    letterSpacing: '3px',
    textTransform: 'uppercase',
    marginTop: '6px',
  },
  main: {
    maxWidth: '600px',
    width: '100%',
    padding: '40px 24px',
  },
  tabs: {
    display: 'flex',
    gap: '0',
    marginBottom: '32px',
    borderBottom: `1px solid ${COLORS.border}`,
  },
  tab: (active) => ({
    flex: 1,
    padding: '14px 0',
    textAlign: 'center',
    cursor: 'pointer',
    fontFamily: font.mono,
    fontSize: '13px',
    fontWeight: 600,
    letterSpacing: '2px',
    textTransform: 'uppercase',
    color: active ? COLORS.accent : COLORS.textMuted,
    borderBottom: active ? `2px solid ${COLORS.accent}` : '2px solid transparent',
    background: 'none',
    border: 'none',
    borderBottomWidth: '2px',
    borderBottomStyle: 'solid',
    borderBottomColor: active ? COLORS.accent : 'transparent',
    transition: 'all 0.2s ease',
  }),
  dropzone: (isDragging, hasFile) => ({
    border: `2px dashed ${isDragging ? COLORS.accent : hasFile ? COLORS.success : COLORS.border}`,
    borderRadius: '8px',
    padding: '40px 20px',
    textAlign: 'center',
    cursor: 'pointer',
    transition: 'all 0.2s ease',
    backgroundColor: isDragging ? COLORS.surfaceHover : COLORS.surface,
    marginBottom: '20px',
  }),
  dropzoneLabel: {
    fontFamily: font.mono,
    fontSize: '13px',
    color: COLORS.textMuted,
  },
  dropzoneFile: {
    fontFamily: font.mono,
    fontSize: '14px',
    color: COLORS.success,
    marginTop: '8px',
  },
  dropzoneIcon: {
    fontSize: '36px',
    marginBottom: '12px',
    opacity: 0.7,
  },
  inputGroup: {
    marginBottom: '20px',
  },
  label: {
    display: 'block',
    fontFamily: font.mono,
    fontSize: '11px',
    fontWeight: 600,
    letterSpacing: '2px',
    textTransform: 'uppercase',
    color: COLORS.textMuted,
    marginBottom: '8px',
  },
  passwordWrapper: {
    position: 'relative',
    display: 'flex',
    alignItems: 'center',
  },
  input: {
    width: '100%',
    padding: '12px 44px 12px 14px',
    backgroundColor: COLORS.surface,
    border: `1px solid ${COLORS.border}`,
    borderRadius: '6px',
    color: COLORS.text,
    fontFamily: font.mono,
    fontSize: '14px',
    outline: 'none',
    boxSizing: 'border-box',
    transition: 'border-color 0.2s ease',
  },
  eyeBtn: {
    position: 'absolute',
    right: '10px',
    background: 'none',
    border: 'none',
    color: COLORS.textMuted,
    cursor: 'pointer',
    fontSize: '16px',
    padding: '4px',
  },
  button: (disabled) => ({
    width: '100%',
    padding: '14px',
    backgroundColor: disabled ? COLORS.accentDim : COLORS.accent,
    color: COLORS.bg,
    border: 'none',
    borderRadius: '6px',
    fontFamily: font.mono,
    fontSize: '13px',
    fontWeight: 700,
    letterSpacing: '2px',
    textTransform: 'uppercase',
    cursor: disabled ? 'not-allowed' : 'pointer',
    transition: 'all 0.2s ease',
    opacity: disabled ? 0.6 : 1,
  }),
  progress: {
    marginTop: '24px',
    padding: '16px',
    backgroundColor: COLORS.surface,
    borderRadius: '6px',
    border: `1px solid ${COLORS.border}`,
  },
  progressBar: {
    height: '4px',
    backgroundColor: COLORS.border,
    borderRadius: '2px',
    overflow: 'hidden',
    marginBottom: '10px',
  },
  progressFill: (pct) => ({
    height: '100%',
    width: `${pct}%`,
    backgroundColor: COLORS.accent,
    borderRadius: '2px',
    transition: 'width 0.3s ease',
  }),
  progressText: {
    fontFamily: font.mono,
    fontSize: '12px',
    color: COLORS.textMuted,
  },
  result: (isError) => ({
    marginTop: '24px',
    padding: '16px',
    backgroundColor: COLORS.surface,
    borderRadius: '6px',
    border: `1px solid ${isError ? COLORS.error : COLORS.success}`,
  }),
  resultText: (isError) => ({
    fontFamily: font.mono,
    fontSize: '13px',
    color: isError ? COLORS.error : COLORS.success,
    margin: 0,
  }),
  downloadLink: {
    display: 'inline-block',
    marginTop: '10px',
    fontFamily: font.mono,
    fontSize: '13px',
    color: COLORS.accent,
    textDecoration: 'underline',
    cursor: 'pointer',
  },
  partsIndicator: {
    display: 'flex',
    gap: '10px',
    marginBottom: '16px',
  },
  partBadge: (present) => ({
    flex: 1,
    padding: '8px',
    textAlign: 'center',
    fontFamily: font.mono,
    fontSize: '11px',
    fontWeight: 600,
    borderRadius: '4px',
    backgroundColor: present ? 'rgba(34, 197, 94, 0.1)' : COLORS.surface,
    border: `1px solid ${present ? COLORS.success : COLORS.border}`,
    color: present ? COLORS.success : COLORS.textMuted,
  }),
  footer: {
    marginTop: 'auto',
    padding: '20px',
    textAlign: 'center',
    fontFamily: font.mono,
    fontSize: '11px',
    color: COLORS.textMuted,
    borderTop: `1px solid ${COLORS.border}`,
    width: '100%',
  },
}

/* ---- Helper ---- */
function formatBytes(bytes) {
  if (bytes === 0) return '0 B'
  const k = 1024
  const sizes = ['B', 'KB', 'MB', 'GB']
  const i = Math.floor(Math.log(bytes) / Math.log(k))
  return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i]
}

function detectParts(files) {
  const parts = { 1: false, 2: false, 3: false }
  for (const f of files) {
    const name = f.name || f
    if (name.endsWith('.media1')) parts[1] = true
    else if (name.endsWith('.media2')) parts[2] = true
    else if (name.endsWith('.media3')) parts[3] = true
  }
  return parts
}

/* ---- DropZone component ---- */
function DropZone({ onFiles, multiple, accept, files }) {
  const [isDragging, setIsDragging] = useState(false)
  const inputRef = useRef(null)

  const handleDrag = useCallback((e) => {
    e.preventDefault()
    e.stopPropagation()
  }, [])

  const handleDragIn = useCallback((e) => {
    e.preventDefault()
    setIsDragging(true)
  }, [])

  const handleDragOut = useCallback((e) => {
    e.preventDefault()
    setIsDragging(false)
  }, [])

  const handleDrop = useCallback((e) => {
    e.preventDefault()
    setIsDragging(false)
    const dropped = Array.from(e.dataTransfer.files)
    if (dropped.length > 0) onFiles(dropped)
  }, [onFiles])

  const hasFiles = files && files.length > 0

  return (
    <div
      style={styles.dropzone(isDragging, hasFiles)}
      onDragEnter={handleDragIn}
      onDragOver={handleDrag}
      onDragLeave={handleDragOut}
      onDrop={handleDrop}
      onClick={() => inputRef.current?.click()}
    >
      <input
        ref={inputRef}
        type="file"
        multiple={multiple}
        accept={accept}
        style={{ display: 'none' }}
        onChange={(e) => onFiles(Array.from(e.target.files))}
      />
      <div style={styles.dropzoneIcon}>{hasFiles ? '✓' : '↑'}</div>
      <div style={styles.dropzoneLabel}>
        {hasFiles
          ? null
          : multiple
            ? 'Drop 2-3 .media files or click to browse'
            : 'Drop file here or click to browse'}
      </div>
      {hasFiles && files.map((f, i) => (
        <div key={i} style={styles.dropzoneFile}>
          {f.name} ({formatBytes(f.size)})
        </div>
      ))}
    </div>
  )
}

/* ---- Archive Tab ---- */
function ArchiveTab() {
  const [file, setFile] = useState(null)
  const [password, setPassword] = useState('')
  const [showPw, setShowPw] = useState(false)
  const [status, setStatus] = useState(null) // { type: 'progress'|'success'|'error', msg, pct? }

  const handleArchive = async () => {
    if (!file || !password) return
    setStatus({ type: 'progress', msg: 'Compressing and encrypting...', pct: 30 })

    try {
      const form = new FormData()
      form.append('file', file)
      form.append('password', password)

      setStatus({ type: 'progress', msg: 'Uploading and processing...', pct: 60 })

      const resp = await fetch('/api/archive', { method: 'POST', body: form })

      if (!resp.ok) {
        const err = await resp.json()
        throw new Error(err.error || 'Archive failed')
      }

      setStatus({ type: 'progress', msg: 'Preparing download...', pct: 90 })

      const blob = await resp.blob()
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = `${file.name}.sand.zip`
      a.click()
      URL.revokeObjectURL(url)

      setStatus({ type: 'success', msg: `Archived into 3 encrypted parts — ${file.name}.sand.zip` })
    } catch (err) {
      setStatus({ type: 'error', msg: err.message })
    }
  }

  return (
    <div>
      <DropZone
        onFiles={(files) => setFile(files[0])}
        multiple={false}
        files={file ? [file] : []}
      />

      <div style={styles.inputGroup}>
        <label style={styles.label}>Password</label>
        <div style={styles.passwordWrapper}>
          <input
            type={showPw ? 'text' : 'password'}
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            placeholder="Enter encryption password"
            style={styles.input}
            onFocus={(e) => e.target.style.borderColor = COLORS.accent}
            onBlur={(e) => e.target.style.borderColor = COLORS.border}
          />
          <button style={styles.eyeBtn} onClick={() => setShowPw(!showPw)}>
            {showPw ? '◉' : '◎'}
          </button>
        </div>
      </div>

      <button
        style={styles.button(!file || !password || status?.type === 'progress')}
        disabled={!file || !password || status?.type === 'progress'}
        onClick={handleArchive}
      >
        ▶ Archive
      </button>

      {status?.type === 'progress' && (
        <div style={styles.progress}>
          <div style={styles.progressBar}>
            <div style={styles.progressFill(status.pct)} />
          </div>
          <div style={styles.progressText}>{status.msg}</div>
        </div>
      )}

      {(status?.type === 'success' || status?.type === 'error') && (
        <div style={styles.result(status.type === 'error')}>
          <p style={styles.resultText(status.type === 'error')}>
            {status.type === 'success' ? '✓ ' : '✗ '}{status.msg}
          </p>
        </div>
      )}
    </div>
  )
}

/* ---- Restore Tab ---- */
function RestoreTab() {
  const [files, setFiles] = useState([])
  const [password, setPassword] = useState('')
  const [showPw, setShowPw] = useState(false)
  const [status, setStatus] = useState(null)

  const parts = detectParts(files)
  const partCount = Object.values(parts).filter(Boolean).length

  const handleRestore = async () => {
    if (partCount < 2 || !password) return
    setStatus({ type: 'progress', msg: 'Decrypting and reconstructing...', pct: 50 })

    try {
      const form = new FormData()
      for (const f of files) {
        form.append('parts[]', f)
      }
      form.append('password', password)

      const resp = await fetch('/api/restore', { method: 'POST', body: form })

      if (!resp.ok) {
        const err = await resp.json()
        const msg = err.code === 'WRONG_PASSWORD'
          ? 'Wrong password — decryption failed'
          : err.code === 'MISMATCHED_PARTS'
            ? 'Parts belong to different archives'
            : err.error || 'Restore failed'
        throw new Error(msg)
      }

      setStatus({ type: 'progress', msg: 'Preparing download...', pct: 90 })

      const disposition = resp.headers.get('Content-Disposition') || ''
      const match = disposition.match(/filename="?([^"]+)"?/)
      const filename = match ? match[1] : 'restored_file'

      const blob = await resp.blob()
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = filename
      a.click()
      URL.revokeObjectURL(url)

      setStatus({ type: 'success', msg: `Restored: ${filename} (${formatBytes(blob.size)})` })
    } catch (err) {
      setStatus({ type: 'error', msg: err.message })
    }
  }

  return (
    <div>
      <div style={styles.partsIndicator}>
        {[1, 2, 3].map(n => (
          <div key={n} style={styles.partBadge(parts[n])}>
            {parts[n] ? '●' : '○'} Part {n}
          </div>
        ))}
      </div>

      <DropZone
        onFiles={(newFiles) => setFiles(prev => {
          const all = [...prev, ...newFiles]
          // Deduplicate by name
          const seen = new Set()
          return all.filter(f => {
            if (seen.has(f.name)) return false
            seen.add(f.name)
            return true
          })
        })}
        multiple={true}
        accept=".media1,.media2,.media3"
        files={files}
      />

      {files.length > 0 && (
        <div style={{ textAlign: 'right', marginTop: '-12px', marginBottom: '16px' }}>
          <button
            style={{ ...styles.eyeBtn, position: 'static', fontSize: '12px', fontFamily: font.mono }}
            onClick={() => { setFiles([]); setStatus(null) }}
          >
            ✕ Clear
          </button>
        </div>
      )}

      <div style={styles.inputGroup}>
        <label style={styles.label}>Password</label>
        <div style={styles.passwordWrapper}>
          <input
            type={showPw ? 'text' : 'password'}
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            placeholder="Enter decryption password"
            style={styles.input}
            onFocus={(e) => e.target.style.borderColor = COLORS.accent}
            onBlur={(e) => e.target.style.borderColor = COLORS.border}
          />
          <button style={styles.eyeBtn} onClick={() => setShowPw(!showPw)}>
            {showPw ? '◉' : '◎'}
          </button>
        </div>
      </div>

      <button
        style={styles.button(partCount < 2 || !password || status?.type === 'progress')}
        disabled={partCount < 2 || !password || status?.type === 'progress'}
        onClick={handleRestore}
      >
        ▶ Restore
      </button>

      {partCount >= 2 && partCount < 3 && (
        <div style={{ marginTop: '12px', fontFamily: font.mono, fontSize: '11px', color: COLORS.textMuted, textAlign: 'center' }}>
          {partCount} of 3 parts provided — sufficient for recovery
        </div>
      )}

      {status?.type === 'progress' && (
        <div style={styles.progress}>
          <div style={styles.progressBar}>
            <div style={styles.progressFill(status.pct)} />
          </div>
          <div style={styles.progressText}>{status.msg}</div>
        </div>
      )}

      {(status?.type === 'success' || status?.type === 'error') && (
        <div style={styles.result(status.type === 'error')}>
          <p style={styles.resultText(status.type === 'error')}>
            {status.type === 'success' ? '✓ ' : '✗ '}{status.msg}
          </p>
        </div>
      )}
    </div>
  )
}

/* ---- Main App ---- */
export default function App() {
  const [tab, setTab] = useState('archive')

  return (
    <div style={styles.container}>
      <header style={styles.header}>
        <h1 style={styles.logo}>▣ SAND</h1>
        <p style={styles.subtitle}>Secure Archival Network Distribution</p>
      </header>

      <main style={styles.main}>
        <div style={styles.tabs}>
          <button
            style={styles.tab(tab === 'archive')}
            onClick={() => setTab('archive')}
          >
            📦 Archive
          </button>
          <button
            style={styles.tab(tab === 'restore')}
            onClick={() => setTab('restore')}
          >
            📂 Restore
          </button>
        </div>

        {tab === 'archive' ? <ArchiveTab /> : <RestoreTab />}
      </main>

      <footer style={styles.footer}>
        AES-256-GCM · Argon2id · zstd · XOR redundancy · any 2 of 3 parts restore
      </footer>
    </div>
  )
}
