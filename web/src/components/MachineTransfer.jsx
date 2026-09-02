import React, { useEffect, useRef, useState } from 'react'
import { COLORS, FONT, formatBytes } from '../theme'
import { api } from '../api'
import { Banner, Button, Input, Modal, PasswordInput, Spinner } from './ui'
import SshKeyField from './SshKeyField'

/* Moving files between the vault and a machine you have an SSH login on, in
   either direction.

   A connected account is a place SAND *writes* — encrypted parts, under names
   it invents, meaningless on their own. A machine here is a place SAND reads
   your own files from and, since it can go both ways, writes your own files
   to: whole, decrypted, under the names they carry. The same VPS can be both
   an account and a machine, and if it is, it is two entries rather than one,
   so that a browser over your files cannot see the shard store and nothing
   that writes your files can write into it.

   Two things are said plainly in the UI because both are surprising:

     · The host key is checked, always. The first connection learns the
       server's fingerprint and stores it; every one after that requires it.
       A machine that answers with a different key is refused rather than
       connected to, because a rebuilt VPS and somebody answering in its place
       are indistinguishable from here — and only the person who owns the
       server can tell them apart.
     · Sending out writes plaintext. Everything else SAND puts on somebody
       else's disk is a shard that means nothing alone; an export is the one
       thing that undoes that, on purpose, and the dialog says so beside the
       button rather than in a manual.

   Both directions resume the same way: interrupting a transfer loses whole
   files only where it has to. Every file that arrived is committed — scattered
   into the vault, or renamed into place on the machine — and running the same
   transfer again skips those and carries on. There is no job to resume, only
   a transfer to repeat.

   The progress bar is a view of the request that is running and not a step
   towards a job framework: the server keeps it in memory for exactly as long
   as the transfer does, this dialog polls it, and nothing is written down. A
   transfer can also be detached, which is the one thing here that outlives
   the page: it runs on the machine with nothing behind it, this dialog shows
   it again on the way back, and stopping it is a button rather than a closed
   tab. Restarting SAND still ends it, and the answer to that is the answer to
   everything else in this file — run the same transfer again. */

/* `mode` is which way the bytes go: 'import' brings the machine's files into
   `path`, 'export' sends what is picked from the vault out. `preset` is a
   selection already made — the folder whose menu opened this — so the export
   skips straight to choosing where on the machine it lands. */
