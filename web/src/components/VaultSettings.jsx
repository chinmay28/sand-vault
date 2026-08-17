import React, { useEffect, useState } from 'react'
import { COLORS, FONT } from '../theme'
import { api } from '../api'
import { Modal } from './ui'
import ChangePassword from './ChangePassword'
import MountDrive from './MountDrive'
import { FilmKeySettings } from './FilmDetails'
import SubVaults from './SubVaults'
import { DefaultClouds, PARTS_PER_FILE, schemeFor, schemeName } from './CloudSelect'

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
   and stays a banner. A menu that mixes those in becomes a second home screen. */

/* The dialogs this opens are opened over it rather than instead of it, so
   closing one puts you back on the list you chose it from. */
const CHILD_Z = 110

export default function VaultSettings({
  providers, stats, webdav, subVaults = [], showSubVaults, onToggleSubVaults,
  onOpenSubVault, onClose, onChanged,
}) {
  const [open, setOpen] = useState(null)
  const [filmKey, setFilmKey] = useState(null)

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
            status={defaults.length > 0
              ? `${defaults.length} of ${providers.length}${
                defaults.length > PARTS_PER_FILE ? ` · ${schemeName(schemeFor(defaults.length))}` : ''}`
              : `${PARTS_PER_FILE} per upload`}
            onClick={() => setOpen('defaults')}
          />
        )}

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
      </div>

      {open === 'defaults' && (
        <DefaultClouds
          providers={providers}
          defaults={stats?.default_accounts || []}
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
          onToggleSubVaults={onToggleSubVaults}
          zIndex={CHILD_Z}
          onClose={close}
          onChanged={onChanged}
          onOpen={(sub) => { onClose(); onOpenSubVault(sub) }}
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

      {open === 'mount' && (
        <MountDrive path={webdav?.path} zIndex={CHILD_Z} onClose={close} />
      )}
    </Modal>
  )
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
