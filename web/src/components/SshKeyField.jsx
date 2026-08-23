import React, { useState } from 'react'
import { COLORS, FONT } from '../theme'
import { api } from '../api'
import { Banner, Button, CopyField, TextArea } from './ui'

/* The private key field, with the paste turned round.

   Asking somebody to run ssh-keygen, find the right one of the two files it
   wrote, and paste that one into a browser is three chances to paste the wrong
   half — and the wrong half is the interesting one to get wrong. So the
   default here is the other direction: SAND makes the pair, keeps the private
   half, and hands back one line to add on the server. The half that travels is
   the one it does not matter about.

   The private half is never sent to the browser at all, not even to be
   displayed. What this component holds in the form is a handle standing in for
   it, which the server swaps back when the connection is stored — see
   internal/server/handlers_sshkeys.go. That is why there is no "download the
   private key" button: there is nothing here to download, which is the point.

   Pasting your own key is the same field and one click away. Plenty of people
   already have a key for the machine in question, and a key held in an agent
   or issued by a CA is not one SAND can invent a replacement for. */

/* Which mode the field opens in.

   Generating is the default on a form that is filling a key in for the first
   time. On an edit form it is not: there the field is already standing for a
   key the account has, an empty box means "leave it alone", and opening on a
   generate button would put "replace the credential" where "change the name"
   used to be. */
function initialMode(secretPlaceholder) {
  return secretPlaceholder ? 'paste' : 'generate'
}

/* What the key is tagged with, which is what somebody reads at the end of the
   authorized_keys line a year from now. The only job a key comment has ever
   had is answering "what is this and can I delete it", so it carries the name
   the connection is called here when there is one.

   Whitespace is collapsed rather than kept: authorized_keys is parsed by
   whitespace, and while the comment runs to the end of the line either way, a
   name with spaces in it reads there as several fields. */
export function keyComment(name) {
  const clean = (name || '').trim().replace(/\s+/g, '-')
  return clean ? `sand-vault-${clean}` : 'sand-vault'
}

export default function SshKeyField({
  label = 'Private key',
  help,
  value,
  onChange,
  disabled,
  placeholder = '-----BEGIN OPENSSH PRIVATE KEY-----',
  secretPlaceholder,
  keyName = '',
}) {
  const [mode, setMode] = useState(() => initialMode(secretPlaceholder))
  const [generated, setGenerated] = useState(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  const generate = async () => {
    setBusy(true)
    setError(null)
    try {
      const resp = await api.generateSshKey(keyComment(keyName))
      setGenerated(resp)
      // The handle, not a key. It is worth nothing to anybody who cannot
      // already reach this server, and it expires on its own.
      onChange(resp.handle)
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  /* Switching modes drops whatever the other one had in it. A form carrying a
     handle it is no longer showing would connect with a key nobody meant to
     use, and the failure would be silent — it would work. */
  const switchTo = (next) => {
    if (next === mode) return
    setMode(next)
    setError(null)
    if (next === 'paste') setGenerated(null)
    onChange('')
  }

  return (
    <div style={{ marginBottom: '14px' }}>
      <div style={{
        display: 'flex', alignItems: 'baseline', justifyContent: 'space-between',
        gap: '10px', marginBottom: '6px', flexWrap: 'wrap',
      }}>
        <span style={{
          fontFamily: FONT.mono, fontSize: '10px', fontWeight: 600,
          letterSpacing: '1.5px', textTransform: 'uppercase', color: COLORS.textMuted,
        }}>{label}</span>
        <Choice
          mode={mode}
          disabled={disabled || busy}
          onChoose={switchTo}
        />
      </div>

      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

      {mode === 'generate'
        ? (
          <Generated
            generated={generated}
            busy={busy}
            disabled={disabled}
            onGenerate={generate}
          />
        )
        : (
          <TextArea
            value={value || ''}
            disabled={disabled}
            placeholder={secretPlaceholder || placeholder}
            onChange={(e) => onChange(e.target.value)}
            help={help || 'The whole file, BEGIN and END lines included. Its public half has to be in the account’s authorized_keys on the server already.'}
          />
        )}
    </div>
  )
}

/* Two words rather than a control with a chrome of its own: this sits on the
   field's own label line, where a select or a pair of radio buttons would
   outweigh everything under it. */
function Choice({ mode, disabled, onChoose }) {
  return (
    <span style={{ display: 'flex', gap: '10px' }}>
      <Word active={mode === 'generate'} disabled={disabled} onClick={() => onChoose('generate')}>
        SAND MAKES ONE
      </Word>
      <span style={{ color: COLORS.border, fontFamily: FONT.mono, fontSize: '10px' }}>|</span>
      <Word active={mode === 'paste'} disabled={disabled} onClick={() => onChoose('paste')}>
        I HAVE A KEY
      </Word>
    </span>
  )
}

function Word({ active, disabled, onClick, children }) {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      aria-pressed={active}
      style={{
        background: 'none', border: 'none', padding: 0,
        cursor: disabled ? 'default' : 'pointer',
        fontFamily: FONT.mono, fontSize: '10px', fontWeight: 600, letterSpacing: '1.5px',
        color: active ? COLORS.accent : COLORS.textMuted,
        borderBottom: `1px solid ${active ? COLORS.accent : 'transparent'}`,
        opacity: disabled ? 0.5 : 1,
      }}
    >{children}</button>
  )
}

/* What there is to do once the pair exists: put one line on the server.

   The line itself and the command that appends it are both offered, because
   which one is useful depends on how you reach the box. A NAS web console has
   a box to paste a key into; a VPS has a shell, where the command is the whole
   job and typing it out from a displayed key is not. */
function Generated({ generated, busy, disabled, onGenerate }) {
  if (!generated) {
    return (
      <>
        <Button variant="primary" onClick={onGenerate} disabled={disabled || busy}>
          {busy ? 'Making one…' : 'Generate a key pair'}
        </Button>
        <p style={{
          marginTop: '8px', marginBottom: 0,
          fontFamily: FONT.sans, fontSize: '11px', lineHeight: 1.5, color: COLORS.textMuted,
        }}>
          An Ed25519 key, made on this machine. The private half stays here and
          goes into the vault when you connect — it is never shown in this
          browser and never sent anywhere. You get one line to add on the
          server.
        </p>
      </>
    )
  }

  const install = `mkdir -p ~/.ssh && chmod 700 ~/.ssh && echo '${generated.public_key}' >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys`

  return (
    <>
      <Banner tone="warn">
        Put this on the server before connecting — until it is there, the
        connection has nothing to sign in with.
      </Banner>

      <CopyField
        label="Public half — goes in authorized_keys"
        value={generated.public_key}
        help="One line. Safe to paste anywhere: it is the half that is meant to be handed out."
      />

      <CopyField
        label="Or run this on the server"
        value={install}
        help="As the user SAND will sign in as — not as root, unless that is the user."
      />

      <p style={{
        margin: '0 0 10px',
        fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textMuted,
        overflowWrap: 'anywhere',
      }}>{generated.fingerprint}</p>

      <button
        type="button"
        onClick={onGenerate}
        disabled={disabled || busy}
        style={{
          background: 'none', border: 'none', padding: 0,
          cursor: disabled || busy ? 'default' : 'pointer',
          fontFamily: FONT.mono, fontSize: '10px', letterSpacing: '1px',
          color: COLORS.accent,
        }}
      >{busy ? 'MAKING ANOTHER…' : 'MAKE A DIFFERENT ONE…'}</button>
    </>
  )
}
