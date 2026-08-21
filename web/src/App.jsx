import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { COLORS, FONT, assignAccountColors, formatBytes } from './theme'
import { useIsMobile } from './hooks'
import { useNavigator } from './navigation'
import { api } from './api'
import LockScreen from './components/LockScreen'
import AccountsPanel from './components/AccountsPanel'
import FileBrowser from './components/FileBrowser'
import PreviewModal, { ShardInspector } from './components/PreviewModal'
import RecoverVault from './components/RecoverVault'
import CleanOrphans from './components/CleanOrphans'
import ReattachShards from './components/ReattachShards'
import FilmDetails from './components/FilmDetails'
import { Brand, DevMark } from './components/Brand'
import { Banner, Button } from './components/ui'
import { UnlockSubVault } from './components/SubVaults'

/* Where the sub vault view preference lives in this browser: the blanket
   answer, and the sub vaults that have been decided one at a time. Neither is
   sent anywhere — the server has no opinion about what a browser draws. */
const SHOW_SUB_VAULTS_KEY = 'sand.showSubVaults'
const SUB_VAULT_CHOICES_KEY = 'sand.subVaultsShown'

function loadSubVaultChoices() {
  try {
    const raw = JSON.parse(localStorage.getItem(SUB_VAULT_CHOICES_KEY) || '{}')
    if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return {}
    /* Anything that is not a plain yes or no is dropped rather than coerced —
       a half-read value deciding what is on screen is worse than the blanket
       setting deciding it. */
    return Object.fromEntries(Object.entries(raw).filter(([, shown]) => typeof shown === 'boolean'))
  } catch {
    return {}
  }
}

/* SAND — a file browser over storage you do not fully trust.
   Files are compressed, split into three encrypted parts and scattered across
   separate cloud accounts. Opening one gathers the parts back and rebuilds it. */
