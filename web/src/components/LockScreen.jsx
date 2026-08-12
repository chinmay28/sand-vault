import React, { useState } from 'react'
import { COLORS, FONT } from '../theme'
import { api } from '../api'
import { Banner, Button, PasswordInput, Spinner } from './ui'
import { Brand, DevMark } from './Brand'

/* The gate in front of everything: without the vault password the server
   cannot decrypt the index, so there is nothing to show and nothing to fetch. */
export default function LockScreen({ status, onUnlocked }) {
  const creating = !status.initialized

  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [policy, setPolicy] = useState('strict')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  const submit = async (e) => {
    e.preventDefault()
    setError(null)

    if (creating && password !== confirm) {
      setError('The two passwords do not match.')
      return
    }
    if (!password) return

    setBusy(true)
    try {
      const result = creating
        ? await api.initVault(password, policy)
        : await api.unlock(password)
      setPassword('')
      setConfirm('')
      onUnlocked(result)
    } catch (err) {
      setError(err.code === 'WRONG_PASSWORD' ? 'Wrong password.' : err.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div style={{
      minHeight: '100vh',
      display: 'flex',
      alignItems: 'center',
      justifyContent: 'center',
      padding: '24px',
    }}>
      <form onSubmit={submit} style={{ width: '100%', maxWidth: '420px' }}>
        <div style={{
          display: 'flex',
          flexDirection: 'column',
          alignItems: 'center',
          gap: '10px',
          marginBottom: '30px',
        }}>
          <Brand size="lg" />
          <div style={{
            fontFamily: FONT.mono,
            fontSize: '10px',
            letterSpacing: '2.5px',
            textTransform: 'uppercase',
            color: COLORS.textMuted,
          }}>Secure Archival Network Distribution</div>
        </div>

        <div style={{
          background: COLORS.surface,
          border: `1px solid ${COLORS.border}`,
          borderRadius: '10px',
          padding: '24px',
        }}>
          <h1 style={{
            margin: '0 0 6px',
            fontFamily: FONT.mono,
            fontSize: '14px',
            fontWeight: 700,
            letterSpacing: '1px',
            color: COLORS.text,
          }}>{creating ? 'Create your vault' : 'Unlock your vault'}</h1>

          <p style={{
            margin: '0 0 20px',
            fontFamily: FONT.sans,
            fontSize: '12px',
            lineHeight: 1.6,
            color: COLORS.textMuted,
          }}>
            {creating
              ? 'This password protects the index of your files and the credentials for every cloud account you connect. It is never stored anywhere — if you lose it, the files cannot be rebuilt.'
              : 'Your file index and cloud credentials are encrypted at rest. Nothing can be listed or fetched until they are unlocked.'}
          </p>

          {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

          <PasswordInput
            label="Vault password"
            value={password}
            autoFocus
            autoComplete={creating ? 'new-password' : 'current-password'}
            placeholder={creating ? 'Choose a strong passphrase' : 'Enter your password'}
            onChange={(e) => setPassword(e.target.value)}
          />

          {creating && (
            <>
              <PasswordInput
                label="Confirm password"
                value={confirm}
                autoComplete="new-password"
                placeholder="Type it again"
                onChange={(e) => setConfirm(e.target.value)}
              />

              <div style={{ marginBottom: '18px' }}>
                <span style={{
                  display: 'block',
                  fontFamily: FONT.mono,
                  fontSize: '10px',
                  fontWeight: 600,
                  letterSpacing: '1.5px',
                  textTransform: 'uppercase',
                  color: COLORS.textMuted,
                  marginBottom: '8px',
                }}>How parts are spread</span>

                {[
                  {
                    value: 'strict',
                    title: 'Strict — one part per account',
                    body: 'No single account ever holds two parts, so breaking into one of them reveals nothing. Needs at least two connected accounts; three gives you a spare part.',
                  },
                  {
                    value: 'redundant',
                    title: 'Redundant — always store all three parts',
                    body: 'Survives an account going offline even with only one or two connected, but an account that ends up with two parts could rebuild the file on its own.',
                  },
                ].map((option) => (
                  <label
                    key={option.value}
                    style={{
                      display: 'flex',
                      gap: '10px',
                      padding: '10px 12px',
                      marginBottom: '8px',
                      background: policy === option.value ? COLORS.surfaceRaised : 'transparent',
                      border: `1px solid ${policy === option.value ? COLORS.accent : COLORS.border}`,
                      borderRadius: '6px',
                      cursor: 'pointer',
                    }}
                  >
                    <input
                      type="radio"
                      name="policy"
                      value={option.value}
                      checked={policy === option.value}
                      onChange={() => setPolicy(option.value)}
                      style={{ marginTop: '3px', accentColor: COLORS.accent }}
                    />
                    <span>
                      <span style={{
                        display: 'block',
                        fontFamily: FONT.mono,
                        fontSize: '12px',
                        color: COLORS.text,
                        marginBottom: '3px',
                      }}>{option.title}</span>
                      <span style={{
                        fontFamily: FONT.sans,
                        fontSize: '11px',
                        color: COLORS.textMuted,
                        lineHeight: 1.5,
                      }}>{option.body}</span>
                    </span>
                  </label>
                ))}
              </div>
            </>
          )}

          <Button
            type="submit"
            variant="primary"
            disabled={busy || !password}
            style={{ width: '100%', justifyContent: 'center', padding: '12px' }}
          >
            {busy ? <Spinner size={13} color={COLORS.bg} /> : null}
            {busy ? 'Working…' : creating ? '▶ Create vault' : '▶ Unlock'}
          </Button>
        </div>

        {/* The crypto line wants the full width; the developer mark sits under
            it on its own row rather than squeezing the text into a wrap. */}
        <div style={{
          marginTop: '20px',
          display: 'flex',
          flexDirection: 'column',
          alignItems: 'center',
          gap: '12px',
        }}>
          <span style={{
            fontFamily: FONT.mono,
            fontSize: '10px',
            color: COLORS.textMuted,
            textAlign: 'center',
          }}>AES-256-GCM · Argon2id · zstd · any 2 of 3 parts rebuild a file</span>
          {/* No divider to hang off down here, so drop the hairline. */}
          <DevMark bare />
        </div>
      </form>
    </div>
  )
}