export function MachineTransfer({ path, vault = '', mode: initialMode = 'import', preset = null, onClose, onChanged }) {
  const [mode, setMode] = useState(initialMode)
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

  const here = path === '/' ? (vault ? `the ${vault} vault` : 'the vault') : path

  if (picked) {
    return (
      <SourceBrowser
        source={picked}
        mode={mode}
        onMode={setMode}
        path={path}
        vault={vault}
        preset={preset}
        onBack={() => setPicked(null)}
        onClose={onClose}
        onChanged={onChanged}
      />
    )
  }

  return (
    <Modal
      title="A machine you have a login on"
      subtitle={mode === 'import' ? `Bring files into ${here}` : `Send files from ${here}`}
      onClose={onClose}
      width={620}
    >
      {error && <Banner tone="error">{error}</Banner>}

      {!connecting && <DirectionSwitch mode={mode} onMode={setMode} />}

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
                    installed on it. Files brought in are compressed, split, encrypted
                    and scattered across your accounts like any other upload; files
                    sent out land on the machine whole and readable, which is the
                    point of sending them.
                  </p>
                )
                : sources.map((source) => (
                  <SourceRow
                    key={source.id}
                    source={source}
                    mode={mode}
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

/* Which way. Two buttons rather than a tab strip, so the one that is not
   chosen still reads as a thing you can do from here. */
function DirectionSwitch({ mode, onMode, disabled }) {
  const option = (value, glyph, label, hint) => {
    const on = mode === value
    return (
      <button
        type="button"
        onClick={() => !disabled && onMode(value)}
        disabled={disabled}
        aria-pressed={on}
        style={{
          flex: 1, display: 'flex', flexDirection: 'column', gap: '2px',
          padding: '8px 10px', textAlign: 'left',
          background: on ? `${COLORS.accent}18` : COLORS.bg,
          border: `1px solid ${on ? COLORS.accent : COLORS.border}`,
          borderRadius: '6px', cursor: disabled ? 'default' : 'pointer',
          opacity: disabled && !on ? 0.5 : 1,
        }}
      >
        <span style={{
          fontFamily: FONT.mono, fontSize: '11px', letterSpacing: '1px',
          color: on ? COLORS.accent : COLORS.textDim,
        }}>{glyph} {label}</span>
        <span style={{ fontFamily: FONT.sans, fontSize: '11px', color: COLORS.textMuted }}>{hint}</span>
      </button>
    )
  }

  return (
    <div style={{ display: 'flex', gap: '8px', marginBottom: '14px' }}>
      {option('import', '↓', 'BRING IN', 'The machine\'s files, into the vault')}
      {option('export', '↑', 'SEND OUT', 'The vault\'s files, onto the machine — in the clear')}
    </div>
  )
}

function SourceRow({ source, mode, onOpen, onForget }) {
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
        <Button size="sm" variant="primary" onClick={onOpen}>
          {mode === 'import' ? 'Browse' : 'Send here'}
        </Button>
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
        help="The folder this machine can be browsed and written under. Nothing outside it can be seen or touched — including through a symlink pointing out of it." />

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

/* Browsing one machine: picking what to bring, or where to put what is going.

   The paths here are relative to the machine's folder and never absolute: an
   API handing out absolute server paths would be inviting the browser to send
   one back. Everything above that folder is refused by the server however the
   path is written, so what this component does about it is simply not offer
   it.

   In the import direction the listing is what you pick from; in the export
   direction it is where you are standing, and the button sends the vault's
   selection here. Both draw the same rows. */
function SourceBrowser({ source, mode, onMode, path, vault, preset, onBack, onClose, onChanged }) {
  const [cwd, setCwd] = useState('')
  const [listing, setListing] = useState(null)
  const [reload, setReload] = useState(0)
  const [picked, setPicked] = useState(() => new Set())
  const [items, setItems] = useState(preset || [])
  const [choosing, setChoosing] = useState(mode === 'export' && !preset)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const [summary, setSummary] = useState(null)
  const [runs, setRuns] = useState([])
  const [detach, setDetach] = useState(false)
  const [overwrite, setOverwrite] = useState(false)

  useEffect(() => {
    let live = true
    setListing(null)
    setError(null)
    api.browseSource(source.id, cwd)
      .then((resp) => { if (live) setListing(resp) })
      .catch((err) => { if (live) { setError(err.message); setListing({ entries: [] }) } })
    return () => { live = false }
  }, [source.id, cwd, reload])

  // Changing direction with nothing picked yet opens the picker for a send
  // and closes it for a bring-in; a selection already made is kept.
  useEffect(() => {
    if (mode === 'export' && items.length === 0) setChoosing(true)
    if (mode === 'import') setChoosing(false)
  }, [mode]) // eslint-disable-line react-hooks/exhaustive-deps

  /* Ask the server what this machine has going, and keep asking.

     A second, short request rather than anything pushed down the transfer's
     own: a transfer is a plain POST, and this leaves it exactly that. The
     server answers out of memory, so the poll costs nothing and cannot slow
     the transfer it is watching.

     Both directions are asked, because a detached export started from this
     dialog and an import still running from another are both this machine's
     business. It runs whenever this view is open rather than only while this
     dialog is the one that started something, because a detached transfer
     outlives the page — coming back to a machine has to show what it is
     doing, and the result of what it did. Once a second while something is
     moving, every five when nothing is. */
  useEffect(() => {
    let live = true
    const ask = async () => {
      try {
        const [imports, exports] = await Promise.all([
          api.sourceImports(source.id), api.sourceExports(source.id),
        ])
        if (live) {
          const all = [...(imports.imports || []), ...(exports.exports || [])]
          all.sort((a, b) => (a.started_at < b.started_at ? -1 : 1))
          setRuns(all)
        }
      } catch {
        // A poll that failed says nothing about a transfer, which is running
        // behind its own request either way. Leave the last answer up.
      }
    }
    ask()
    const timer = setInterval(ask, busy || runs.some((run) => !run.done) ? 1000 : 5000)
    return () => { live = false; clearInterval(timer) }
  }, [source.id, busy, runs.some((run) => !run.done)]) // eslint-disable-line react-hooks/exhaustive-deps

  /* A detached transfer that has just finished has changed something the
     page behind this dialog knows nothing about: an import put files in the
     vault, an export put files on the machine being browsed. Refreshing on
     the transition — rather than on every poll — is what keeps that from
     being a list that redraws itself once a second all night. */
  const finished = useRef(new Set())
  useEffect(() => {
    let landed = null
    for (const run of runs) {
      if (run.done && !finished.current.has(run.id)) {
        finished.current.add(run.id)
        landed = run.kind
      }
    }
    if (landed === 'import') onChanged?.()
    if (landed === 'export') setReload((n) => n + 1)
  }, [runs, onChanged])

  // Stopping a running transfer and dismissing a finished one's result are
  // the same request, because they are the same gesture: stop showing me this.
  const stopRun = async (run) => {
    setError(null)
    try {
      if (run.kind === 'export') await api.stopExport(source.id, run.id)
      else await api.stopImport(source.id, run.id)
      setRuns((current) => current.filter((r) => r.id !== run.id || !r.done))
    } catch (err) {
      setError(err.message)
    }
  }

  const toggle = (entry) => {
    const full = cwd ? `${cwd}/${entry.name}` : entry.name
    const next = new Set(picked)
    if (next.has(full)) next.delete(full)
    else next.add(full)
    setPicked(next)
  }

  const dropped = (err, verb) => (err instanceof api.ApiError
    ? err.message
    : `The connection to SAND dropped, so this ${verb} stopped. Every file `
      + 'that arrived whole is where it was going — run the same one again to '
      + 'carry on from there, or tick "Keep going if I close this page" to '
      + 'have the machine finish it without the browser.')

  const runImport = async () => {
    setBusy(true)
    setError(null)
    try {
      const resp = await api.importFromSource(source.id, {
        vault,
        paths: [...picked],
        dest: path,
        detach,
      })
      // Detached, the answer is the run rather than the result: the transfer
      // has not happened yet, and what it comes to arrives on the poll. The
      // summary banner is for the request that waited for its own answer.
      if (detach) setRuns((current) => [...current, resp.run].filter(Boolean))
      else setSummary({ kind: 'import', ...resp })
      setPicked(new Set())
      onChanged?.()
    } catch (err) {
      /* A dropped connection is not an error about the import. It is the
         import's own request having gone away — the page navigated, the phone
         put the browser to sleep — which on a foreground transfer takes it
         with it. The browser's word for that is "load failed", which explains
         nothing and suggests something is broken. Say what happened and what
         the box above is for. */
      setError(dropped(err, 'import'))
    } finally {
      setBusy(false)
    }
  }

  const runExport = async () => {
    setBusy(true)
    setError(null)
    try {
      const resp = await api.exportToSource(source.id, {
        vault,
        paths: items.map((item) => item.path),
        dest: cwd,
        overwrite,
        detach,
      })
      if (detach) setRuns((current) => [...current, resp.run].filter(Boolean))
      else setSummary({ kind: 'export', ...resp })
      // The machine's listing has new files in it now.
      setReload((n) => n + 1)
    } catch (err) {
      setError(dropped(err, 'export'))
    } finally {
      setBusy(false)
    }
  }

  const entries = listing?.entries || []
  const exporting = mode === 'export'
  const machineHere = cwd ? `${source.root}/${cwd}` : source.root
  const vaultHere = path === '/' ? 'the vault' : path
  // A folder's weight is in the levels below it and is not known here, so a
  // size is shown only when every item is a file — a total that left the
  // folders out would read as smaller than what is about to move.
  const totalBytes = items.every((item) => item.kind === 'file')
    ? items.reduce((sum, item) => sum + (item.size || 0), 0)
    : 0

  if (exporting && choosing) {
    return (
      <VaultPicker
        path={path}
        vault={vault}
        source={source}
        mode={mode}
        onMode={onMode}
        initial={items}
        onBack={onBack}
        onClose={onClose}
        onPick={(chosen) => { setItems(chosen); setChoosing(false) }}
      />
    )
  }

  return (
    <Modal
      title={source.name}
      subtitle={exporting ? `${vaultHere} → ${machineHere}` : `${machineHere} → ${vaultHere}`}
      onClose={onClose}
      width={640}
    >
      {error && <Banner tone="error">{error}</Banner>}
      {summary && <TransferSummary summary={summary} kind={summary.kind} onDismiss={() => setSummary(null)} />}

      <DirectionSwitch mode={mode} onMode={onMode} disabled={busy} />

      {exporting && (
        <div style={{
          display: 'flex', alignItems: 'center', gap: '10px', marginBottom: '12px',
          padding: '8px 10px', background: COLORS.surface,
          border: `1px solid ${COLORS.border}`, borderRadius: '6px',
        }}>
          <span style={{
            flex: 1, minWidth: 0, fontFamily: FONT.sans, fontSize: '12px', color: COLORS.textDim,
            overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
          }}>
            Sending {describeItems(items)}{totalBytes > 0 ? ` · ${formatBytes(totalBytes)}` : ''}
          </span>
          <Button size="sm" variant="ghost" onClick={() => setChoosing(true)} disabled={busy}>Change</Button>
        </div>
      )}

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
            }}>{exporting ? 'Nothing here yet.' : 'Nothing here.'}</p>
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
                  pickable={!exporting}
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
          only part of it. Narrow it down on the server, or work a subfolder at
          a time.
        </Banner>
      )}

      {/* Everything this machine has going, and what it lately finished. A
          foreground transfer is drawn from the same list as a detached one —
          the only difference between them is what happens to this page. */}
      {runs.map((run) => (run.done
        ? <FinishedTransfer key={run.id} run={run} onDismiss={() => stopRun(run)} />
        : <TransferProgress key={run.id} run={run} onStop={() => stopRun(run)} />))}
      {busy && runs.length === 0 && <TransferProgress run={{ kind: mode }} />}

      {exporting && (
        <Banner tone="warn">
          <strong>Sent in the clear.</strong> Files land on the machine decrypted
          and whole, under their own names, readable by anyone who can read that
          folder. That is the point of sending them — and the opposite of what a
          connected cloud ever holds.
        </Banner>
      )}

      {exporting && (
        <Choice
          checked={overwrite}
          disabled={busy}
          onChange={setOverwrite}
          label="Replace files already there"
          help="Otherwise a file already at the name is left alone and reported: as already sent when it is the same file, and as in the way when it is not."
        />
      )}

      {/* Asked for, never assumed. A transfer that keeps running after the
          page is closed is a thing to have decided, not to discover. */}
      <Choice
        checked={detach}
        disabled={busy}
        onChange={setDetach}
        label="Keep going if I close this page"
        help={`The machine carries on ${exporting ? 'sending' : 'fetching'} on its own, and this dialog shows how far it has got whenever you come back to it. Restarting SAND still stops it — run the same ${exporting ? 'export' : 'import'} again and it picks up from the files that already landed.`}
      />

      <div style={{ display: 'flex', gap: '8px', alignItems: 'center' }}>
        {exporting
          ? (
            <Button variant="primary" onClick={runExport} disabled={busy || items.length === 0}>
              {busy ? 'Sending…' : `Send ${describeItems(items, true)} here`}
            </Button>
          )
          : (
            <>
              <Button variant="primary" onClick={runImport} disabled={busy || picked.size === 0}>
                {busy ? 'Bringing them in…' : `Import ${picked.size || ''}`.trim()}
              </Button>
              {picked.size > 0 && (
                <Button size="sm" variant="ghost" onClick={() => setPicked(new Set())} disabled={busy}>
                  Clear
                </Button>
              )}
            </>
          )}
      </div>

      <p style={{
        marginTop: '16px', paddingTop: '12px', borderTop: `1px solid ${COLORS.border}`,
        fontFamily: FONT.sans, fontSize: '11px', lineHeight: 1.55, color: COLORS.textMuted,
      }}>
        {exporting
          ? 'A folder sends everything under it, keeping its shape, into the folder you are standing in here. Nothing is removed from the vault. If this is interrupted, every file that landed whole is on the machine, and running the same export again skips those and carries on from the next one. A file cut off partway is the exception: nothing of it is left under its name, and the next run sends it again from the first byte.'
          : 'A folder brings everything under it, keeping its shape. Nothing is removed from the machine. If this is interrupted, every file that arrived whole is already scattered, and running the same import again skips those and carries on from the next one. A file cut off partway is the exception: nothing of it is kept, and the next run fetches it again from the first byte.'}
      </p>
    </Modal>
  )
}