export default function App() {
  const [status, setStatus] = useState(null)
  /* Not a single path but the trail of folders walked through, so Back and
     Forward have somewhere to step. See navigation.js. */
  const nav = useNavigator()
  const path = nav.path
  const [listing, setListing] = useState(null)
  const [providers, setProviders] = useState([])
  const [loadingList, setLoadingList] = useState(false)
  const [loadingProviders, setLoadingProviders] = useState(false)
  const [error, setError] = useState(null)
  const [preview, setPreview] = useState(null)
  const [inspecting, setInspecting] = useState(null)
  const [filming, setFilming] = useState(null)
  const [accountsOpen, setAccountsOpen] = useState(false)
  const [recovery, setRecovery] = useState(null)
  const [recovering, setRecovering] = useState(false)
  const [orphans, setOrphans] = useState(null)
  const [sweeping, setSweeping] = useState(false)
  const [reattaching, setReattaching] = useState(false)
  /* Which set of connected clouds the tidy-up notice has already been shown
     for. Kept in state rather than in a ref because dismissing it has to redraw
     — and unlike the recovery scan's ref, this cannot re-run anything: the scan
     effect below does not read it. */
  const [orphansDismissed, setOrphansDismissed] = useState('')
  const [unlockingSub, setUnlockingSub] = useState(null)
  /* Which sub vaults show up in the file list. A view preference, kept in this
     browser: it decides what is drawn and nothing else. A locked sub vault
     stays locked whether it is drawn or not, and no setting puts one on a
     mounted drive.

     Two halves, because "show them" was never one answer for everybody: the
     sub vault you want in front of you at the root is rarely the same one you
     would rather nobody standing behind you saw named. `showSubVaults` is what
     a sub vault does when nothing has been said about it in particular — the
     answer for the ones you have not decided on, and for the next one you
     make. `subVaultChoices` holds the ones that have been decided one at a
     time, and beats it. */
  const [showSubVaults, setShowSubVaults] = useState(() => {
    try { return localStorage.getItem(SHOW_SUB_VAULTS_KEY) === '1' } catch { return false }
  })
  const [subVaultChoices, setSubVaultChoices] = useState(loadSubVaultChoices)

  const mobile = useIsMobile()
  const unlocked = !!status?.unlocked
  const subVaults = status?.sub_vaults || []

  /* An individual choice if one has been made, the blanket setting otherwise.
     A sub vault made after the fact has no entry here, so it follows whatever
     the blanket setting says rather than appearing where its siblings were
     deliberately taken out of. */
  const subVaultShown = useCallback((id) => (
    Object.prototype.hasOwnProperty.call(subVaultChoices, id) ? subVaultChoices[id] : showSubVaults
  ), [subVaultChoices, showSubVaults])

  const shownSubVaults = useMemo(
    () => subVaults.filter((sub) => subVaultShown(sub.id)),
    [subVaults, subVaultShown],
  )

  // A prompt that reappears every time the accounts are refreshed stops being a
  // prompt and starts being an obstacle. Held in a ref rather than in state so
  // that saying "not now" does not itself re-run the scan.
  const recoveryDismissed = useRef(false)

  // The recovery dialog connects clouds of its own, so the accounts it is
  // working through change underneath this component while it is open. It owns
  // its own copy of the scan from the moment it mounts; this ref is what stops
  // the scan out here being cleared from under it and unmounting it mid-flow.
  const recoveringRef = useRef(false)
  const openRecovery = (open) => { recoveringRef.current = open; setRecovering(open) }

  // Which account owns which colour can only be decided against the whole list,
  // and every pane below reads the answer back by id — so settle it here, in
  // the render that owns the list, before any of them draw a badge.
  assignAccountColors(providers)

  // On a phone the accounts panel is a drawer over the browser; once there is
  // room for both panes again it is simply always there, so drop the flag.
  useEffect(() => { if (!mobile) setAccountsOpen(false) }, [mobile])

  useEffect(() => {
    api.vaultStatus().then(setStatus).catch((err) => {
      setStatus({ initialized: false, unlocked: false })
      setError(err.message)
    })
  }, [])

  const refreshProviders = useCallback(async () => {
    setLoadingProviders(true)
    try {
      const resp = await api.providers()
      setProviders(resp.providers || [])
    } catch (err) {
      if (err.code === 'LOCKED') setStatus((s) => ({ ...s, unlocked: false }))
      else setError(err.message)
    } finally {
      setLoadingProviders(false)
    }
  }, [])

  /* Which listing the browser is actually showing. A folder listing is a
     round-trip, and walking into a sub vault changes where you are while one
     for somewhere else is still in flight — so an answer that arrives for
     somewhere you have left is dropped rather than drawn. Without this,
     opening a sub vault could paint the main vault's files under its name,
     which is worse than a slow listing by some distance. */
  const showing = useRef({ vault: '', path: '/' })
  useEffect(() => { showing.current = { vault: nav.vault, path } }, [nav.vault, path])

  const refreshListing = useCallback(async (target = path, inVault = nav.vault) => {
    setLoadingList(true)
    try {
      const resp = await api.list(target, inVault)
      if (showing.current.vault !== inVault || showing.current.path !== target) return
      setListing(resp)
      setError(null)
    } catch (err) {
      if (showing.current.vault !== inVault || showing.current.path !== target) return
      if (err.code === 'LOCKED') {
        setStatus((s) => ({ ...s, unlocked: false }))
      } else if (err.code === 'SUB_VAULT_LOCKED') {
        /* It was locked from another tab, or the session outlived it. Step
           back to the main vault rather than leaving the browser pointed at a
           tree it can no longer read — and drop the trail through it, since
           those folder names are that sub vault's index. */
        nav.leaveVault(inVault)
        setListing(null)
      } else if (err.code === 'NOT_FOUND' && target !== '/') {
        // The folder went away underneath us — fall back to the root. It
        // replaces the step rather than adding one, because a folder that is
        // no longer there is not somewhere Back should lead.
        nav.replace('/')
      } else {
        setError(err.message)
      }
    } finally {
      setLoadingList(false)
    }
  }, [path, nav])

  const refreshAll = useCallback(async () => {
    const fresh = await api.vaultStatus().catch(() => null)
    if (fresh) setStatus(fresh)
    await Promise.all([refreshListing(), refreshProviders()])
  }, [refreshListing, refreshProviders])

  useEffect(() => {
    if (!unlocked) return
    refreshListing(path, nav.vault)
  }, [unlocked, path, nav.vault, refreshListing])

  useEffect(() => {
    if (!unlocked) return
    refreshProviders()
  }, [unlocked, refreshProviders])

  /* Disaster recovery starts here, without being asked for.

     Someone whose machine died reinstalls SAND, makes a fresh vault and
     reconnects their clouds — and those clouds are still carrying the index of
     the vault that is gone. The app can see that and they cannot, so it looks
     as soon as there is an account to look at, and says so.

     On an empty vault, because that is the state a reinstalled machine is in and
     the only state a whole vault can be adopted into — and on a vault carrying
     shard records that point at accounts it is not connected to, which is what
     a recovery run before every cloud was back leaves behind. Any other vault is
     simply in use, and is not asked. */
  const providerKey = providers.map((p) => p.id).sort().join(',')
  const fileCount = status?.stats?.files ?? 0
  const unresolved = status?.stats?.unresolved ?? 0

  useEffect(() => {
    if (recoveringRef.current) return
    if (!unlocked || providerKey === '' || (fileCount > 0 && unresolved === 0)) {
      setRecovery(null)
      return
    }
    let cancelled = false
    api.recoveryScan().then((scan) => {
      if (cancelled) return
      setRecovery(scan)
      // Only the disaster interrupts. Resuming is offered from the banner
      // instead: it is not news — the vault has been in that state since the
      // recovery that made it — and a modal on every load would be a nag.
      if (scan.available && !recoveryDismissed.current) openRecovery(true)
    }).catch(() => {
      // An account that will not answer is the accounts panel's story to tell;
      // it should not put an error over an otherwise working vault.
    })
    return () => { cancelled = true }
  }, [unlocked, providerKey, fileCount, unresolved])

  /* And the opposite question, asked at the same moment.

     Recovery is about parts the index points at and cannot reach. This is about
     parts the index has stopped pointing at at all: the ones a delete could not
     erase because the cloud holding them was disconnected at the time. That
     cloud comes back as a new account, and nothing ever goes looking again —
     the room is simply gone. Both gaps open when the set of connected accounts
     changes, so both are looked for then.

     Never while a recovery is on the table: a vault waiting to be recovered has
     every part on its accounts unaccounted for, and the last thing that state
     needs is an offer to sweep them. The server refuses too (see orphanGuard);
     this is about not putting the question in front of somebody at the worst
     possible moment. */
  useEffect(() => {
    if (!unlocked || providerKey === '' || recovery?.available) {
      setOrphans(null)
      return
    }
    let cancelled = false
    api.orphanScan().then((scan) => {
      if (cancelled) return
      setOrphans(scan.found || scan.reattachable > 0 ? scan : null)
    }).catch(() => {
      // An account that will not answer is the accounts panel's story, and a
      // tidy-up nobody asked for has no business raising an error over it.
    })
    return () => { cancelled = true }
  }, [unlocked, providerKey, recovery?.available])

  const lock = async () => {
    await api.lock().catch(() => {})
    setStatus((s) => ({ ...s, unlocked: false, stats: null }))
    setListing(null)
    setProviders([])
    setPreview(null)
    setInspecting(null)
    setFilming(null)
    setAccountsOpen(false)
    setRecovery(null)
    openRecovery(false)
    recoveryDismissed.current = false
    setOrphans(null)
    setSweeping(false)
    setReattaching(false)
    setOrphansDismissed('')
    // The trail is a list of folder names, which is the file index — locking
    // the vault has to put that away with everything else.
    nav.reset()
  }

  /* Walking into a sub vault, from the strip at the root or from settings.

     A locked one asks for its password on the way in rather than refusing.
     Navigating first and letting the listing fail would bounce straight back
     to the main root, which from the outside is a click that did nothing. */
  const openSubVault = useCallback((sub) => {
    if (!sub?.id) return
    const known = subVaults.find((s) => s.id === sub.id) || sub
    /* Whoever is asking may have opened it a moment ago — the unlock dialog
       walks straight in once the password is accepted, and the fresh status
       behind that is still in flight, so the listed copy here still says
       locked. Their word that it is open beats this list, which is the older
       of the two: trusting the list instead puts the same dialog back up over
       the one that just closed, and from the outside that is a password the
       app ignored. Only the list saying so unprompted keeps it shut. */
    if (known.unlocked === false && sub.unlocked !== true) {
      setUnlockingSub(known)
      return
    }
    nav.navigate({ vault: sub.id, path: '/' })
  }, [nav, subVaults])

  /* The blanket tick answers for all of them, so the individual choices go
     with it. Leaving them underneath would mean a box that says "all of them"
     over a list where one is still crossed out. */
  const toggleSubVaults = useCallback((next) => {
    setShowSubVaults(next)
    setSubVaultChoices({})
    try {
      localStorage.setItem(SHOW_SUB_VAULTS_KEY, next ? '1' : '0')
      localStorage.removeItem(SUB_VAULT_CHOICES_KEY)
    } catch { /* private mode */ }
  }, [])

  const toggleSubVault = useCallback((id, next) => {
    /* Rebuilt from the sub vaults that exist rather than merely written over:
       a deleted one has no row left to take its choice off again, and its
       entry would otherwise sit in this browser for good. */
    const kept = {}
    for (const sub of subVaults) {
      if (sub.id === id) kept[sub.id] = next
      else if (Object.prototype.hasOwnProperty.call(subVaultChoices, sub.id)) kept[sub.id] = subVaultChoices[sub.id]
    }
    setSubVaultChoices(kept)
    try { localStorage.setItem(SUB_VAULT_CHOICES_KEY, JSON.stringify(kept)) } catch { /* private mode */ }
  }, [subVaults, subVaultChoices])

  if (!status) {
    return <Shell><div style={{ padding: '80px 24px', textAlign: 'center', color: COLORS.textMuted }}>Loading…</div></Shell>
  }

  if (!unlocked) {
    return (
      <Shell>
        <LockScreen
          status={status}
          onUnlocked={(next) => { setStatus(next); setError(null) }}
        />
      </Shell>
    )
  }

  return (
    <Shell>
      {/* dvh rather than vh: a phone's collapsing address bar counts towards
          100vh, which would leave the last row of files under the browser UI. */}
      <div style={{ height: 'var(--app-height)', display: 'flex', flexDirection: 'column' }}>
        <header style={{
          display: 'flex',
          alignItems: 'center',
          // Tighter on a phone than it looks like it wants to be: the three
          // header controls are now full 44px targets, and the room for them
          // comes out of the spacing between them.
          gap: mobile ? '6px' : '14px',
          padding: mobile ? '8px 10px' : '10px 20px',
          borderBottom: `1px solid ${COLORS.border}`,
          background: COLORS.surface,
          flexShrink: 0,
        }}>
          {mobile && (
            <Button
              size="sm"
              variant="ghost"
              aria-label="Connected clouds"
              data-icon-button="true"
              onClick={() => setAccountsOpen(true)}
              style={{ fontSize: '17px', padding: '4px 8px', minWidth: '44px', justifyContent: 'center' }}
            >☰</Button>
          )}

          <Brand />

          {/* The tagline is the first thing to go: on a phone it can only wrap
              into a column beside the wordmark. */}
          {!mobile && (
            <span style={{
              fontFamily: FONT.mono,
              fontSize: '10px',
              letterSpacing: '1.5px',
              textTransform: 'uppercase',
              color: COLORS.textMuted,
              marginTop: '4px',
            }}>
              {/* Two phrases, not one — the bullet is faded against the text
                  around it so it reads as a divider rather than a word of its
                  own. The spaces are written out because JSX drops whitespace
                  that wraps a line. */}
              {'Secure Archival '}
              <span style={{ opacity: 0.55 }}>•</span>
              {' Network Distribution'}
            </span>
          )}

          <span style={{ flex: 1 }} />

          {/* Narrow enough and the labels are dropped; the glyphs carry the
              meaning and the accessible name comes off aria-label. */}
          <Button size="sm" variant="ghost" onClick={refreshAll}
            data-icon-button={mobile || undefined}
            style={mobile ? { fontSize: '16px', padding: '4px 8px', minWidth: '44px', justifyContent: 'center' } : null}
            title="Refresh" aria-label="Refresh">⟳{mobile ? '' : ' Refresh'}</Button>
          <Button size="sm" onClick={lock}
            data-icon-button={mobile || undefined}
            style={mobile ? { fontSize: '15px', padding: '4px 8px', minWidth: '44px', justifyContent: 'center' } : null}
            title="Lock vault" aria-label="Lock vault">🔒{mobile ? '' : ' Lock vault'}</Button>
          {/* No room for the developer mark up here on a phone — it moves to
              the foot of the accounts drawer instead. */}
          {!mobile && <DevMark />}
        </header>

        {/* Dismissed, but not forgotten — and the standing offer for a recovery
            that has not finished, which no modal is going to nag about. */}
        {(recovery?.available || recovery?.resumable) && !recovering && (
          <div style={{ padding: mobile ? '10px 10px 0' : '12px 20px 0', flexShrink: 0 }}>
            <Banner tone="warn">
              <span style={{ display: 'flex', alignItems: 'center', gap: '10px', flexWrap: 'wrap' }}>
                <span>
                  {recoveryNotice(recovery)}
                </span>
                <Button size="sm" variant="ghost" onClick={() => openRecovery(true)}>
                  {recovery.available ? 'Attempt recovery' : 'Finish recovery'}
                </Button>
              </span>
            </Banner>
          </div>
        )}

        {/* Two different pieces of news out of one listing, and the repair
            leads — a file short of a spare part is worth more attention than
            room being wasted, and putting it right costs nothing at all. Both
            are dismissible for good: housekeeping notices, not warnings, raised
            again the next time the clouds change. */}
        {orphans && orphansDismissed !== providerKey && (
          <div style={{ padding: mobile ? '10px 10px 0' : '12px 20px 0', flexShrink: 0 }}>
            {orphans.reattachable > 0 && !reattaching && (
              <Banner tone="warn" onDismiss={() => setOrphansDismissed(providerKey)}>
                <span style={{ display: 'flex', alignItems: 'center', gap: '10px', flexWrap: 'wrap' }}>
                  <span>{reattachNotice(orphans)}</span>
                  <Button size="sm" variant="ghost" onClick={() => setReattaching(true)}>
                    Put them back
                  </Button>
                </span>
              </Banner>
            )}
            {orphans.found && !sweeping && (
              <Banner tone="info" onDismiss={() => setOrphansDismissed(providerKey)}>
                <span style={{ display: 'flex', alignItems: 'center', gap: '10px', flexWrap: 'wrap' }}>
                  <span>{orphanNotice(orphans)}</span>
                  <Button size="sm" variant="ghost" onClick={() => setSweeping(true)}>
                    Take a look
                  </Button>
                </span>
              </Banner>
            )}
          </div>
        )}

        <div style={{ flex: 1, display: 'flex', minHeight: 0, position: 'relative' }}>
          <AccountsPanel
            providers={providers}
            loading={loadingProviders}
            status={status}
            stats={status.stats}
            webdav={status.webdav}
            mobile={mobile}
            open={accountsOpen}
            subVaults={subVaults}
            showSubVaults={showSubVaults}
            subVaultShown={subVaultShown}
            onToggleSubVaults={toggleSubVaults}
            onToggleSubVault={toggleSubVault}
            onOpenSubVault={openSubVault}
            onClose={() => setAccountsOpen(false)}
            onRefresh={refreshProviders}
            onChanged={refreshAll}
          />

          <FileBrowser
            nav={nav}
            listing={listing}
            loading={loadingList}
            error={error}
            providers={providers}
            defaultAccounts={status.stats?.default_accounts || []}
            defaultScheme={status.stats?.default_scheme || ''}
            mobile={mobile}
            subVaults={subVaults}
            shownSubVaults={shownSubVaults}
            onOpenSubVault={openSubVault}
            onRefresh={refreshAll}
            onPreview={(file, hasThumb, film) => setPreview({ file, hasThumb, film })}
            onInspect={setInspecting}
            onFilm={setFilming}
            onError={setError}
          />
        </div>
      </div>

      {preview && (
        <PreviewModal
          file={preview.file}
          hasThumb={preview.hasThumb}
          film={preview.film}
          onClose={() => setPreview(null)}
          /* A file that had no picture in the list has one now. */
          onThumbStored={() => refreshListing()}
          /* And a match made from in there changes the same listing. */
          onFilmChanged={() => refreshListing()}
        />
      )}
      {filming && (
        <FilmDetails
          file={filming}
          onClose={() => setFilming(null)}
          /* A match changes the row's caption and its picture, both of which
             the listing carries. */
          onChanged={() => refreshListing()}
        />
      )}
      {recovering && recovery && (
        <RecoverVault
          scan={recovery}
          onClose={() => { openRecovery(false); recoveryDismissed.current = true }}
          onRecovered={() => {
            openRecovery(false)
            setRecovery(null)
            refreshAll()
          }}
          /* The dialog connects clouds of its own, so the accounts panel has
             to hear about them the moment they land rather than when it
             closes. */
          onAccountsChanged={refreshProviders}
        />
      )}
      {reattaching && orphans && (
        <ReattachShards
          scan={orphans}
          /* The rows go when the panel does, not when the write commits: the
             dialog stays up afterwards to say what it recorded, and clearing
             them underneath it would take that away mid-sentence. */
          onClose={() => {
            setReattaching(false)
            setOrphans(null)
            setOrphansDismissed(providerKey)
          }}
          /* The index changed, so the file list and every part badge in it are
             stale. */
          onDone={refreshAll}
        />
      )}
      {sweeping && orphans && (
        <CleanOrphans
          scan={orphans}
          /* The rows go when the panel does, not when the sweep commits — the
             dialog stays up afterwards to say what it erased, and clearing them
             underneath it would take that away mid-sentence. Closing it counts
             as having been told, so the banner does not come back until the
             clouds change. */
          onClose={() => {
            setSweeping(false)
            setOrphans(null)
            setOrphansDismissed(providerKey)
          }}
          /* What was erased is room on somebody's cloud, which the accounts
             panel draws. */
          onSwept={refreshProviders}
        />
      )}
      {unlockingSub && (
        <UnlockSubVault
          sub={unlockingSub}
          onClose={() => setUnlockingSub(null)}
          onUnlocked={() => {
            const sub = unlockingSub
            setUnlockingSub(null)
            refreshAll()
            nav.navigate({ vault: sub.id, path: '/' })
          }}
        />
      )}

      {inspecting && (
        <ShardInspector
          file={inspecting}
          providers={providers}
          onClose={() => setInspecting(null)}
          /* Moving the parts changes the badges the listing draws. */
          onChanged={refreshAll}
        />
      )}
    </Shell>
  )
}

