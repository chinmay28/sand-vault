import React, { useState } from 'react'
import { COLORS, FONT } from '../theme'
import { Input, PasswordInput, TextArea } from './ui'
import DirectoryPicker from './DirectoryPicker'

/* What a stored secret looks like once it has left the vault.

   The server never sends a real one back — it substitutes this — so an account
   whose options carry it has a secret set, and one whose options carry an empty
   string has none. Worth being able to tell apart: "there is a client secret,
   you just cannot see it" and "there is no client secret" are different answers
   to the same field, and a blank box says neither.

   Keep it in step with provider.RedactedSecret in the Go side. */
export const STORED_SECRET = '\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022'

/* The generated part of a backend's form.

   Every backend describes its own settings — what they are called, which are
   secret, which name a folder on this machine — and both dialogs that ask for
   them draw the form from that description rather than from a list of their
   own. A backend added to the registry gets a connect form and an edit form
   without either file knowing it exists.

   `secretPlaceholder` is what the edit dialog needs and the connect dialog does
   not. A connected account's secrets never reach the browser, so a field for
   one starts empty and empty means "leave it alone" — the placeholder is what
   says so, in the box, instead of the field looking like a credential somebody
   deleted. */
export default function SpecFields({ fields, values, onChange, secretPlaceholder, disabled }) {
  return fields.map((field) => {
    if (field.directory) {
      return (
        <DirectoryField
          key={field.key}
          field={field}
          value={values[field.key] || ''}
          disabled={disabled}
          onChange={(value) => onChange(field.key, value)}
        />
      )
    }

    /* A key is pasted, not typed, so it gets a box big enough to paste into
       and to check afterwards. Secret and multiline together resolve to the
       textarea: masking a PEM block would hide the only part of it worth
       looking at. */
    if (field.multiline) {
      return (
        <TextArea
          key={field.key}
          label={field.label + (field.required ? ' *' : '')}
          help={field.help}
          placeholder={field.secret && secretPlaceholder ? secretPlaceholder : field.placeholder}
          value={values[field.key] || ''}
          disabled={disabled}
          onChange={(e) => onChange(field.key, e.target.value)}
        />
      )
    }

    const Control = field.secret ? PasswordInput : Input
    return (
      <Control
        key={field.key}
        label={field.label + (field.required ? ' *' : '')}
        help={field.help}
        placeholder={field.secret && secretPlaceholder ? secretPlaceholder : field.placeholder}
        value={values[field.key] || ''}
        disabled={disabled}
        onChange={(e) => onChange(field.key, e.target.value)}
      />
    )
  })
}

/* A folder on the machine SAND runs on. Still a text field — a path pasted
   from somewhere else is the fastest way in when you have one — with a browse
   button for when you do not, which is most of the time on a phone. */
function DirectoryField({ field, value, disabled, onChange }) {
  const [picking, setPicking] = useState(false)

  return (
    <>
      <Input
        label={field.label + (field.required ? ' *' : '')}
        help={field.help}
        placeholder={field.placeholder}
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
        style={{ paddingRight: '74px' }}
        trailing={
          <button
            type="button"
            onClick={() => setPicking(true)}
            disabled={disabled}
            style={{
              position: 'absolute', top: 0, bottom: 0, right: '4px',
              display: 'flex', alignItems: 'center', justifyContent: 'center',
              width: '66px', background: 'none', border: 'none',
              color: COLORS.accent, cursor: disabled ? 'not-allowed' : 'pointer',
              fontFamily: FONT.mono, fontSize: '10px', letterSpacing: '1px', padding: 0,
            }}
          >BROWSE…</button>
        }
      />

      {picking && (
        <DirectoryPicker
          value={value}
          title={`Choose the ${field.label.toLowerCase()}`}
          onPick={(path) => { onChange(path); setPicking(false) }}
          onClose={() => setPicking(false)}
        />
      )}
    </>
  )
}
