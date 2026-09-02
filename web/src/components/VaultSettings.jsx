import React, { useEffect, useState } from 'react'
import { COLORS, FONT } from '../theme'
import { api } from '../api'
import { Banner, Button, Input, Modal } from './ui'
import ChangePassword from './ChangePassword'
import CloudHealth, { Choice } from './CloudHealth'
import MountDrive from './MountDrive'
import { FilmKeySettings } from './FilmDetails'
import SubVaults from './SubVaults'
import RecoveryKit from './RecoveryKit'
import { StrayParts } from './CleanOrphans'
import {
  DefaultClouds, PARTS_PER_FILE, defaultSchemeFor, parseScheme, schemeName,
} from './CloudSelect'

/* Everything you set on the vault itself, in one place.

   These used to be a row of tiles at the foot of the accounts drawer, one per
   setting, which worked while there were two of them and stopped working at
   four: a phone's drawer was three lines of grey figures and then a wall of
   little boxes, and every new setting made it worse. A row of tiles is also
   the wrong shape for the answer to "where do I change X" — it can only say
   the four things it happens to show, and it says them in the space where the
   thing you were actually looking at, your accounts, ought to be.

   So the drawer carries one button now and this is what it opens: a list, in
   the order the questions come up, each line saying what it is and what it is
   currently set to. Each still opens the dialog it always did — this menu
   holds no settings of its own, it only knows where they live.

   What is *not* here is anything that is not a setting. Connecting a cloud is
   an action and stays a button of its own; an unfinished re-encryption is news
   and stays a banner. A menu that mixes those in becomes a second home screen.

   The last line in the list breaks that rule, and the reason is worth writing
   down so that the next exception has to earn its place the same way. Stray parts on the clouds are news
   too — the app scans when the accounts change and says so in a banner — but
   that banner is dismissible and only ever appears when there is something to
   say, so the panels behind it could not be reached at all once it was gone or
   had never come. That is not a menu deciding to hold an action; it is the
   only door to a room the app otherwise reaches into and then locks. A line
   here is admissible on exactly that ground and no other: not because
   something was hard to find, but because there was no way in. */

/* The dialogs this opens are opened over it rather than instead of it, so
   closing one puts you back on the list you chose it from. */
const CHILD_Z = 110

