import React, { useEffect, useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { api } from '../api'
import { Banner, Button, Input, Modal, PasswordInput, Spinner } from './ui'
import SshKeyField from './SshKeyField'

/* Bringing files in from a machine you have an SSH login on.

   This is the opposite direction from everything else that talks to somebody
   else's computer here, and the difference is worth keeping in view while
   reading it. A connected account is a place SAND *writes* — encrypted parts,
   under names it invents, meaningless on their own. A source is a place SAND
   *reads*: your own files, under paths you browse, never written to. The same
   VPS can be both, and if it is, it is two entries rather than one, so that an
   import browser cannot see the shard store.

   Two things are said plainly in the UI because both are surprising:

     · The host key is checked, always. The first connection learns the
       server's fingerprint and stores it; every one after that requires it.
       A machine that answers with a different key is refused rather than
       connected to, because a rebuilt VPS and somebody answering in its place
       are indistinguishable from here — and only the person who owns the
       server can tell them apart.
     · Interrupting an import loses nothing. Every file that arrived is
       already scattered and committed, and running the same import again
       skips those and carries on. There is no job to resume, only an import
       to repeat, which is why this dialog can afford to have no progress bar
       worth the name. */

export function ImportFromMachine({ path, vault = '', onClose, onChanged }) {
  const [sources, setSources] = useState(null)
  const [picked, setPicked] = useState(null)
  const [connecting, setConnecting] = useState(false)
  const [error, setError] = useState(null)

  const load = async () => {
    try {
      const resp = await api.sources()
      setSources(resp.sources || [])
    } catch (err) {
      setError(err.message)
      setSources([])
    }
  }

  useEffect(() => { load() }, [])

  const forget = async (source) => {
    setError(null)
    try {
      await api.forgetSource(source.id)
      if (picked?.id === source.id) setPicked(null)
      await load()
    } catch (err) {
      setError(err.message)
    }
  }

  if (picked) {
    return (
      <SourceBrowser
        source={picked}
        path={path}
        vault={vault}
        onBack={() => setPicked(null)}
        onClose={onClose}
        onChanged={onChanged}
      />
    )
  }

  return (
    <Modal
      title="Bring files in from a machine"
      subtitle={`Into ${path === '/' ? 'the vault' : path}`}
      onClose={onClose}
      width={620}
    >
      {error && <Banner tone="error">{error}</Banner>}

      {connecting
        ? (
          <ConnectSource
            onCancel={() => setConnecting(false)}
            onConnected={async (source) => {
              setConnecting(false)
              await load()
              setPicked(source)
            }}
          />
        )
        : sources === null
          ? <Spinner />
          : (
            <>
              {sources.length === 0
                ? (
                  <p style={{
                    fontFamily: FONT.sans, fontSize: '12.5px', lineHeight: 1.6,
                    color: COLORS.textDim, margin: '0 0 16px',
                  }}>
                    No machines yet. Anything you can <code style={{ margin: '0 3px' }}>sftp</code>
                    to will do — a VPS, a NAS, a storage box — and nothing has to be
                    installed on it. What you browse there is read, never written to:
                    files come here, get compressed, split, encrypted and scattered
                    across your accounts like any other upload.
                  </p>
                )
                : sources.map((source) => (
                  <SourceRow
                    key={source.id}
                    source={source}
                    onOpen={() => setPicked(source)}
                    onForget={() => forget(source)}
                  />
                ))}

              <div style={{ borderTop: `1px solid ${COLORS.border}`, paddingTop: '14px', marginTop: '4px' }}>
                <Button variant="primary" onClick={() => setConnecting(true)}>
                  + Connect a machine
                </Button>
              </div>
            </>
          )}
    </Modal>
  )
}

function SourceRow({ source, onOpen, onForget }) {
  return (
    <div style={{
      padding: '10px 12px',
      background: COLORS.bg,
      border: `1px solid ${COLORS.border}`,
      borderRadius: '6px',
      marginBottom: '10px',
    }}>
      <div style={{
        fontFamily: FONT.sans, fontSize: '13px', color: COLORS.text, marginBottom: '2px',
      }}>{source.name}</div>
      <div style={{
        fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textMuted,
        overflowWrap: 'anywhere', marginBottom: '8px',
      }}>
        {source.user}@{source.host}{source.port && source.port !== 22 ? `:${source.port}` : ''}
        {' · '}{source.root}
      </div>
      <div style={{ display: 'flex', gap: '8px' }}>
        <Button size="sm" variant="primary" onClick={onOpen}>Browse</Button>
        <Button size="sm" variant="ghost" onClick={onForget}>Forget</Button>
      </div>
    </div>
  )
}

/* The connect form.

   The key field is the interesting one, and it runs the opposite way from what
   a connect form usually does. Rather than asking for a private key that was
   made somewhere else, it offers to make the pair here and hands back the
   public half to install on the server — so what gets pasted is the half it
   does not matter about, in the direction where pasting the wrong one is
   harmless. Pasting your own key is one word away and unchanged. See
   SshKeyField. */
function ConnectSource({ onCancel, onConnected }) {
  const [form, setForm] = useState({
    name: '', host: '', port: '22', user: '', root: '',
    private_key: '', passphrase: '', host_key: '',
  })
  const [advanced, setAdvanced] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  const set = (key) => (e) => setForm({ ...form, [key]: e.target.value })

  const connect = async () => {
    setBusy(true)
    setError(null)
    try {
      const resp = await api.connectSource({
        name: form.name.trim() || form.host.trim(),
        host: form.host.trim(),
        port: Number(form.port) || 22,
        user: form.user.trim(),
        root: form.root.trim(),
        private_key: form.private_key,
        passphrase: form.passphrase,
        host_key: form.host_key.trim(),
      })
      onConnected(resp.source)
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  const ready = form.host.trim() && form.user.trim() && form.root.trim() && form.private_key.trim()

  return (
    <>
      {error && <Banner tone="error">{error}</Banner>}

      <Input label="Name" placeholder="the vps" value={form.name} disabled={busy}
        onChange={set('name')} help="What to call it here. Left empty, the hostname." />

      <div style={{ display: 'flex', gap: '10px' }}>
        <div style={{ flex: 3 }}>
          <Input label="Host *" placeholder="vps.example.com" value={form.host}
            disabled={busy} onChange={set('host')} />
        </div>
        <div style={{ flex: 1 }}>
          <Input label="Port" placeholder="22" value={form.port} disabled={busy} onChange={set('port')} />
        </div>
      </div>

      <Input label="Username *" placeholder="sand" value={form.user} disabled={busy} onChange={set('user')} />

      <Input label="Folder *" placeholder="/srv/media" value={form.root} disabled={busy}
        onChange={set('root')}
        help="The folder this source can see. Nothing outside it can be browsed or read — including through a symlink pointing out of it." />

      <SshKeyField
        label="Private key *"
        value={form.private_key}
        disabled={busy}
        keyName={form.name.trim() || form.host.trim()}
        onChange={(value) => setForm((current) => ({ ...current, private_key: value }))}
      />

      <PasswordInput label="Key passphrase" value={form.passphrase} disabled={busy}
        onChange={set('passphrase')}
        help="Only for a key you pasted that is encrypted. One SAND generated has no passphrase." />

      {advanced
        ? (
          <Input label="Host key fingerprint" placeholder="SHA256:…" value={form.host_key}
            disabled={busy} onChange={set('host_key')}
            help="Left empty, this first connection learns it and every one after requires it. Fill it in to have the first connection checked too: run ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub on the server." />
        )
        : (
          <button type="button" onClick={() => setAdvanced(true)} style={{
            background: 'none', border: 'none', padding: '0 0 12px', cursor: 'pointer',
            fontFamily: FONT.mono, fontSize: '10px', letterSpacing: '1px', color: COLORS.accent,
          }}>PIN THE HOST KEY YOURSELF…</button>
        )}

      <div style={{ display: 'flex', gap: '8px' }}>
        <Button variant="primary" onClick={connect} disabled={busy || !ready}>
          {busy ? 'Connecting…' : 'Connect'}
        </Button>
        <Button variant="ghost" onClick={onCancel} disabled={busy}>Cancel</Button>
      </div>

      <p style={{
        marginTop: '16px', paddingTop: '12px', borderTop: `1px solid ${COLORS.border}`,
        fontFamily: FONT.sans, fontSize: '11px', lineHeight: 1.55, color: COLORS.textMuted,
      }}>
        This connection learns the server's host key and stores it. Every
        connection after it requires the same one, and a machine answering with a
        different key is refused rather than connected to — a rebuilt server and
        somebody answering in its place look identical from here, and only you
        can tell which it was.
      </p>
    </>
  )
}

/* Browsing one machine and picking what to bring.

   The paths here are relative to the source's folder and never absolute: an API
   handing out absolute server paths would be inviting the browser to send one
   back. Everything above that folder is refused by the server however the path
   is written, so what this component does about it is simply not offer it. */
function SourceBrowser({ source, path, vault, onBack, onClose, onChanged }) {
  const [cwd, setCwd] = useState('')
  const [listing, setListing] = useState(null)
  const [picked, setPicked] = useState(() => new Set())
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const [summary, setSummary] = useState(null)

  useEffect(() => {
    let live = true
    setListing(null)
    setError(null)
    api.browseSource(source.id, cwd)
      .then((resp) => { if (live) setListing(resp) })
      .catch((err) => { if (live) { setError(err.message); setListing({ entries: [] }) } })
    return () => { live = false }
  }, [source.id, cwd])

  const toggle = (entry) => {
    const full = cwd ? `${cwd}/${entry.name}` : entry.name
    const next = new Set(picked)
    if (next.has(full)) next.delete(full)
    else next.add(full)
    setPicked(next)
  }

  const runImport = async () => {
    setBusy(true)
    setError(null)
    try {
      const resp = await api.importFromSource(source.id, {
        vault,
        paths: [...picked],
        dest: path,
      })
      setSummary(resp)
      setPicked(new Set())
      onChanged?.()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  const entries = listing?.entries || []

  return (
    <Modal
      title={source.name}
      subtitle={`${cwd ? `${source.root}/${cwd}` : source.root} → ${path === '/' ? 'the vault' : path}`}
      onClose={onClose}
      width={640}
    >
      {error && <Banner tone="error">{error}</Banner>}
      {summary && <ImportSummary summary={summary} onDismiss={() => setSummary(null)} />}

      <div style={{ display: 'flex', gap: '8px', marginBottom: '12px', alignItems: 'center' }}>
        <Button size="sm" variant="ghost" onClick={onBack}>← Machines</Button>
        {!listing?.at_root && (
          <Button size="sm" onClick={() => { setCwd(listing?.parent ?? ''); }}>↑ Up</Button>
        )}
        <span style={{
          fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textMuted,
          overflowWrap: 'anywhere',
        }}>{cwd || '/'}</span>
      </div>

      {listing === null
        ? <Spinner />
        : entries.length === 0
          ? (
            <p style={{
              fontFamily: FONT.sans, fontSize: '12.5px', color: COLORS.textDim, margin: '0 0 16px',
            }}>Nothing here.</p>
          )
          : (
            <div style={{
              maxHeight: '46vh', overflowY: 'auto', marginBottom: '12px',
              border: `1px solid ${COLORS.border}`, borderRadius: '6px',
            }}>
              {entries.map((entry) => (
                <RemoteEntry
                  key={entry.name}
                  entry={entry}
                  checked={picked.has(cwd ? `${cwd}/${entry.name}` : entry.name)}
                  disabled={busy}
                  onOpen={() => setCwd(cwd ? `${cwd}/${entry.name}` : entry.name)}
                  onToggle={() => toggle(entry)}
                />
              ))}
            </div>
          )}

      {listing?.truncated && (
        <Banner tone="warn">
          This folder holds more than can be listed at once, so what is above is
          only part of it. Narrow it down on the server, or import a subfolder at
          a time.
        </Banner>
      )}

      <div style={{ display: 'flex', gap: '8px', alignItems: 'center' }}>
        <Button variant="primary" onClick={runImport} disabled={busy || picked.size === 0}>
          {busy ? 'Bringing them in…' : `Import ${picked.size || ''}`.trim()}
        </Button>
        {picked.size > 0 && (
          <Button size="sm" variant="ghost" onClick={() => setPicked(new Set())} disabled={busy}>
            Clear
          </Button>
        )}
      </div>

      <p style={{
        marginTop: '16px', paddingTop: '12px', borderTop: `1px solid ${COLORS.border}`,
        fontFamily: FONT.sans, fontSize: '11px', lineHeight: 1.55, color: COLORS.textMuted,
      }}>
        A folder brings everything under it, keeping its shape. Nothing is
        removed from the machine. If this is interrupted, nothing is lost — every
        file that arrived is already scattered, and running the same import again
        skips those and carries on.
      </p>
    </Modal>
  )
}

function RemoteEntry({ entry, checked, disabled, onOpen, onToggle }) {
  const unreachable = !!entry.unreachable

  return (
    <div style={{
      display: 'flex', alignItems: 'center', gap: '10px',
      padding: '8px 10px',
      borderBottom: `1px solid ${COLORS.border}`,
      opacity: unreachable ? 0.55 : 1,
    }}>
      <input
        type="checkbox"
        checked={checked}
        disabled={disabled || unreachable}
        onChange={onToggle}
        aria-label={`Select ${entry.name}`}
      />
      <span style={{ fontSize: '13px', width: '16px', textAlign: 'center' }}>
        {entry.dir ? '📁' : entry.symlink ? '↪' : '📄'}
      </span>
      <button
        type="button"
        onClick={entry.dir && !unreachable ? onOpen : onToggle}
        disabled={disabled || unreachable}
        style={{
          flex: 1, textAlign: 'left', background: 'none', border: 'none', padding: 0,
          cursor: disabled || unreachable ? 'default' : 'pointer',
          fontFamily: FONT.sans, fontSize: '12.5px', color: COLORS.text,
          overflowWrap: 'anywhere',
        }}
      >
        {entry.name}
        {unreachable && (
          <span style={{ display: 'block', fontSize: '11px', color: COLORS.warn }}>
            {entry.reason}
          </span>
        )}
      </button>
      {!entry.dir && !unreachable && (
        <span style={{ fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textMuted }}>
          {formatBytes(entry.size)}
        </span>
      )}
    </div>
  )
}

/* What happened, one line per file where a line is worth having.

   Skipped is reported as loudly as imported, because on a second run it is the
   answer: it says the files are already here rather than that nothing
   happened. */
function ImportSummary({ summary, onDismiss }) {
  const failures = (summary.results || []).filter((r) => r.error)

  return (
    <Banner tone={failures.length ? 'warn' : 'info'} onDismiss={onDismiss}>
      <div>
        {summary.imported} brought in
        {summary.skipped ? `, ${summary.skipped} already here` : ''}
        {failures.length ? `, ${failures.length} failed` : ''}.
        {summary.truncated ? ' More was selected than one import can carry — run it again to continue.' : ''}
      </div>
      {failures.slice(0, 5).map((r) => (
        <div key={r.path} style={{
          fontFamily: FONT.mono, fontSize: '11px', marginTop: '4px', overflowWrap: 'anywhere',
        }}>{r.path}: {r.error}</div>
      ))}
    </Banner>
  )
}