/* What the banner says, which is two quite different pieces of news.

   The first is a discovery: this machine is new and those clouds are carrying a
   vault. The second is unfinished business — a recovery that ran while some
   accounts were still missing, where the distinction worth drawing is between
   files that cannot be opened at all and parts that were only ever the spare. */
function recoveryNotice(scan) {
  if (scan.available) {
    return `Sand files detected on your clouds — ${scan.parts} part${scan.parts === 1 ? '' : 's'} `
      + `(${formatBytes(scan.bytes)}) and an encrypted copy of a vault index this one did not write.`
  }
  if (scan.stranded > 0) {
    return `${scan.stranded} file${scan.stranded === 1 ? '' : 's'} in this vault cannot be opened: `
      + 'their parts are on clouds it is not connected to. Connect them to finish the recovery.'
  }
  return `${scan.unresolved} part${scan.unresolved === 1 ? '' : 's'} of your files sit on clouds `
    + 'this vault is not connected to. Your files still open, with nothing to spare.'
}

/* What the tidy-up banner says. Two figures and no more: how much room is
   being held and on how many clouds. What each abandoned archive used to be is
   not knowable — that lived in the index that stopped mentioning it — so the
   banner does not pretend otherwise, and the panel behind it says the same
   thing at more length. */
function orphanNotice(scan) {
  const clouds = scan.accounts.filter((account) => account.orphans > 0).length
  const where = clouds === 1 ? 'one of your clouds is' : `${clouds} of your clouds are`
  return `${formatBytes(scan.bytes)} across ${scan.objects} part${scan.objects === 1 ? '' : 's'} `
    + `on ${where} holding storage no file in this vault points at any more.`
}