export default function VaultSettings({
  providers, stats, webdav, health, onHealthChanged,
  subVaults = [], showSubVaults, subVaultShown,
  onToggleSubVaults, onToggleSubVault, onOpenSubVault, onClose, onChanged,
}) {
  const [open, setOpen] = useState(null)
  const [filmKey, setFilmKey] = useState(null)
  const [kit, setKit] = useState(null)
  const [links, setLinks] = useState(null)

  const defaults = (stats?.default_accounts || []).filter(
    (id) => providers.some((p) => p.id === id))

  /* Only so its line can say whether there is one — the key itself never
     leaves the server. Asked when this menu opens rather than when the app
     does, since nothing before now had a use for the answer. */
  useEffect(() => {
    let cancelled = false
    api.movieSettings()
      .then((resp) => { if (!cancelled) setFilmKey(!!resp.has_key) })
      .catch(() => {})
    return () => { cancelled = true }
  }, [])

  /* Only so the line can say how stale the kit is, which is the one thing
     about it worth reading without opening anything. */
  const loadKit = () => api.kitStatus().then(setKit).catch(() => {})
  useEffect(() => { loadKit() }, [])

  /* How long a folder's download link stays good, for the line to read out.
     Asked here rather than carried in the status, since nothing else in the
     app needs it. */
  useEffect(() => {
    let cancelled = false
    api.linkSettings()
      .then((resp) => { if (!cancelled) setLinks(resp) })
      .catch(() => {})
    return () => { cancelled = true }
  }, [])

  const close = () => setOpen(null)

  return (
    <Modal
      title="Vault settings"
      subtitle="Everything that belongs to this vault rather than to one account or one folder"
      onClose={onClose}
      width={520}
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: '8px' }}>
        {providers.length > 0 && (
          <Setting
            icon="☁️"
            label="Default clouds"
            hint="Where an upload goes unless it picks its own"
            /* The code as well as the count, because a default of five clouds
               says nothing on its own about what five clouds means here. */
            status={defaults.length > 0
              ? `${defaults.length} of ${providers.length} · ${schemeName(
                parseScheme(stats?.default_scheme) || defaultSchemeFor(defaults.length))}`
              : `${PARTS_PER_FILE} per upload`}
            onClick={() => setOpen('defaults')}
          />
        )}

        {/* How often the clouds are asked whether they are still there, which
            is a setting on the vault in exactly the way the placement policy
            is: it belongs to no one account and to no one folder. The line
            reports the schedule rather than the finding — the drawer's own
            line does the finding, and this menu is where somebody comes to
            change how often it is refreshed. It borrows the warning colour
            when something is actually down, because a settings list that knew
            and said nothing would be the wrong kind of quiet. */}
        {providers.length > 0 && (
          <Setting
            icon="📡"
            label="Cloud health"
            hint="How often SAND checks that every cloud is still answering"
            status={healthStatusLine(health)}
            tone={health?.unhealthy > 0 ? COLORS.error : undefined}
            onClick={() => setOpen('health')}
          />
        )}

        {/* A folder downloaded as a zip is fetched through an address that
            carries its own credential, so it can be handed to a download
            manager or another device — and how long such an address should
            stay good is the vault owner's call, not a constant. */}
        <Setting
          icon="🔗"
          label="Download links"
          hint="How long a folder's download address stays good"
          status={links ? describeHours(links.hours) : '…'}
          onClick={() => setOpen('links')}
        />

        <Setting
          icon="🔑"
          label="Password"
          hint="Re-encrypts every stored file onto the new key"
          /* Nothing to report unless a change was left half-finished, which is
             the one thing about a password this list can actually tell you. */
          status={stats?.pending_migration > 0
            ? `${stats.pending_migration} on the old key`
            : null}
          tone={COLORS.warn}
          onClick={() => setOpen('password')}
        />

        {/* The one artefact that brings the clouds back too. A recovery from
            an account's manifest.sand rebuilds the index and leaves every
            sign-in to be done by hand; this is what carries them.

            Its standing is the two facts that reduce what it can do — a new
            account, a changed password — rather than its age, because a merely
            old kit still recovers everything through the newer index sitting
            on the clouds. */}
        <Setting
          icon="🧰"
          label="Recovery kit"
          hint="One sealed file that brings the clouds back too"
          status={kitStatusLine(kit)}
          tone={kitStatusTone(kit)}
          onClick={() => setOpen('kit')}
        />

        {/* A sub vault is a vault inside this one, with a password of its own.
            The line says how many there are and how many are open, because
            "open" is the whole state that matters: a locked one is listed and
            unreadable, which is the point of it. */}
        <Setting
          icon="🔒"
          label="Sub vaults"
          hint="Sealed under their own passwords, and never on a mounted drive"
          status={subVaults.length === 0
            ? 'None'
            : `${subVaults.length}, ${subVaults.filter((s) => s.unlocked).length} open`}
          onClick={() => setOpen('subvaults')}
        />

        <Setting
          icon="🎬"
          label="Film key"
          hint="For the folders matched against the film database"
          status={filmKey === null ? '…' : filmKey ? 'Stored' : 'Not set'}
          tone={filmKey ? COLORS.textDim : COLORS.textMuted}
          onClick={() => setOpen('film')}
        />

        {/* Absent unless the server was started with --webdav: telling
            someone to mount a share that is not being served is worse than
            not mentioning it. */}
        {webdav?.path && (
          <Setting
            icon="💾"
            label="Mount as a drive"
            hint="Open the vault in a file manager, or a player"
            onClick={() => setOpen('mount')}
          />
        )}

        {/* The one line here that is not a setting — see the note at the top
            for why it is allowed to be. Alone in this list it reports nothing
            on the right: the answer is a full listing of every account, slow
            enough that opening a settings menu must not quietly start one. So
            this line is a question rather than a reading, and the scan begins
            when it is asked. */}
        {providers.length > 0 && (
          <Setting
            icon="🧹"
            label="Stray parts"
            hint="What your clouds hold that no file here points at"
            onClick={() => setOpen('strays')}
          />
        )}
      </div>

      {open === 'defaults' && (
        <DefaultClouds
          providers={providers}
          defaults={stats?.default_accounts || []}
          defaultScheme={stats?.default_scheme || ''}
          zIndex={CHILD_Z}
          onClose={close}
          onChanged={onChanged}
        />
      )}

      {open === 'password' && (
        <ChangePassword
          stats={stats}
          zIndex={CHILD_Z}
          onClose={close}
          onChanged={onChanged}
        />
      )}

      {open === 'subvaults' && (
        <SubVaults
          subVaults={subVaults}
          showSubVaults={showSubVaults}
          subVaultShown={subVaultShown}
          onToggleSubVaults={onToggleSubVaults}
          onToggleSubVault={onToggleSubVault}
          zIndex={CHILD_Z}
          onClose={close}
          onChanged={onChanged}
          onOpen={(sub) => { onClose(); onOpenSubVault(sub) }}
        />
      )}

      {open === 'kit' && (
        <RecoveryKit
          zIndex={CHILD_Z}
          onClose={() => { setOpen(null); loadKit() }}
        />
      )}

      {open === 'film' && (
        <FilmKeySettings
          zIndex={CHILD_Z}
          onClose={close}
          /* This menu's own line, kept honest without another round trip. */
          onChanged={setFilmKey}
        />
      )}

      {open === 'links' && (
        <DownloadLinks
          settings={links}
          zIndex={CHILD_Z}
          onClose={close}
          onChanged={setLinks}
        />
      )}

      {open === 'health' && (
        <CloudHealth
          health={health}
          zIndex={CHILD_Z}
          onClose={close}
          onChanged={onHealthChanged}
        />
      )}

      {open === 'mount' && (
        <MountDrive path={webdav?.path} zIndex={CHILD_Z} onClose={close} />
      )}

      {/* A sweep changes what the accounts are holding and a reattach changes
          the index, so both are `onChanged` rather than a quiet close. */}
      {open === 'strays' && (
        <StrayParts zIndex={CHILD_Z} onClose={close} onChanged={onChanged} />
      )}
    </Modal>
  )
}