/* One tick-box with a sentence under it. */
function Choice({ checked, disabled, onChange, label, help }) {
  return (
    <label style={{
      display: 'flex', gap: '8px', alignItems: 'flex-start', marginBottom: '12px',
      cursor: disabled ? 'default' : 'pointer',
    }}>
      <input
        type="checkbox"
        checked={checked}
        disabled={disabled}
        onChange={(e) => onChange(e.target.checked)}
        style={{ marginTop: '2px' }}
      />
      <span style={{
        fontFamily: FONT.sans, fontSize: '12px', lineHeight: 1.5, color: COLORS.textDim,
      }}>
        {label}
        <span style={{ display: 'block', fontSize: '11px', color: COLORS.textMuted }}>{help}</span>
      </span>
    </label>
  )
}

/* "3 items", "the folder photos", "a.jpg" — what is being sent, in the fewest
   words that are still exact. */
function describeItems(items, short = false) {
  if (items.length === 0) return 'nothing'
  if (items.length === 1) {
    const [item] = items
    if (short) return item.kind === 'folder' ? 'the folder' : 'the file'
    return item.kind === 'folder' ? `the folder ${item.name}` : item.name
  }
  const folders = items.filter((i) => i.kind === 'folder').length
  const files = items.length - folders
  if (short) return `${items.length} items`
  const parts = []
  if (folders) parts.push(`${folders} folder${folders === 1 ? '' : 's'}`)
  if (files) parts.push(`${files} file${files === 1 ? '' : 's'}`)
  return parts.join(' and ')
}

