import React, { useEffect, useRef, useState } from 'react'
import { COLORS, FONT } from '../theme'
import { useIsMobile } from '../hooks'
import { api } from '../api'
import { Banner, Button, Input, Modal, PasswordInput, Spinner } from './ui'

/* Ask the vault a question in plain words.

   The answer comes from a language model the user runs on a machine of their
   own — Ollama on the PC with the graphics card, say — which SAND hands three
   tools: list the films, search the vault by name, search the film database.
   The model sees what those tools return and nothing else: names, paths,
   sizes and stored film titles. Never a file's contents, never an account.

   The conversation lives here, in the browser, and is sent back with every
   question. The server keeps nothing between questions, which is what makes
   locking the vault end the conversation the way it ends everything else. */

/* What the assistant sends, said where it is set up and where it is used. */
const PRIVACY = 'The assistant sends the names of your files and folders, and the ' +
  'film titles stored against them, to the model server at the address you ' +
  'give it — and nothing else: not a file’s contents, not an account, not a ' +
  'key. Run the model on a machine you own, on your own network. The one ' +
  'lookup that leaves that network is a film database search, which sends ' +
  'the title being searched for and nothing about your vault.'

/* Where to start, said once. */
const URL_HELP = 'The server’s OpenAI-compatible address. Ollama is ' +
  'http://<pc>:11434/v1 once it is set to listen on the network ' +
  '(OLLAMA_HOST=0.0.0.0); vLLM is http://<pc>:8000/v1.'

const MODEL_HELP = 'A model the server already holds — for Ollama, one you have ' +
  'pulled. It must be able to call tools: qwen3:14b or gpt-oss:20b on a ' +
  '16 GB card, llama3.1:8b on less.'

/* The questions a first-timer is most likely to want, offered as buttons so
   the panel explains itself by example rather than by paragraph. */
const SUGGESTIONS = [
  'What Batman movies are missing from my collection?',
  'Which films do I have from the 1980s?',
  'What have I got called “holiday”?',
]

/* A tool the model ran, as a small tag under its answer: what was looked up,
   so that a reader can judge the answer by what it was made from. */
function StepTag({ step }) {
  let detail = ''
  try {
    const args = step.arguments || {}
    detail = args.query || args.dir || ''
  } catch {
    detail = ''
  }
  const label = {
    list_films: 'Listed the films',
    search_vault: 'Searched the vault',
    search_film_database: 'Searched the film database',
  }[step.tool] || step.tool

  return (
    <span
      title={step.error ? `Failed: ${step.error}` : undefined}
      style={{
        display: 'inline-flex', alignItems: 'center', gap: '5px',
        padding: '2px 8px',
        borderRadius: '10px',
        background: step.error ? `${COLORS.error}18` : COLORS.surfaceRaised,
        border: `1px solid ${step.error ? `${COLORS.error}66` : COLORS.border}`,
        fontFamily: FONT.mono, fontSize: '10px', letterSpacing: '0.3px',
        color: step.error ? COLORS.error : COLORS.textMuted,
        maxWidth: '100%',
      }}
    >
      <span aria-hidden="true">{step.error ? '✗' : '⌕'}</span>
      <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
        {label}{detail ? ` · ${detail}` : ''}
      </span>
    </span>
  )
}

/* One turn. The user's on the right in the accent, the assistant's on the
   left in the surface colour, the way every chat since the first has drawn
   it — a convention worth keeping because it is one nobody has to learn. */
function Bubble({ turn }) {
  const mine = turn.role === 'user'
  return (
    <div style={{
      display: 'flex', flexDirection: 'column',
      alignItems: mine ? 'flex-end' : 'flex-start',
      gap: '6px',
    }}>
      <div
        role="article"
        aria-label={mine ? 'You' : 'Assistant'}
        style={{
          maxWidth: '88%',
          padding: '9px 12px',
          borderRadius: mine ? '12px 12px 3px 12px' : '12px 12px 12px 3px',
          background: mine ? `${COLORS.accent}22` : COLORS.surfaceRaised,
          border: `1px solid ${mine ? `${COLORS.accent}55` : COLORS.border}`,
          fontFamily: FONT.sans, fontSize: '13px', lineHeight: 1.55,
          color: COLORS.text,
          whiteSpace: 'pre-wrap', overflowWrap: 'anywhere',
        }}
      >
        {turn.content}
      </div>
      {turn.steps?.length > 0 && (
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: '4px', maxWidth: '88%' }}>
          {turn.steps.map((step, i) => <StepTag key={i} step={step} />)}
        </div>
      )}
    </div>
  )
}

/* The panel. `chat` and `onChat` live in the app so a closed panel reopens
   on the same conversation; `vault` names which vault the questions are
   about, since a sub vault's films are its own. */
