import React, { useEffect, useMemo, useState } from 'react'
import { COLORS, FONT, KIND_ICONS } from '../theme'
import { api } from '../api'
import {
  callbackURL, forgetFlow, openAuthWindow, pendingOAuthFlow, rememberFlow, useSignInResult,
} from '../oauth'
import { Banner, Button, CopyField, Input, Modal, PasswordInput, Spinner } from './ui'
import SpecFields from './SpecFields'
import CloudCatalog from './CloudCatalog'

/* Connecting an account, without leaving the app.

   Backends that speak OAuth are connected by signing in: SAND opens the
   provider's own consent page, the provider redirects back to this server, and
   the tokens are exchanged and stored server-side. The browser never handles a
   credential — it only ever learns how far along the flow is. The mechanics of
   that live in ../oauth.js, shared with the edit dialog, where the same sign-in
   replaces the credentials of an account already connected.

   Everything else is still a form, generated from the backend's field specs,
   with presets for the services people actually name. */

export default function ConnectCloud({ onClose, onConnected }) {
  const [specs, setSpecs] = useState([])
  const [kind, setKind] = useState(null)

  // The catalogue of everything connectable, opened from the picker.
  const [catalog, setCatalog] = useState(false)

  // 'signin' walks the OAuth flow; 'form' is the generated credentials form,
  // which is also where an OAuth backend lands if you would rather paste
  // tokens you already have.
  const [mode, setMode] = useState('form')
  const [step, setStep] = useState('client')

  const [client, setClient] = useState({ clientId: '', clientSecret: '' })
  const [flow, setFlow] = useState(null)
  const [account, setAccount] = useState('')
  const [pasted, setPasted] = useState('')
  const [showPaste, setShowPaste] = useState(false)

  const [name, setName] = useState('')
  const [values, setValues] = useState({})
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  const spec = useMemo(() => specs.find((s) => s.kind === kind), [specs, kind])

  useEffect(() => {
    api.providerSpecs()
      .then((resp) => setSpecs(resp.specs || []))
      .catch((err) => setError(err.message))
  }, [])

  /* A sign-in that was still running when the app was last unloaded. One
     belonging to an account that is already connected is not this dialog's —
     the sidebar reopens that one in the account's edit dialog. */
  useEffect(() => {
    const pending = pendingOAuthFlow()
    if (!pending || pending.provider_id || !specs.length || flow) return
    setKind(pending.kind)
    setFlow(pending)
    setMode('signin')
    setStep('waiting')
  }, [specs, flow])

  const defaultsFor = (target) => {
    const out = {}
    for (const field of target.fields) {
      if (field.default) out[field.key] = field.default
    }
    return out
  }

  const choose = (target) => {
    setKind(target.kind)
    setError(null)
    setValues(defaultsFor(target))
    setName(target.oauth ? '' : target.label)
    setClient({ clientId: '', clientSecret: '' })
    setPasted('')
    setShowPaste(false)

    if (target.oauth) {
      setMode('signin')
      setStep('client')
      return
    }
    setMode('form')
  }

  /* Closing the dialog abandons whatever was in flight — otherwise the next
     reload would reopen it on a sign-in the user walked away from. */
  const close = () => {
    forgetFlow()
    onClose()
  }

  const back = () => {
    forgetFlow()
    setKind(null)
    setFlow(null)
    setAccount('')
    setError(null)
  }

  /* --- the sign-in itself ------------------------------------------------ */

  const beginSignIn = async () => {
    setBusy(true)
    setError(null)
    try {
      const resp = await api.oauthStart(spec.kind, {
        clientId: client.clientId,
        clientSecret: client.clientSecret,
        redirectUri: callbackURL(),
      })
      const started = { id: resp.flow_id, kind: spec.kind, redirect_uri: resp.redirect_uri }
      rememberFlow(started)
      setFlow(started)
      setStep('waiting')
      openAuthWindow(resp.auth_url)
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  /* While the provider's window is open, wait for the server to say how it
     went. */
  useSignInResult({
    flowId: flow?.id,
    active: mode === 'signin' && step === 'waiting',
    onReady: (resp) => {
      setAccount(resp.account || '')
      setName(resp.suggested_name || '')
      setStep('ready')
    },
    onFailed: (message) => {
      setError(message)
      setStep('client')
    },
  })

  const submitPasted = async () => {
    setBusy(true)
    setError(null)
    try {
      const resp = await api.oauthExchange(flow.id, pasted)
      forgetFlow()
      setAccount(resp.account || '')
      setName(resp.suggested_name || '')
      setStep('ready')
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  const finishSignIn = async (e) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await api.oauthComplete(flow.id, name, settingsOnly(spec, values))
      forgetFlow()
      onConnected()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  const submitForm = async (e) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await api.addProvider(spec.kind, name, values)
      onConnected()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  /* --- screens ----------------------------------------------------------- */

  if (!spec) {
    return (
      <>
        <Modal
          title="Connect a cloud account"
          subtitle="Pick where SAND should put one of the parts. Each account holds an encrypted fragment that is useless on its own."
          onClose={close}
        >
          {error && <Banner tone="error">{error}</Banner>}
          {specs.length === 0 && !error && <Spinner />}
          <ProviderPicker specs={specs} onChoose={choose} />

          {/* Two of those entries are protocols, not services, and cover
              dozens of names between them. The full list lives in a window of
              its own rather than in this one, which is already as long as a
              dialog can usefully be. */}
          {specs.length > 0 && (
            <button
              type="button"
              onClick={() => setCatalog(true)}
              style={{
                display: 'block',
                width: '100%',
                marginTop: '2px',
                padding: '10px',
                background: 'none',
                border: `1px dashed ${COLORS.border}`,
                borderRadius: '7px',
                color: COLORS.textMuted,
                fontFamily: FONT.mono,
                fontSize: '11px',
                cursor: 'pointer',
              }}
            >Not here? See the full list of services →</button>
          )}
        </Modal>

        {catalog && <CloudCatalog specs={specs} onClose={() => setCatalog(false)} />}
      </>
    )
  }

  if (mode === 'signin') {
    return (
      <Modal
        title={`Connect ${spec.label}`}
        subtitle={step === 'ready' ? 'Authorized. Name it and it joins the rotation.' : spec.description}
        onClose={close}
      >
        {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

        {step === 'client' && (
          <SignInStart
            spec={spec}
            client={client}
            setClient={setClient}
            busy={busy}
            onStart={beginSignIn}
            onManual={() => { setMode('form'); setName(spec.label) }}
            onBack={back}
          />
        )}

        {step === 'waiting' && (
          <SignInWaiting
            spec={spec}
            flow={flow}
            pasted={pasted}
            setPasted={setPasted}
            showPaste={showPaste}
            setShowPaste={setShowPaste}
            busy={busy}
            onPaste={submitPasted}
            onCancel={() => { forgetFlow(); setFlow(null); setStep('client') }}
          />
        )}

        {step === 'ready' && (
          <form onSubmit={finishSignIn}>
            <Banner tone="success">
              {account
                ? <>Signed in as <strong>{account}</strong>.</>
                : <>Authorized. SAND holds the tokens; they never reach this page.</>}
            </Banner>

            <Input
              label="Display name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="How this account appears in the sidebar"
            />

            <SpecFields
              fields={settingFields(spec)}
              values={values}
              onChange={(key, value) => setValues({ ...values, [key]: value })}
            />

            <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end' }}>
              <Button type="button" variant="ghost" onClick={back}>← Back</Button>
              <Button type="submit" variant="primary" disabled={busy}>
                {busy ? <Spinner size={12} color={COLORS.bg} /> : null}
                {busy ? 'Testing connection…' : 'Connect'}
              </Button>
            </div>
          </form>
        )}
      </Modal>
    )
  }

  return (
    <Modal title={`Connect ${spec.label}`} subtitle={spec.description} onClose={close}>
      <form onSubmit={submitForm}>
        {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

        {spec.oauth && (
          <Banner tone="info">
            Signing in is easier — SAND fetches the tokens for you.{' '}
            <a
              href="#"
              onClick={(e) => { e.preventDefault(); setMode('signin'); setStep('client'); setError(null) }}
              style={{ color: COLORS.accent }}
            >Sign in instead</a>
          </Banner>
        )}

        <Presets
          spec={spec}
          onApply={(preset) => setValues({ ...defaultsFor(spec), ...values, ...preset.values })}
        />

        <Input
          label="Display name"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="How this account appears in the sidebar"
        />

        <SpecFields
          fields={spec.fields}
          values={values}
          onChange={(key, value) => setValues({ ...values, [key]: value })}
        />

        {spec.docs_url && (
          <p style={{
            fontFamily: FONT.sans, fontSize: '11px',
            color: COLORS.textMuted, marginBottom: '14px',
          }}>
            Need credentials?{' '}
            <a href={spec.docs_url} target="_blank" rel="noreferrer"
              style={{ color: COLORS.accent }}>Provider documentation ↗</a>
          </p>
        )}

        <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end' }}>
          <Button type="button" variant="ghost" onClick={back}>← Back</Button>
          <Button type="submit" variant="primary" disabled={busy}>
            {busy ? <Spinner size={12} color={COLORS.bg} /> : null}
            {busy ? 'Testing connection…' : 'Connect'}
          </Button>
        </div>
      </form>
    </Modal>
  )
}

/* Fields the sign-in does not fill in for you — a folder, usually. */
function settingFields(spec) {
  return spec.fields.filter((field) => !field.advanced)
}

function settingsOnly(spec, values) {
  const out = {}
  for (const field of settingFields(spec)) {
    if (values[field.key] !== undefined) out[field.key] = values[field.key]
  }
  return out
}

/* A backend that asks for nothing but folders is a different kind of thing to
   connect: no account, no keys, just somewhere on the machine SAND runs on
   that something else keeps in step. Read off the spec rather than listed by
   name, so a backend added to the registry lands in the right section without
   this file knowing it exists. */
const isFolderBackend = (spec) =>
  spec.fields?.length > 0 && spec.fields.every((field) => field.directory)

function ProviderPicker({ specs, onChoose }) {
  const signIn = specs.filter((s) => s.oauth)
  const manual = specs.filter((s) => !s.oauth)
  const credentials = manual.filter((s) => !isFolderBackend(s))
  const folders = manual.filter(isFolderBackend)

  return (
    <>
      {signIn.length > 0 && <PickerHeading>Sign in with your account</PickerHeading>}
      {signIn.map((spec) => <ProviderCard key={spec.kind} spec={spec} onChoose={onChoose} />)}

      {credentials.length > 0 && <PickerHeading>Connect with credentials</PickerHeading>}
      {credentials.map((spec) => <ProviderCard key={spec.kind} spec={spec} onChoose={onChoose} />)}

      {folders.length > 0 && (
        <PickerHeading>Point at a folder on this machine</PickerHeading>
      )}
      {folders.map((spec) => <ProviderCard key={spec.kind} spec={spec} onChoose={onChoose} />)}
    </>
  )
}

function PickerHeading({ children }) {
  return (
    <div style={{
      fontFamily: FONT.mono,
      fontSize: '10px',
      fontWeight: 700,
      letterSpacing: '1.5px',
      textTransform: 'uppercase',
      color: COLORS.textMuted,
      margin: '4px 0 9px',
    }}>{children}</div>
  )
}

function ProviderCard({ spec, onChoose }) {
  const [hover, setHover] = useState(false)

  return (
    <button
      onClick={() => onChoose(spec)}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      style={{
        display: 'block',
        width: '100%',
        textAlign: 'left',
        padding: '13px 14px',
        marginBottom: '9px',
        background: hover ? COLORS.surfaceHover : COLORS.bg,
        border: `1px solid ${hover ? COLORS.borderBright : COLORS.border}`,
        borderRadius: '7px',
        cursor: 'pointer',
        color: COLORS.text,
        transition: 'background 0.15s ease, border-color 0.15s ease',
      }}
    >
      <div style={{
        display: 'flex', alignItems: 'center', gap: '9px',
        fontFamily: FONT.mono, fontSize: '13px', marginBottom: '5px',
      }}>
        <span>{KIND_ICONS[spec.kind] || '☁'}</span>
        <span style={{ flex: 1, minWidth: 0 }}>{spec.label}</span>
        {spec.oauth && (
          <span style={{
            fontSize: '9px',
            letterSpacing: '1px',
            padding: '2px 6px',
            borderRadius: '10px',
            color: spec.oauth.configured ? COLORS.success : COLORS.textMuted,
            border: `1px solid ${spec.oauth.configured ? COLORS.success : COLORS.border}66`,
          }}>{spec.oauth.configured ? 'ONE CLICK' : 'SIGN IN'}</span>
        )}
      </div>
      <div style={{
        fontFamily: FONT.sans, fontSize: '11.5px',
        color: COLORS.textMuted, lineHeight: 1.55,
      }}>{spec.description}</div>
    </button>
  )
}

/* The screen a sign-in starts from. With app credentials already configured on
   the server it is one button; without them, it is the shortest possible
   detour through the provider's developer console. */
function SignInStart({ spec, client, setClient, busy, onStart, onManual, onBack }) {
  const ready = spec.oauth.configured
    || (client.clientId.trim() && (!spec.oauth.secret_required || client.clientSecret.trim()))

  return (
    <>
      {!spec.oauth.configured && (
        <>
          <Banner tone="info">
            SAND ships no registered app of its own, so the first connection to{' '}
            {spec.label} needs one of yours. It takes a minute, and every later
            account reuses it.
          </Banner>

          <ol style={{
            margin: '0 0 16px', paddingLeft: '18px',
            fontFamily: FONT.sans, fontSize: '12px',
            color: COLORS.textDim, lineHeight: 1.7,
          }}>
            <li>
              {spec.oauth.console_url ? (
                <a href={spec.oauth.console_url} target="_blank" rel="noreferrer"
                  style={{ color: COLORS.accent }}>Open the {spec.label} developer console ↗</a>
              ) : 'Open the provider\'s developer console'}
            </li>
            {spec.oauth.console_help && <li>{spec.oauth.console_help}</li>}
            <li>Paste the client ID below.</li>
          </ol>

          <CopyField label="Redirect URI to register" value={callbackURL()} />

          <Input
            label="Client ID *"
            value={client.clientId}
            onChange={(e) => setClient({ ...client, clientId: e.target.value })}
            placeholder="from the developer console"
          />
          <PasswordInput
            label={`Client secret${spec.oauth.secret_required ? ' *' : ''}`}
            help={spec.oauth.secret_required ? undefined : 'Leave blank for a public client.'}
            value={client.clientSecret}
            onChange={(e) => setClient({ ...client, clientSecret: e.target.value })}
          />
        </>
      )}

      {spec.oauth.configured && (
        <p style={{
          fontFamily: FONT.sans, fontSize: '12.5px',
          color: COLORS.textDim, lineHeight: 1.6, margin: '0 0 16px',
        }}>
          You will be sent to {spec.label} to approve access, then straight back
          here. SAND keeps the tokens on this machine, inside the vault.
        </p>
      )}

      <Button
        variant="primary"
        onClick={onStart}
        disabled={busy || !ready}
        style={{ width: '100%', justifyContent: 'center', marginBottom: '12px' }}
      >
        {busy ? <Spinner size={12} color={COLORS.bg} /> : null}
        {busy ? 'Opening…' : spec.oauth.sign_in_label}
      </Button>

      <div style={{ display: 'flex', gap: '8px', justifyContent: 'space-between', alignItems: 'center' }}>
        <Button type="button" variant="ghost" onClick={onBack}>← Back</Button>
        <button
          type="button"
          onClick={onManual}
          style={{
            background: 'none', border: 'none', cursor: 'pointer',
            fontFamily: FONT.sans, fontSize: '11px', color: COLORS.textMuted,
            textDecoration: 'underline', padding: 0,
          }}
        >Paste tokens manually instead</button>
      </div>
    </>
  )
}

/* Waiting on the provider's window. Also the home of the escape hatch for the
   case the redirect cannot reach this server — a vault on localhost being
   driven from a phone, most often. */
function SignInWaiting({ spec, flow, pasted, setPasted, showPaste, setShowPaste, busy, onPaste, onCancel }) {
  return (
    <>
      <div style={{
        display: 'flex', alignItems: 'center', gap: '10px',
        padding: '14px 0 16px',
        fontFamily: FONT.sans, fontSize: '13px', color: COLORS.textDim,
      }}>
        <Spinner size={14} />
        Waiting for {spec.label} to hand the account back…
      </div>

      <p style={{
        fontFamily: FONT.sans, fontSize: '11.5px',
        color: COLORS.textMuted, lineHeight: 1.6, margin: '0 0 14px',
      }}>
        Approve the request in the window that opened. If nothing appeared, your
        browser blocked it — cancel and try again, or finish it by hand below.
      </p>

      {!showPaste && (
        <button
          type="button"
          onClick={() => setShowPaste(true)}
          style={{
            background: 'none', border: 'none', cursor: 'pointer', padding: 0,
            fontFamily: FONT.sans, fontSize: '11px', color: COLORS.textMuted,
            textDecoration: 'underline', marginBottom: '14px',
          }}
        >The page did not come back — paste the URL instead</button>
      )}

      {showPaste && (
        <>
          <p style={{
            fontFamily: FONT.sans, fontSize: '11.5px',
            color: COLORS.textMuted, lineHeight: 1.6, margin: '0 0 10px',
          }}>
            If the provider redirected to an address this browser cannot reach,
            copy the whole URL it landed on — the one starting{' '}
            <code style={{ color: COLORS.textDim }}>{flow?.redirect_uri || callbackURL()}</code>{' '}
            — and paste it here.
          </p>
          <Input
            label="Redirect URL"
            value={pasted}
            onChange={(e) => setPasted(e.target.value)}
            placeholder="http://…/api/providers/oauth/callback?code=…&state=…"
          />
          <Button
            variant="primary"
            onClick={onPaste}
            disabled={busy || !pasted.trim()}
            style={{ width: '100%', justifyContent: 'center', marginBottom: '12px' }}
          >{busy ? 'Finishing…' : 'Finish sign-in'}</Button>
        </>
      )}

      <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
        <Button type="button" variant="ghost" onClick={onCancel}>Cancel</Button>
      </div>
    </>
  )
}

/* Known services, one click away from a filled-in form. */
function Presets({ spec, onApply }) {
  const [applied, setApplied] = useState(null)
  if (!spec.presets || spec.presets.length === 0) return null

  const chosen = spec.presets.find((preset) => preset.key === applied)

  return (
    <div style={{ marginBottom: '14px' }}>
      <span style={{
        display: 'block',
        fontFamily: FONT.mono, fontSize: '10px', fontWeight: 600,
        letterSpacing: '1.5px', textTransform: 'uppercase',
        color: COLORS.textMuted, marginBottom: '8px',
      }}>Start from</span>

      <div style={{ display: 'flex', flexWrap: 'wrap', gap: '6px' }}>
        {spec.presets.map((preset) => {
          const active = preset.key === applied
          return (
            <button
              key={preset.key}
              type="button"
              onClick={() => { setApplied(preset.key); onApply(preset) }}
              style={{
                padding: '5px 10px',
                background: active ? COLORS.accentDim : COLORS.bg,
                border: `1px solid ${active ? COLORS.accent : COLORS.border}`,
                borderRadius: '20px',
                color: active ? COLORS.text : COLORS.textDim,
                fontFamily: FONT.mono,
                fontSize: '11px',
                cursor: 'pointer',
              }}
            >{preset.label}</button>
          )
        })}
      </div>

      {chosen?.help && (
        <span style={{
          display: 'block', marginTop: '7px',
          fontFamily: FONT.sans, fontSize: '11px',
          color: COLORS.textMuted, lineHeight: 1.45,
        }}>{chosen.help}</span>
      )}
    </div>
  )
}