/* Choosing what to send, starting from the folder the browser is standing in.

   The vault's own listing, walked a level at a time with a tick per row: a
   folder ticked sends everything under it, a folder opened shows what is
   inside so one file deep in it can be picked on its own. Ticks survive
   walking around, so a selection can be gathered from several folders and
   sent in one go. The shortcut at the top is the case this is usually wanted
   in: the whole folder, as it is, onto the machine. */
function VaultPicker({ path, vault, source, mode, onMode, initial, onBack, onClose, onPick }) {
  const [cwd, setCwd] = useState(path)
  const [listing, setListing] = useState(null)
  const [error, setError] = useState(null)
  const [chosen, setChosen] = useState(() => new Map(initial.map((item) => [item.path, item])))

  useEffect(() => {
    let live = true
    setListing(null)
    api.list(cwd, vault)
      .then((resp) => { if (live) setListing(resp) })
      .catch((err) => { if (live) { setError(err.message); setListing({ folders: [], files: [] }) } })
    return () => { live = false }
  }, [cwd, vault])

  const toggle = (item) => {
    const next = new Map(chosen)
    if (next.has(item.path)) next.delete(item.path)
    else next.set(item.path, item)
    setChosen(next)
  }

  const join = (dir, name) => (dir === '/' ? `/${name}` : `${dir}/${name}`)
  const parent = cwd === '/' ? '/' : cwd.slice(0, cwd.lastIndexOf('/')) || '/'
  const here = cwd === '/' ? 'the vault' : cwd
  const whole = {
    kind: 'folder',
    path: cwd,
    name: cwd === '/' ? (vault || 'vault') : cwd.slice(cwd.lastIndexOf('/') + 1),
  }
  const folders = (listing?.folders || []).map((name) => ({
    kind: 'folder', name, path: join(cwd, name),
  }))
  const files = (listing?.files || []).map((file) => ({
    kind: 'file', name: file.name, path: join(cwd, file.name),
    size: file.size, legacy: !file.chunk_count,
  }))
  const rows = [...folders, ...files]

  /* A row is covered when it, or a folder above it, is ticked: a file inside
     a ticked folder is going already, and ticking it again would only be a
     second line saying so. */
  const covered = (item) => [...chosen.keys()].some((p) => p !== item.path
    && (p === '/' || item.path.startsWith(`${p}/`)))

  return (
    <Modal
      title={source.name}
      subtitle={`What to send from ${here}`}
      onClose={onClose}
      width={640}
    >
      {error && <Banner tone="error">{error}</Banner>}

      <DirectionSwitch mode={mode} onMode={onMode} />

      <div style={{ display: 'flex', gap: '8px', marginBottom: '12px', alignItems: 'center', flexWrap: 'wrap' }}>
        <Button size="sm" variant="ghost" onClick={onBack}>← Machines</Button>
        {cwd !== '/' && (
          <Button size="sm" onClick={() => setCwd(parent)} aria-label="Up">↑ Up</Button>
        )}
        <span style={{
          fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textMuted,
          overflowWrap: 'anywhere', flex: 1, minWidth: 0,
        }}>{cwd}</span>
        <Button size="sm" onClick={() => onPick([whole])} disabled={rows.length === 0}>
          Everything in {cwd === '/' ? 'the vault' : whole.name}
        </Button>
      </div>

      {listing === null
        ? <Spinner />
        : rows.length === 0
          ? (
            <p style={{
              fontFamily: FONT.sans, fontSize: '12.5px', color: COLORS.textDim, margin: '0 0 16px',
            }}>Nothing in this folder to send.</p>
          )
          : (
            <div style={{
              maxHeight: '46vh', overflowY: 'auto', marginBottom: '12px',
              border: `1px solid ${COLORS.border}`, borderRadius: '6px',
            }}>
              {rows.map((item) => {
                const inherited = covered(item)
                const off = item.legacy || inherited
                return (
                  <div key={item.path} style={{
                    display: 'flex', alignItems: 'center', gap: '10px',
                    padding: '8px 10px',
                    borderBottom: `1px solid ${COLORS.border}`,
                    opacity: item.legacy ? 0.55 : 1,
                  }}>
                    <input
                      type="checkbox"
                      checked={inherited || chosen.has(item.path)}
                      disabled={off}
                      onChange={() => toggle(item)}
                      aria-label={`Select ${item.name}`}
                      title={inherited ? 'Already going, inside a folder that is ticked' : undefined}
                    />
                    <span style={{ fontSize: '13px', width: '16px', textAlign: 'center' }}>
                      {item.kind === 'folder' ? '📁' : '📄'}
                    </span>
                    {/* A folder's name opens it, so a file deep inside can be
                        picked on its own; the tick beside it takes the whole
                        folder. A file's name is its tick. */}
                    <button
                      type="button"
                      onClick={() => {
                        if (item.kind === 'folder') setCwd(item.path)
                        else if (!off) toggle(item)
                      }}
                      disabled={item.kind === 'file' && off}
                      aria-label={item.kind === 'folder' ? `Open ${item.name}` : undefined}
                      style={{
                        flex: 1, textAlign: 'left', background: 'none', border: 'none', padding: 0,
                        cursor: item.kind === 'file' && off ? 'default' : 'pointer',
                        fontFamily: FONT.sans, fontSize: '12.5px', color: COLORS.text,
                        overflowWrap: 'anywhere',
                      }}
                    >
                      {item.name}
                      {item.kind === 'folder' && (
                        <span style={{ marginLeft: '6px', fontSize: '11px', color: COLORS.textMuted }}>▸</span>
                      )}
                      {item.legacy && (
                        <span style={{ display: 'block', fontSize: '11px', color: COLORS.warn }}>
                          Stored in the old format — convert it before it can be sent
                        </span>
                      )}
                    </button>
                    {item.kind === 'file' && (
                      <span style={{ fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textMuted }}>
                        {formatBytes(item.size)}
                      </span>
                    )}
                  </div>
                )
              })}
            </div>
          )}

      {/* What is ticked so far, wherever it was ticked: walking into a folder
          hides its parent's rows, and a count alone would not say what the
          count is of. */}
      {chosen.size > 0 && (
        <div style={{
          marginBottom: '12px', fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textMuted,
          overflowWrap: 'anywhere',
        }}>
          Picked: {[...chosen.values()].map((item) => item.path).join(', ')}
        </div>
      )}

      <div style={{ display: 'flex', gap: '8px', alignItems: 'center' }}>
        <Button variant="primary" onClick={() => onPick([...chosen.values()])} disabled={chosen.size === 0}>
          Next: choose where on the machine{chosen.size > 0 ? ` (${chosen.size})` : ''}
        </Button>
        {chosen.size > 0 && (
          <Button size="sm" variant="ghost" onClick={() => setChosen(new Map())}>Clear</Button>
        )}
      </div>

      <p style={{
        marginTop: '16px', paddingTop: '12px', borderTop: `1px solid ${COLORS.border}`,
        fontFamily: FONT.sans, fontSize: '11px', lineHeight: 1.55, color: COLORS.textMuted,
      }}>
        A folder ticked sends everything under it, keeping its shape; open one
        to pick single files inside it. Files leave the vault whole and
        decrypted, gathered from your clouds a piece at a time and written
        straight onto the machine — nothing passes through this browser, and
        nothing is held in this machine's memory beyond the piece in flight.
      </p>
    </Modal>
  )
}