export default function Assistant({ chat, onChat, vault = '', onClose }) {
  const mobile = useIsMobile()
  const [settings, setSettings] = useState(null)
  const [settingsOpen, setSettingsOpen] = useState(false)
  const [question, setQuestion] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const scroller = useRef(null)
  const inputRef = useRef(null)

  useEffect(() => {
    let cancelled = false
    api.assistantSettings()
      .then((resp) => { if (!cancelled) setSettings(resp) })
      .catch((err) => { if (!cancelled) setError(err.message) })
    return () => { cancelled = true }
  }, [])

  /* The newest turn is what you came to read. */
  useEffect(() => {
    const node = scroller.current
    if (node) node.scrollTop = node.scrollHeight
  }, [chat, busy])

  const configured = !!settings?.configured

  const ask = async (text) => {
    const content = text.trim()
    if (!content || busy) return
    setError(null)
    setQuestion('')

    const next = [...chat, { role: 'user', content }]
    onChat(next)
    setBusy(true)
    try {
      /* Text only goes over: the tags under earlier answers are how they
         were reached, not part of the conversation. */
      const transcript = next.map(({ role, content: c }) => ({ role, content: c }))
      const resp = await api.askAssistant(transcript, { vault })
      onChat([...next, { role: 'assistant', content: resp.text, steps: resp.steps || [] }])
    } catch (err) {
      if (err.code === 'NO_ASSISTANT') {
        setSettings((s) => ({ ...(s || {}), configured: false }))
      }
      setError(err.message)
      /* The question stays asked, so it can be sent again once whatever
         went wrong is fixed. */
    } finally {
      setBusy(false)
      inputRef.current?.focus()
    }
  }

  const clear = () => { onChat([]); setError(null) }

  return (
    <Modal
      title="Ask the vault"
      subtitle={configured
        ? `${settings.model} on your own network, reading the index`
        : 'A model on your own network, reading the index'}
      onClose={() => !busy && onClose()}
      width={640}
    >
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}

      {settings && !configured && (
        <div style={{
          padding: '14px',
          background: COLORS.bg, border: `1px solid ${COLORS.border}`, borderRadius: '8px',
          marginBottom: '14px',
        }}>
          <div style={{
            fontFamily: FONT.mono, fontSize: '12px', fontWeight: 600, color: COLORS.text,
            marginBottom: '6px',
          }}>No model server yet</div>
          <div style={{
            fontFamily: FONT.sans, fontSize: '12px', lineHeight: 1.55, color: COLORS.textDim,
            marginBottom: '12px',
          }}>
            Point SAND at a model running on a machine of your own — Ollama or
            vLLM on the PC with the graphics card — and it can answer questions
            like “what Batman films am I missing?” from your index.
          </div>
          <Button variant="primary" size="sm" onClick={() => setSettingsOpen(true)}>
            Set up the assistant
          </Button>
        </div>
      )}

      {/* The conversation. Fixed-height and scrolling inside the dialog, so
          the question box stays under your hands however long it gets. */}
      <div
        ref={scroller}
        aria-live="polite"
        style={{
          height: mobile ? 'calc(var(--app-height) - 330px)' : '380px',
          minHeight: '160px',
          overflowY: 'auto',
          display: 'flex', flexDirection: 'column', gap: '12px',
          padding: '12px',
          background: COLORS.bg, border: `1px solid ${COLORS.border}`, borderRadius: '8px',
          marginBottom: '12px',
        }}
      >
        {chat.length === 0 && !busy && (
          <div style={{ margin: 'auto 0', textAlign: 'center' }}>
            <div style={{
              fontFamily: FONT.sans, fontSize: '12px', lineHeight: 1.55, color: COLORS.textMuted,
              marginBottom: '14px',
            }}>
              It can list your films, search by name, and check the film
              database for what a series has that you do not. Try one of these:
            </div>
            <div style={{ display: 'flex', flexDirection: 'column', gap: '6px', alignItems: 'center' }}>
              {SUGGESTIONS.map((s) => (
                <Button
                  key={s}
                  size="sm"
                  disabled={!configured}
                  onClick={() => ask(s)}
                  style={{ whiteSpace: 'normal', textAlign: 'left', fontFamily: FONT.sans, fontWeight: 500 }}
                >{s}</Button>
              ))}
            </div>
          </div>
        )}

        {chat.map((turn, i) => <Bubble key={i} turn={turn} />)}

        {busy && (
          <div style={{
            display: 'flex', alignItems: 'center', gap: '8px',
            fontFamily: FONT.mono, fontSize: '11px', color: COLORS.textMuted,
          }}>
            <Spinner size={12} /> Looking that up…
          </div>
        )}
      </div>

      <form
        onSubmit={(e) => { e.preventDefault(); ask(question) }}
        style={{ display: 'flex', gap: '8px', alignItems: 'flex-start' }}
      >
        {/* A bare field rather than the shared Input: that one carries the
            margin a form row wants, and this row is the button's height. */}
        <input
          ref={inputRef}
          aria-label="Your question"
          placeholder={configured ? 'Ask about your files or films…' : 'Set up a model server first'}
          value={question}
          autoComplete="off"
          disabled={!configured || busy}
          onChange={(e) => setQuestion(e.target.value)}
          style={{
            flex: 1, minWidth: 0,
            padding: '10px 12px',
            background: COLORS.bg,
            border: `1px solid ${COLORS.border}`,
            borderRadius: '6px',
            color: COLORS.text,
            fontFamily: FONT.sans,
            fontSize: '13px',
            outline: 'none',
            boxSizing: 'border-box',
          }}
        />
        <Button type="submit" variant="primary" disabled={!configured || busy || !question.trim()}>
          Ask
        </Button>
      </form>

      <div style={{
        display: 'flex', alignItems: 'center', gap: '8px', flexWrap: 'wrap',
        marginTop: '12px',
      }}>
        <span style={{
          flex: 1, minWidth: '200px',
          fontFamily: FONT.sans, fontSize: '10.5px', lineHeight: 1.5, color: COLORS.textMuted,
        }}>
          Answers are made only from what the tools return, and say what was
          looked up. The model can still misread a list — check the answer
          against your files before acting on it.
        </span>
        {chat.length > 0 && (
          <Button size="sm" variant="ghost" disabled={busy} onClick={clear}>Clear</Button>
        )}
        <Button size="sm" variant="ghost" onClick={() => setSettingsOpen(true)}>
          {configured ? 'Model…' : 'Settings…'}
        </Button>
      </div>

      {settingsOpen && (
        <AssistantSettings
          zIndex={110}
          onClose={() => setSettingsOpen(false)}
          onChanged={(resp) => { setSettings(resp); setError(null) }}
        />
      )}
    </Modal>
  )
}

