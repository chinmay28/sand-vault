import React, { useEffect, useState } from 'react'
import { COLORS, FONT } from '../theme'
import { api } from '../api'
import { Banner, Button, Modal, Spinner } from './ui'

/* A folder opened to a TV for a while, with a four-character code.

   The TV — the SAND TV app on a Fire TV, or any TV browser pointed at /tv/ —
   asks for the code and nothing else. It is good for this folder's videos and
   the folders under it, until it runs out, and for nothing else in the vault:
   no other folder, no settings, no deleting. The code is read off this screen
   and keyed in with the remote, which is why it is four characters from an
   alphabet with no 0/O or 1/I to mix up.

   The codes live in the server's memory only. Restarting SAND ends them all,
   and revoking one here unpairs every TV that used it and stops whatever it
   is playing. Locking the vault does not end them — nothing can be read while
   it is locked, and the TV waits for it to be unlocked again. */

const LENGTHS = [
  { minutes: 60, label: '1 hour' },
  { minutes: 240, label: '4 hours' },
  { minutes: 60 * 24, label: '1 day' },
  { minutes: 60 * 24 * 7, label: '1 week' },
  { minutes: 60 * 24 * 30, label: '30 days' },
]

export function WatchOnTV({ path, name, vault = '', onClose }) {
  const [minutes, setMinutes] = useState(240)
  const [code, setCode] = useState(null)
  const [codes, setCodes] = useState(null)
  const [addresses, setAddresses] = useState([])
  const [discovery, setDiscovery] = useState(0)
  const [lan, setLan] = useState(true)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  const refresh = () => api.tvCodes()
    .then((resp) => {
      setCodes(resp.codes || [])
      setAddresses(resp.addresses || [])
      setDiscovery(resp.discovery_port || 0)
      setLan(resp.lan !== false)
    })
    .catch(setError)

  useEffect(() => { refresh() }, [])

  const mint = async () => {
    setBusy(true)
    setError(null)
    try {
      const grant = await api.tvCodeCreate(path, vault, minutes)
      setCode(grant)
      refresh()
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }

  const revoke = async (c) => {
    try {
      await api.tvCodeRevoke(c)
      if (code && code.code === c) setCode(null)
      refresh()
    } catch (err) {
      setError(err)
    }
  }

  const others = (codes || []).filter((c) => !code || c.code !== code.code)

  return (
    <Modal title={`Watch ${name} on TV`} subtitle={path} onClose={onClose} width={540}>
      {error && <Banner tone="error">{error.message}</Banner>}
      {!lan && (
        <Banner tone="warn">
          SAND is only listening on this machine, so no TV can reach it. Start it
          with <span style={{ fontFamily: FONT.mono }}>--bind 0.0.0.0</span> (the
          service's default) for a TV on your network to connect.
        </Banner>
      )}

      {!code && (
        <>
          <p style={paragraph}>
            A code for the TV to open this folder with. It can watch the videos in
            here and in the folders under it, with their posters and film details,
            and nothing else in the vault.
          </p>

          <div style={{ marginBottom: '14px' }}>
            <div style={label}>Good for</div>
            <div style={{ display: 'flex', gap: '6px', flexWrap: 'wrap' }}>
              {LENGTHS.map((l) => (
                <Button
                  key={l.minutes}
                  size="sm"
                  variant={minutes === l.minutes ? 'primary' : 'default'}
                  onClick={() => setMinutes(l.minutes)}
                >
                  {l.label}
                </Button>
              ))}
            </div>
          </div>

          <div style={{ display: 'flex', gap: '8px' }}>
            <Button variant="primary" onClick={mint} disabled={busy}>
              {busy ? 'Making a code…' : 'Make a code'}
            </Button>
            <Button variant="ghost" onClick={onClose}>Cancel</Button>
          </div>
        </>
      )}

      {code && (
        <>
          <div style={{ textAlign: 'center', margin: '6px 0 18px' }}>
            <div style={label}>On the TV, open SAND TV and key in</div>
            <div style={{
              fontFamily: FONT.mono, fontSize: '44px', fontWeight: 700, letterSpacing: '0.35em',
              color: COLORS.accentBright, margin: '6px 0 4px', paddingLeft: '0.35em',
            }}>
              {code.code}
            </div>
            <div style={{ fontFamily: FONT.mono, fontSize: '11.5px', color: COLORS.textDim }}>
              Good until {formatUntil(code.expires_at)}
            </div>
          </div>

          <p style={paragraph}>
            {discovery
              ? 'The SAND TV app finds this server by itself on the same network. '
              : 'Discovery is off on this server, so point the app at it by hand. '}
            {addresses.length > 0 && (
              <>
                {discovery ? 'If it does not, choose Enter address and type ' : 'Type '}
                <strong style={{ color: COLORS.text, fontFamily: FONT.mono }}>{addresses[0]}</strong>
                {addresses.length > 1 && <> (or {addresses.slice(1).map((a, i) => (
                  <span key={a}>{i > 0 && ', '}<span style={{ fontFamily: FONT.mono }}>{a}</span></span>
                ))})</>}.
              </>
            )}
            {' '}A TV browser can open <span style={{ fontFamily: FONT.mono }}>/tv/</span> on
            the same address instead.
          </p>

          <div style={{ display: 'flex', gap: '8px' }}>
            <Button variant="primary" onClick={onClose}>Done</Button>
            <Button variant="ghost" onClick={() => setCode(null)}>Make another</Button>
            <Button variant="danger" onClick={() => revoke(code.code)}>Revoke this code</Button>
          </div>
        </>
      )}

      <div style={{ marginTop: '18px', paddingTop: '14px', borderTop: `1px solid ${COLORS.border}` }}>
        <div style={label}>Codes that are live</div>
        {codes === null && <Spinner size={14} />}
        {codes !== null && others.length === 0 && (
          <div style={{ ...paragraph, margin: 0 }}>
            {code ? 'No others.' : 'None. A code ends when it runs out, when you revoke it, or when SAND restarts.'}
          </div>
        )}
        {others.map((c) => (
          <div
            key={c.code}
            style={{
              display: 'flex', alignItems: 'center', gap: '12px', padding: '8px 0',
              borderBottom: `1px solid ${COLORS.border}`,
            }}
          >
            <span style={{ fontFamily: FONT.mono, fontWeight: 700, fontSize: '15px', color: COLORS.text, minWidth: '56px' }}>
              {c.code}
            </span>
            <span style={{ flex: 1, minWidth: 0, fontFamily: FONT.sans, fontSize: '12px', color: COLORS.textDim }}>
              <div style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', color: COLORS.text }}>
                {c.path}{c.vault ? ' (sub vault)' : ''}
              </div>
              <div>
                until {formatUntil(c.expires_at)} · {c.devices === 1 ? '1 TV' : `${c.devices} TVs`} paired
              </div>
            </span>
            <Button size="sm" variant="danger" onClick={() => revoke(c.code)}>Revoke</Button>
          </div>
        ))}
      </div>
    </Modal>
  )
}

const paragraph = {
  margin: '0 0 14px', fontFamily: FONT.sans, fontSize: '12.5px', lineHeight: 1.6, color: COLORS.textDim,
}

const label = {
  fontFamily: FONT.mono, fontSize: '10.5px', letterSpacing: '0.08em', textTransform: 'uppercase',
  color: COLORS.textMuted, marginBottom: '6px',
}

function formatUntil(iso) {
  const d = new Date(iso)
  const sameDay = d.toDateString() === new Date().toDateString()
  return sameDay
    ? d.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' })
    : d.toLocaleString(undefined, { weekday: 'short', day: 'numeric', month: 'short', hour: 'numeric', minute: '2-digit' })
}
