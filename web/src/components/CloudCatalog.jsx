import { useMemo, useState } from 'react'
import { COLORS, FONT, KIND_ICONS } from '../theme'
import { Empty, Input, Modal } from './ui'

/* Everything SAND can hold parts on, which is a longer list than the connect
   dialog can be without becoming unusable.

   The gap this closes: two of the backends are protocols rather than services.
   "S3-compatible storage" is a true description and a useless answer to *can
   it hold my Google Cloud Storage bucket* — it can, and nothing in the dialog
   said so. Each backend now declares the services it reaches (`covers` on the
   spec), and this window is that list, read from the same registry the connect
   form is built from so the two cannot drift apart. */

/* Which section a backend belongs in — the same three the picker uses, decided
   the same way, so a backend added to the registry lands in the right place in
   both without either file knowing it exists. */
const isFolderBackend = (spec) =>
  spec.fields?.length > 0 && spec.fields.every((field) => field.directory)

const SECTIONS = [
  {
    key: 'signin',
    title: 'Sign in with your account',
    note: 'Approved on the provider’s own page. SAND holds the tokens; the browser never sees them.',
    holds: (spec) => !!spec.oauth,
  },
  {
    key: 'credentials',
    title: 'Connect with credentials',
    note: 'Two protocols rather than two services — every name below is reachable today.',
    holds: (spec) => !spec.oauth && !isFolderBackend(spec),
  },
  {
    key: 'folder',
    title: 'Point at a folder on this machine',
    note: 'For services with no API for anyone else: their desktop app carries the parts up, and the parts are encrypted before it sees them.',
    holds: isFolderBackend,
  },
]

/* How a backend is connected, for the backends whose label is already the
   service and so declare no `covers` of their own. */
function howToConnect(spec) {
  if (spec.oauth) return 'Sign in from the browser'
  if (isFolderBackend(spec)) return 'Needs its desktop app on the machine SAND runs on'
  return 'Credentials you paste in'
}

const contains = (text, query) => (text || '').toLowerCase().includes(query)

/* A backend matches a search by its own name, or by any service it covers —
   which is the point: people search for "Wasabi", not for "S3".

   What comes back with it differs. Matching the backend shows everything it
   reaches, because the question was about the backend. Matching one service
   shows that service alone: answering "does it do Wasabi?" with seventeen
   lines of S3 is the pile this window exists to sort out. */
function search(spec, query) {
  const all = spec.covers || []
  if (!query) return { hit: true, covers: all }

  // Only the label and the kind count as naming the backend. A description
  // reads "Amazon S3, Cloudflare R2, Backblaze B2, Wasabi, MinIO, or any other
  // service speaking the S3 API" — matching that would answer every one of
  // those searches with all seventeen lines, which is the thing being fixed.
  if (contains(spec.label, query) || contains(spec.kind, query)) {
    return { hit: true, covers: all }
  }

  const covers = all.filter(
    (service) => contains(service.name, query) || contains(service.hint, query))
  if (covers.length > 0) return { hit: true, covers }

  // Nothing named, but the prose still knows the answer — "SigV4", say, or a
  // protocol nobody thinks of as a service.
  return { hit: contains(spec.description, query), covers: all }
}

export default function CloudCatalog({ specs = [], onClose }) {
  const [query, setQuery] = useState('')
  const needle = query.trim().toLowerCase()

  const sections = useMemo(() => SECTIONS.map((section) => ({
    ...section,
    entries: specs
      .filter(section.holds)
      .map((spec) => ({ spec, ...search(spec, needle) }))
      .filter((entry) => entry.hit),
  })).filter((section) => section.entries.length > 0), [specs, needle])

  // Rows rather than services: a line naming three small providers together
  // counts once, so the number is only ever an undercount.
  const named = specs.reduce((total, spec) => total + Math.max(1, (spec.covers || []).length), 0)

  return (
    <Modal
      title="Every cloud SAND can hold parts on"
      subtitle={`${specs.length} backends covering ${named} services — and any other server speaking S3 or WebDAV, named here or not.`}
      onClose={onClose}
      width={620}
      // Above the connect dialog this opens from, so Escape closes this one
      // first and leaves the picker where it was.
      zIndex={120}
    >
      <Input
        label="Find a service"
        value={query}
        onChange={(e) => setQuery(e.target.value)}
        placeholder="wasabi, seafile, nextcloud…"
      />

      {sections.length === 0 && (
        <Empty icon="⌕" title={`Nothing here matches “${query.trim()}”`}>
          It may still work: anything speaking the S3 or WebDAV protocol connects
          with those backends whether or not it is named here.
        </Empty>
      )}

      {sections.map((section) => (
        <div key={section.key} style={{ marginBottom: '18px' }}>
          <SectionHeading>{section.title}</SectionHeading>
          <p style={{
            margin: '0 0 10px',
            fontSize: '12px',
            lineHeight: 1.5,
            color: COLORS.textMuted,
          }}>{section.note}</p>
          {section.entries.map(({ spec, covers }) => (
            <BackendEntry key={spec.kind} spec={spec} covers={covers} />
          ))}
        </div>
      ))}

      <p style={{
        margin: '4px 0 0',
        fontSize: '12px',
        lineHeight: 1.6,
        color: COLORS.textMuted,
      }}>
        Spread parts across accounts that can fail separately — different
        companies, different logins, different countries. Three buckets at one
        provider are one account wearing three hats.
      </p>
    </Modal>
  )
}

function SectionHeading({ children }) {
  return (
    <div style={{
      fontFamily: FONT.mono,
      fontSize: '10px',
      fontWeight: 700,
      letterSpacing: '1.5px',
      textTransform: 'uppercase',
      color: COLORS.textMuted,
      margin: '4px 0 6px',
    }}>{children}</div>
  )
}

function BackendEntry({ spec, covers = [] }) {
  return (
    <div style={{
      padding: '11px 12px',
      marginBottom: '8px',
      background: COLORS.bg,
      border: `1px solid ${COLORS.border}`,
      borderRadius: '7px',
    }}>
      <div style={{ display: 'flex', alignItems: 'baseline', gap: '8px' }}>
        <span aria-hidden="true" style={{ fontSize: '13px' }}>{KIND_ICONS[spec.kind] || '☁'}</span>
        <span style={{ fontFamily: FONT.mono, fontSize: '13px', color: COLORS.text }}>
          {spec.label}
        </span>
        <code style={{
          fontFamily: FONT.mono,
          fontSize: '10px',
          color: COLORS.textMuted,
        }}>{spec.kind}</code>
      </div>

      {covers.length === 0 ? (
        <div style={{
          marginTop: '5px',
          fontSize: '12px',
          color: COLORS.textMuted,
        }}>{howToConnect(spec)}</div>
      ) : (
        <ul style={{ margin: '7px 0 0', padding: 0, listStyle: 'none' }}>
          {covers.map((service) => (
            <li key={service.name} style={{
              display: 'flex',
              flexWrap: 'wrap',
              gap: '4px 8px',
              padding: '2px 0',
              fontSize: '12px',
              lineHeight: 1.5,
            }}>
              <span style={{ color: COLORS.text }}>{service.name}</span>
              {service.hint && (
                <span style={{
                  fontFamily: FONT.mono,
                  fontSize: '11px',
                  color: COLORS.textMuted,
                  wordBreak: 'break-word',
                }}>{service.hint}</span>
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