/* Where the assistant's model runs: a URL, a model name, and a token for the
   servers that want one. Kept where the rest of the vault's own settings are,
   beside the film key, because it is the vault's rather than any folder's. */
export function AssistantSettings({ onClose, onChanged, zIndex }) {
  const [settings, setSettings] = useState(null)
  const [url, setUrl] = useState('')
  const [model, setModel] = useState('')
  const [key, setKey] = useState('')
  const [keyTouched, setKeyTouched] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const [saved, setSaved] = useState(false)

  useEffect(() => {
    api.assistantSettings()
      .then((resp) => {
        setSettings(resp)
        setUrl(resp.url || '')
        setModel(resp.model || '')
      })
      .catch((err) => setError(err.message))
  }, [])

  /* Saving checks the server before anything is kept: it has to answer, and
     it has to list the model, so a PC that is off or a model never pulled
     fails here rather than on the first question. */
  const store = async (next) => {
    setBusy(true)
    setError(null)
    setSaved(false)
    try {
      const resp = await api.setAssistant(next)
      setSettings(resp)
      setUrl(resp.url || '')
      setModel(resp.model || '')
      setKey('')
      setKeyTouched(false)
      setSaved(resp.configured)
      onChanged?.(resp)
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  const submit = (e) => {
    e.preventDefault()
    store({ url: url.trim(), model: model.trim(), key: keyTouched ? key.trim() : undefined })
  }

  return (
    <Modal
      title="Assistant"
      subtitle="A model on a machine you own answers questions about the vault"
      onClose={() => !busy && onClose()}
      width={560}
      zIndex={zIndex}
    >
      {error && <Banner tone="error" onDismiss={() => setError(null)}>{error}</Banner>}
      {saved && (
        <Banner tone="success" onDismiss={() => setSaved(false)}>
          The server answered and has {settings?.model}. The assistant is ready.
        </Banner>
      )}

      <Banner tone="info">{PRIVACY}</Banner>

      {!settings && !error && <Spinner />}

      {settings && (
        <form onSubmit={submit}>
          <Input
            label="Model server"
            value={url}
            autoFocus={!settings.configured}
            autoComplete="off"
            spellCheck="false"
            placeholder="http://gaming-pc:11434/v1"
            onChange={(e) => setUrl(e.target.value)}
            help={URL_HELP}
          />
          <Input
            label="Model"
            value={model}
            autoComplete="off"
            spellCheck="false"
            placeholder="qwen3:14b"
            onChange={(e) => setModel(e.target.value)}
            help={MODEL_HELP}
          />
          <PasswordInput
            label={settings.has_key ? 'Token (one is stored — leave blank to keep it)' : 'Token (optional)'}
            value={key}
            autoComplete="off"
            placeholder={settings.has_key ? '••••••••' : 'Only if the server asks for one'}
            onChange={(e) => { setKey(e.target.value); setKeyTouched(true) }}
            help="Ollama needs none. Sent as a bearer token to a server started with one."
          />

          <div style={{ display: 'flex', gap: '8px', justifyContent: 'flex-end', flexWrap: 'wrap' }}>
            {settings.configured && (
              <Button
                type="button"
                variant="ghost"
                disabled={busy}
                title="Forget the server. Nothing else about the vault changes."
                onClick={() => store({ url: '', model: '', key: '' })}
              >Remove</Button>
            )}
            <Button type="submit" variant="primary" disabled={busy || !url.trim() || !model.trim()}>
              {busy ? <><Spinner size={12} color={COLORS.bg} /> Checking…</> : 'Check and save'}
            </Button>
          </div>
        </form>
      )}
    </Modal>
  )
}