/* The file being worked on, while it is being worked on.

   An import has two stages, named rather than merged, because they are two
   passes over the same bytes and they are slow for different reasons: coming
   down is the machine's upstream, going up is this one's. Adding them into a
   single percentage would make a file look half done when it had only
   arrived, and would hide which half of the trip is the slow one. An export
   has one: the parts come down from the clouds and go out to the machine in
   the same pass.

   A detached transfer says so, and can be stopped from here — there is no page
   to close that would stop it, which is the point of it. Stopping keeps
   whatever arrived whole, so it is a pause as much as a cancel: the same
   transfer run again skips those files and carries on. */
function TransferProgress({ run, onStop }) {
  const at = run?.at
  const exporting = run?.kind === 'export'
  const size = at?.size || 0
  const fraction = size > 0 ? Math.min(1, (at.done || 0) / size) : 0
  const scattering = at?.stage === 'scattering'

  /* How fast, and how much longer. Both come from the server's own reading of
     the stage it is on rather than from the difference between two polls — it
     sees every few megabytes go past and this dialog sees one snapshot a
     second, so its arithmetic is over the transfer rather than over the
     polling. It reports nothing while a stage is starting and nothing once a
     transfer has stalled, and both of those are drawn as nothing rather than
     as zero. */
  const rate = run?.rate || 0
  const left = rate > 0 && size > 0 ? (size - (at?.done || 0)) / rate : 0

  /* Between the request going out and the first file being picked up, the
     server is walking the selection — one round trip per folder, and on a
     folder of ten thousand files that is a real wait. Until a file is named
     there is nothing to count, so the bar says what is happening instead of
     drawing 0 B of 0 B. */
  const started = !!at?.name
  const heading = started ? at.name : 'Looking over what was picked…'
  const counted = started && at.files > 1 ? `${at.file} of ${at.files}` : ''

  const stageText = exporting
    ? 'gathering from the clouds and sending to the machine…'
    : scattering
      ? 'splitting, encrypting and scattering…'
      : 'coming down from the machine…'

  return (
    <div style={{
      padding: '11px 13px',
      marginBottom: '12px',
      background: COLORS.surface,
      border: `1px solid ${COLORS.border}`,
      borderRadius: '6px',
    }}>
      <div style={{
        display: 'flex', justifyContent: 'space-between', gap: '10px',
        fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textDim, marginBottom: '6px',
      }}>
        <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
          {exporting ? '↑ ' : '↓ '}{heading}
        </span>
        {counted && <span style={{ flexShrink: 0 }}>{counted}</span>}
      </div>

      {started && (
        <div style={{
          display: 'flex', justifyContent: 'space-between', gap: '10px',
          fontFamily: FONT.mono, fontSize: '10.5px', color: COLORS.textMuted, marginBottom: '8px',
        }}>
          <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
            {stageText}
          </span>
          <span style={{ flexShrink: 0 }}>
            {formatBytes(at.done || 0)} / {formatBytes(size)}
          </span>
        </div>
      )}

      <div style={{ height: '3px', background: COLORS.border, borderRadius: '2px', overflow: 'hidden' }}>
        <div style={{
          height: '100%',
          width: `${Math.max(4, fraction * 100)}%`,
          background: scattering || exporting ? COLORS.accent : COLORS.accentDim,
          transition: 'width 0.3s ease',
        }} />
      </div>

      {/* Under the bar: on the left what the summary would say if it stopped
          here, which is also what a second run would find already done; on the
          right how fast it is going and how much longer that leaves. */}
      {started && (
        <div style={{
          display: 'flex', justifyContent: 'space-between', gap: '10px', marginTop: '7px',
          fontFamily: FONT.mono, fontSize: '10.5px', color: COLORS.textMuted,
        }}>
          <span>
            {(at.completed > 0 || at.skipped > 0 || at.failed > 0) && (
              <>
                {at.completed} {exporting ? 'sent' : 'in'}
                {at.skipped ? `, ${at.skipped} already there` : ''}
                {at.failed ? `, ${at.failed} failed` : ''}
              </>
            )}
          </span>
          {rate > 0 && (
            <span style={{ flexShrink: 0 }}>
              {formatBytes(rate)}/s{left > 0 ? ` · ${formatDuration(left)} left` : ''}
            </span>
          )}
        </div>
      )}

      {run?.detached && (
        <div style={{
          display: 'flex', alignItems: 'center', justifyContent: 'space-between',
          gap: '10px', marginTop: '9px',
        }}>
          <span style={{
            fontFamily: FONT.sans, fontSize: '11px', color: COLORS.textMuted,
          }}>
            Running on the machine — you can close this page.
          </span>
          {onStop && <Button size="sm" variant="ghost" onClick={onStop}>Stop</Button>}
        </div>
      )}
    </div>
  )
}