/* What the repair banner says. It leads on the files rather than on the room,
   because that is the thing that got worse: they are short of a spare part and
   the spare is sitting on a cloud that is connected. */
function reattachNotice(scan) {
  const shards = scan.reattachable
  const files = scan.stray_files
  return `${shards} part${shards === 1 ? '' : 's'} of ${files} file${files === 1 ? '' : 's'} `
    + `${shards === 1 ? 'is' : 'are'} on your clouds with nothing pointing at ${shards === 1 ? 'it' : 'them'} `
    + '— a disconnected cloud takes its records with it. Putting them back moves no data.'
}

function Shell({ children }) {
  return (
    <div style={{
      minHeight: '100vh',
      background: COLORS.bg,
      color: COLORS.text,
      fontFamily: FONT.sans,
    }}>
      <style>{`
        @keyframes sand-spin { to { transform: rotate(360deg); } }
        @keyframes sand-dev-veil {
          0% { opacity: 0; }
          7% { opacity: 1; }
          82% { opacity: 1; }
          100% { opacity: 0; }
        }
        /* Lands with a small overshoot, then drifts out as the veil clears. */
        @keyframes sand-dev-badge {
          0% { transform: scale(0.82); }
          14% { transform: scale(1.02); }
          22% { transform: scale(1); }
          82% { transform: scale(1); }
          100% { transform: scale(1.06); }
        }
        /* Row menus rise from the edge they are anchored to, so it is obvious
           they belong to the thumb rather than to the middle of the screen. */
        @keyframes sand-sheet-up {
          from { transform: translateY(14px); opacity: 0; }
          to { transform: translateY(0); opacity: 1; }
        }
        .sand-sheet { animation: sand-sheet-up 0.18s ease-out; }
        /* The cross-fade stays — it isn't motion — but the scale doesn't. */
        @media (prefers-reduced-motion: reduce) {
          .sand-dev-lockup { animation: none !important; }
          .sand-sheet { animation: none !important; }
        }
        * { box-sizing: border-box; }
        :root {
          color-scheme: dark;
          /* The visible height of the viewport. Mobile browsers include their
             collapsing address bar in 100vh, so anything sized to fill the
             screen ends up taller than the screen; dvh tracks what is actually
             on show. Kept behind a variable so the vh fallback survives on
             browsers that never learned the unit. */
          --app-height: 100vh;
        }
        @supports (height: 100dvh) { :root { --app-height: 100dvh; } }
        html { -webkit-text-size-adjust: 100%; }
        /* WebKit draws its own clear button inside a search field, which would
           sit on top of the app's — and only appears in that one browser. */
        input[type="search"]::-webkit-search-cancel-button,
        input[type="search"]::-webkit-search-decoration { -webkit-appearance: none; }
        body { margin: 0; background: ${COLORS.bg}; }
        /* iOS zooms the whole page in when it focuses a field smaller than
           16px. Every input here is styled inline, so this is one of the few
           places that has to shout to win. */
        @media (max-width: 860px) {
          input, textarea, select { font-size: 16px !important; }
        }
        /* A fingertip is a far blunter instrument than a mouse pointer, so
           give every control a real target on touch screens: 44px is the size
           both Apple and Google publish as the smallest one worth aiming at.
           Glyph-only controls need the width as much as the height — an arrow
           is barely a dozen pixels across on its own. */
        @media (pointer: coarse) {
          button,
          a[href],
          input:not([type="radio"]):not([type="checkbox"]) { min-height: 44px; }
          [data-icon-button] { min-width: 44px; }
        }
        /* Stops the 300ms wait for a possible second tap, which otherwise
           reads as the app being slow to answer. */
        button, a[href], [role="button"] { touch-action: manipulation; }
        ::-webkit-scrollbar { width: 10px; height: 10px; }
        ::-webkit-scrollbar-track { background: ${COLORS.bg}; }
        ::-webkit-scrollbar-thumb { background: ${COLORS.border}; border-radius: 5px; }
        ::-webkit-scrollbar-thumb:hover { background: ${COLORS.borderBright}; }
      `}</style>
      {children}
    </div>
  )
}