/* A lifetime in hours, the way a person would say it. */
function describeHours(hours) {
  if (hours === 1) return '1 hour'
  if (hours === 24) return 'A day'
  if (hours % 24 === 0 && hours >= 48) return `${hours / 24} days`
  return `${hours} hours`
}

const LINK_CHOICES = [
  { hours: 1, label: '1 hour' },
  { hours: 3, label: '3 hours' },
  { hours: 12, label: '12 hours' },
  { hours: 24, label: 'A day' },
  { hours: 168, label: 'A week' },
]

/* How long a folder's download link stays good.

   The link is a bearer address to a folder in the clear: anyone holding it can
   fetch the archive until it expires, with no sign-in. That is what makes it
   useful — a download manager on the desktop with the disk, a browser on
   another device — and what makes its lifetime worth choosing. Three hours is
   the default. It slides forward while a download runs, and every link ends
   the moment the vault locks, whatever this says. */
function DownloadLinks({ settings, zIndex, onClose, onChanged }) {
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState(null)
  const [custom, setCustom] = useState('')

  const choose = async (hours) => {
    setSaving(true)
    setError(null)
    try {
      onChanged(await api.setLinkSettings(hours))
      setCustom('')
    } catch (err) {
      setError(err.message)
    } finally {
      setSaving(false)
    }
  }

  const hours = settings?.hours
  const min = settings?.min_hours ?? 1
  const max = settings?.max_hours ?? 168
  const customHours = Number(custom)
  const customOK = Number.isInteger(customHours) && customHours >= min && customHours <= max

  return (
    <Modal
      title="Download links"
      subtitle="How long a folder's download address stays good"
      onClose={onClose}
      width={480}
      zIndex={zIndex}
    >
      {error && <Banner tone="error">{error}</Banner>}

      <p style={{
        margin: '0 0 14px', fontFamily: FONT.sans, fontSize: '12.5px', lineHeight: 1.6,
        color: COLORS.textDim,
      }}>
        Downloading a folder as a zip hands you an address that carries its own
        key: anyone holding it can fetch the folder, in the clear, until it
        expires — no sign-in needed, which is what lets a download manager or
        another device take it. A link slides forward while a download runs,
        and every link ends the moment the vault locks. Shortening this also
        shortens the links already handed out.
      </p>

      <div style={{ display: 'flex', flexWrap: 'wrap', gap: '6px', marginBottom: '14px' }}>
        {LINK_CHOICES.map((choice) => (
          <Choice
            key={choice.hours}
            label={choice.label}
            on={hours === choice.hours}
            disabled={saving || !settings}
            onClick={() => choose(choice.hours)}
          />
        ))}
      </div>

      <div style={{ display: 'flex', gap: '8px', alignItems: 'flex-end' }}>
        <div style={{ flex: 1 }}>
          <Input
            label={`Or any number of hours (${min}–${max})`}
            type="number"
            min={min}
            max={max}
            value={custom}
            disabled={saving || !settings}
            placeholder={hours ? String(hours) : ''}
            onChange={(e) => setCustom(e.target.value)}
          />
        </div>
        <Button
          onClick={() => choose(customHours)}
          disabled={saving || !customOK}
          style={{ marginBottom: '12px' }}
        >Save</Button>
      </div>

      <p style={{
        margin: '4px 0 0', fontFamily: FONT.sans, fontSize: '11px', lineHeight: 1.6,
        color: COLORS.textMuted,
      }}>
        {settings
          ? `Links currently last ${describeHours(settings.hours).toLowerCase()}; the default is ${describeHours(settings.default_hours).toLowerCase()}.`
          : 'Reading the setting…'}
      </p>
    </Modal>
  )
}