/* How much longer, in the roundest terms that are still true.

   Rounded hard on purpose: the estimate is a current speed multiplied by what
   is left, and a home connection does not hold one speed for an hour. "About
   40 minutes" is honest about that in a way "38m 12s" is not, and the second
   would tick down in a way that invites watching it. */
function formatDuration(seconds) {
  if (seconds < 60) return 'under a minute'
  const minutes = Math.round(seconds / 60)
  if (minutes < 60) return `about ${minutes} min`
  const hours = seconds / 3600
  if (hours < 2) return 'about an hour'
  if (hours < 24) return `about ${Math.round(hours)} hours`
  return 'over a day'
}

/* A detached transfer that is over, waiting to be read by whoever comes back.

   It says the same thing the summary of a foreground transfer says, because
   it is the same summary — the only difference is that nobody was holding a
   request open to receive it, so it waited here instead. It goes when it is
   dismissed, or when it goes stale.

   Stopped is reported as an outcome rather than as a failure. It was asked
   for, and what it had already moved is where it was going. */
function FinishedTransfer({ run, onDismiss }) {
  const exporting = run.kind === 'export'
  if (run.error) {
    return (
      <Banner tone="error" onDismiss={onDismiss}>
        <div>That {exporting ? 'export' : 'import'} stopped: {run.error}</div>
        <div style={{ marginTop: '4px' }}>
          Whatever arrived whole is {exporting ? 'on the machine' : 'in the vault'}. Running
          the same {exporting ? 'export' : 'import'} again skips those and carries on.
        </div>
      </Banner>
    )
  }

  return (
    <TransferSummary
      summary={run.summary || {}}
      kind={run.kind}
      lead={run.cancelled ? 'Stopped.' : 'Finished in the background.'}
      onDismiss={onDismiss}
    />
  )
}

function RemoteEntry({ entry, pickable, checked, disabled, onOpen, onToggle }) {
  const unreachable = !!entry.unreachable

  return (
    <div style={{
      display: 'flex', alignItems: 'center', gap: '10px',
      padding: '8px 10px',
      borderBottom: `1px solid ${COLORS.border}`,
      opacity: unreachable ? 0.55 : 1,
    }}>
      {pickable && (
        <input
          type="checkbox"
          checked={checked}
          disabled={disabled || unreachable}
          onChange={onToggle}
          aria-label={`Select ${entry.name}`}
        />
      )}
      <span style={{ fontSize: '13px', width: '16px', textAlign: 'center' }}>
        {entry.dir ? '📁' : entry.symlink ? '↪' : '📄'}
      </span>
      <button
        type="button"
        onClick={entry.dir && !unreachable ? onOpen : (pickable ? onToggle : undefined)}
        disabled={disabled || unreachable || (!pickable && !entry.dir)}
        style={{
          flex: 1, textAlign: 'left', background: 'none', border: 'none', padding: 0,
          cursor: disabled || unreachable || (!pickable && !entry.dir) ? 'default' : 'pointer',
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

   Skipped is reported as loudly as moved, because on a second run it is the
   answer: it says the files are already there rather than that nothing
   happened. */
function TransferSummary({ summary, kind, lead = '', onDismiss }) {
  const exporting = kind === 'export'
  const failures = (summary.results || []).filter((r) => r.error)
  const moved = exporting ? summary.exported : summary.imported
  const skipped = (summary.results || []).filter((r) => r.skipped)
  // A file left alone because something else is under its name is worth
  // its own line: it is the one skip a second run will not resolve.
  const inTheWay = exporting ? skipped.filter((r) => r.reason && r.reason !== 'already there') : []

  return (
    <Banner tone={failures.length || inTheWay.length ? 'warn' : 'info'} onDismiss={onDismiss}>
      <div>
        {lead && `${lead} `}
        {moved || 0} {exporting ? 'sent' : 'brought in'}
        {summary.skipped ? `, ${summary.skipped} already there` : ''}
        {failures.length ? `, ${failures.length} failed` : ''}.
        {summary.truncated ? ` More was selected than one ${exporting ? 'export' : 'import'} can carry — run it again to continue.` : ''}
      </div>
      {inTheWay.slice(0, 5).map((r) => (
        <div key={r.path} style={{
          fontFamily: FONT.mono, fontSize: '11px', marginTop: '4px', overflowWrap: 'anywhere',
        }}>{r.dest}: {r.reason}</div>
      ))}
      {failures.slice(0, 5).map((r) => (
        <div key={r.path} style={{
          fontFamily: FONT.mono, fontSize: '11px', marginTop: '4px', overflowWrap: 'anywhere',
        }}>{r.path}: {r.error}</div>
      ))}
    </Banner>
  )
}