/* What the cloud health line says on the right: the schedule, or what is wrong.

   The schedule most of the time, because that is the setting this line is for.
   The finding takes over when there is one, since a menu that shows "Hourly"
   beside a cloud that has been dark for two days is technically answering the
   question and practically misleading. */
function healthStatusLine(health) {
  if (!health) return null
  if (health.unhealthy > 0) return `${health.unhealthy} unhealthy`
  if (!health.schedule?.enabled) return 'Off'

  const minutes = health.schedule.interval_minutes || 60
  if (minutes < 60) return `Every ${minutes} min`
  if (minutes === 60) return 'Hourly'
  if (minutes % (24 * 60) === 0) {
    const days = minutes / (24 * 60)
    return days === 1 ? 'Daily' : `Every ${days} days`
  }
  return `Every ${Math.round(minutes / 60)} hours`
}

/* What the recovery kit's line says on the right.

   It reports the worst true thing rather than the newest: an account the kit
   has never heard of is a credential nobody can restore, and it outranks any
   number of files, which the index on the clouds brings back anyway. */
function kitStatusLine(kit) {
  if (!kit) return null
  if (!kit.exported) return 'None'
  if (kit.accounts_changed) return 'Missing an account'
  if (kit.password_changed_since) return 'Predates your password'
  if (kit.files_added > 0) return `${kit.files_added.toLocaleString()} files newer`
  if (kit.age_days > 0) return `${kit.age_days} days old`
  return 'Current'
}

function kitStatusTone(kit) {
  if (!kit) return undefined
  // A vault with nothing connected has nothing a kit could carry, so "None"
  // there is a fact rather than a warning.
  if (!kit.exported) return kit.accounts > 0 ? COLORS.warn : COLORS.textMuted
  if (kit.accounts_changed || kit.password_changed_since) return COLORS.warn
  if (kit.files_added > 0) return COLORS.textDim
  return COLORS.success
}

/* One line of the list: what it is, what it is for, and where it stands.

   The standing sits on the right rather than in the description, because half
   the reason to open this menu is to read one of them — "which clouds am I
   defaulting to", "did I ever set that key" — and a line with nothing to
   report leaves the column empty rather than filling it with a verb. */
function Setting({ icon, label, hint, status, tone, onClick }) {
  const [hover, setHover] = useState(false)

  return (
    <button
      type="button"
      onClick={onClick}
      onPointerEnter={(e) => { if (e.pointerType === 'mouse') setHover(true) }}
      onPointerLeave={() => setHover(false)}
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: '12px',
        width: '100%',
        // Past the fingertip floor the rest of the app holds to, since two
        // lines of text need the room anyway.
        minHeight: '56px',
        padding: '10px 12px',
        background: hover ? COLORS.surfaceHover : COLORS.surfaceRaised,
        border: `1px solid ${hover ? COLORS.borderBright : COLORS.border}`,
        borderRadius: '8px',
        cursor: 'pointer',
        textAlign: 'left',
        transition: 'background 0.15s ease, border-color 0.15s ease',
      }}
    >
      {/* Hidden from the accessibility tree on purpose: the label beside it
          already names the button, and a glyph read out as well would land in
          the button's spoken name. */}
      <span aria-hidden="true" style={{
        fontSize: '16px',
        lineHeight: 1,
        flexShrink: 0,
        opacity: hover ? 1 : 0.75,
        transition: 'opacity 0.15s ease',
      }}>{icon}</span>

      <span style={{ display: 'flex', flexDirection: 'column', gap: '3px', flex: 1, minWidth: 0 }}>
        <span style={{
          fontFamily: FONT.mono, fontSize: '12px', fontWeight: 600,
          letterSpacing: '0.5px', color: COLORS.text,
        }}>{label}</span>
        <span style={{
          fontFamily: FONT.sans, fontSize: '11px', lineHeight: 1.45, color: COLORS.textMuted,
        }}>{hint}</span>
      </span>

      <span style={{
        display: 'flex', alignItems: 'center', gap: '8px', flexShrink: 0,
        fontFamily: FONT.mono, fontSize: '10.5px', color: tone || COLORS.textDim,
      }}>
        {status}
        <span aria-hidden="true" style={{ color: COLORS.textMuted }}>›</span>
      </span>
    </button>
  )
}
