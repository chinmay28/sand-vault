/* Thin wrapper over the SAND HTTP API.
   Every call goes to the local server, which is the only thing that ever sees
   plaintext — the browser never touches encryption keys. */

class ApiError extends Error {
  constructor(message, code, status) {
    super(message)
    this.code = code
    this.status = status
  }
}

async function request(path, { method = 'GET', body, formData, signal } = {}) {
  const init = { method, signal, credentials: 'same-origin', headers: {} }

  if (formData) {
    init.body = formData
  } else if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json'
    init.body = JSON.stringify(body)
  }

  const resp = await fetch(path, init)
  const contentType = resp.headers.get('Content-Type') || ''

  if (!contentType.includes('application/json')) {
    if (!resp.ok) throw new ApiError(`${resp.status} ${resp.statusText}`, 'HTTP_ERROR', resp.status)
    return null
  }

  const payload = await resp.json()
  if (!resp.ok) {
    throw new ApiError(payload.error || 'request failed', payload.code, resp.status)
  }
  return payload
}

export const api = {
  ApiError,

  vaultStatus: () => request('/api/vault'),
  initVault: (password, policy) => request('/api/vault/init', { method: 'POST', body: { password, policy } }),
  unlock: (password) => request('/api/vault/unlock', { method: 'POST', body: { password } }),
  lock: () => request('/api/vault/lock', { method: 'POST' }),
  setPolicy: (policy) => request('/api/vault/policy', { method: 'POST', body: { policy } }),
  /* The accounts an upload spreads over unless it names its own, and the code
     they are cut with. An empty list clears the default, which puts every
     upload back to picking three clouds at random.

     Both halves go at once, because neither is checkable alone: a scheme is
     only a default while accounts as wide as it are named under it. Passing no
     scheme clears that half and hands the code back to the count. */
  setDefaultAccounts: (accounts, scheme = '') =>
    request('/api/vault/defaults', { method: 'POST', body: { accounts, scheme } }),
  /* Changing the password rotates the key the stored parts are encrypted
     under, so unless the migration is deferred this call only comes back once
     every file has been rebuilt onto the new key — minutes, on a full vault.
     Deferring leaves the files readable and finishable with migrate(). */
  changePassword: (oldPassword, newPassword, { migrate = true } = {}) =>
    request('/api/vault/password', {
      method: 'POST',
      body: { old_password: oldPassword, new_password: newPassword, migrate },
    }),
  migrate: () => request('/api/vault/migrate', { method: 'POST' }),

  /* Disaster recovery. Every connected account carries an encrypted copy of
     the index, so a machine that lost its vault can find one sitting on the
     clouds it reconnects. The scan costs a listing and one small download per
     account and needs no password — it only reports what is there, and whether
     it belongs to a vault other than this one. */
  recoveryScan: () => request('/api/vault/recovery'),
  /* Rebuilds the index from that copy. The password is the *lost* vault's, not
     this one's: the backup is still sealed under whatever password wrote it.
     Ask with dryRun first — it reports exactly what would come back and what
     would not, without touching the vault. */
  recover: ({ providerId, password, dryRun = false } = {}) =>
    request('/api/vault/recovery', {
      method: 'POST',
      body: { provider_id: providerId || '', password, dry_run: dryRun },
    }),
  /* Finishes a recovery that ran before every account was reconnected. It asks
     the accounts what they hold and re-points the index at whichever one
     answers, which needs no password at all — the key was adopted by the
     recovery that ran first, and what was missing is a reachable copy of the
     parts rather than a secret. */
  resumeRecovery: ({ dryRun = false } = {}) =>
    request('/api/vault/recovery/resume', { method: 'POST', body: { dry_run: dryRun } }),
  /* Takes recovered files off the key they came back on. A recovery adopts the
     lost vault's data key — it is the only thing that opens the parts already
     on the accounts — which leaves the old password able to open them too,
     through any copy of the old index backup. This mints a fresh key under the
     current password and rebuilds every file onto it, erasing the old parts.
     `accounts` is where they should land; empty leaves each file where it is.

     A download and an upload of the whole vault, so it holds the connection for
     a long time. Nothing is unreadable while it runs and stopping is safe —
     whatever moved stays moved, and migrate() finishes the rest. */
  reclaim: (accounts = []) =>
    request('/api/vault/reclaim', { method: 'POST', body: { accounts } }),

  /* Parts on the accounts that nothing in the index points at any more.

     They pile up in one particular way. Erasing a file erases its parts from
     the accounts holding them — the ones that are connected at the time. A
     cloud that is disconnected while files are deleted keeps its share of them,
     and reconnecting it gives that account a brand new internal ID, so nothing
     ever goes back for what it kept. It is unreadable, unreferenced and still
     counting against the quota.

     The scan costs a listing and one small download per account — the same as
     recoveryScan() — which is why the browser asks when the set of connected
     accounts changes rather than on every render. Nothing is written by it. */
  orphanScan: () => request('/api/vault/orphans'),
  /* Erases them. `targets` are the {provider_id, archive_id} rows that were
     ticked; an empty list means everything the scan called deletable.

     The server re-scans before it deletes, so a target that has stopped being
     abandoned in the meantime is skipped and reported rather than erased. Ask
     with dryRun first: what it promises is measured against the accounts as
     they are now, not as the scan left them. */
  sweepOrphans: ({ targets = [], dryRun = false } = {}) =>
    request('/api/vault/orphans', { method: 'POST', body: { targets, dry_run: dryRun } }),
  /* The other half of the same listing, and the opposite of a sweep.

     A part with no record is not always rubbish. Disconnecting a cloud drops
     the index records naming it — an index that still claimed them would be
     lying about what can be retrieved — while the objects stay where they are.
     Reconnect and the two never meet again on their own: the account arrives
     with a new id, and resumeRecovery() re-points records rather than inventing
     them. The file goes on saying it is missing a spare part while the part
     sits on a cloud you are connected to.

     This records them again. Not a byte moves — a part's object key is derived
     from the archive id and the shard number, so the object is already exactly
     where the record says it is. Purely additive: nothing is erased, and a file
     can only come out of it with more shards than it went in with. */
  reattachShards: ({ dryRun = false } = {}) =>
    request('/api/vault/orphans/reattach', { method: 'POST', body: { dry_run: dryRun } }),
  /* The third thing the same scan turns up, and the only one that is not about
     a cloud at all.

     SAND writes its working files into the folder its vault file lives in —
     /var/lib/sand on a server, ~/.sand on a desktop — and the big one is the
     upload spool. A stream cannot say its own hash until its last byte, and
     every chunk has to carry that hash, so an upload arriving over the network
     is written to disk in full before any of it is sent. That file is removed
     on every way out of an upload except the one that is not a way out at all:
     the process being killed, or the machine losing power. What is left is the
     whole file that was being uploaded, sitting there for nobody.

     orphanScan() reports them under `leftovers`; this erases the ones named,
     or all of the ones it considers finished with when nothing is named. The
     server re-scans first, so a spool something has started writing to since
     the scan is skipped rather than pulled out from under it. */
  sweepLeftovers: ({ names = [], dryRun = false } = {}) =>
    request('/api/vault/orphans/leftovers', { method: 'POST', body: { names, dry_run: dryRun } }),
  /* The recovery kit: one sealed file that reconnects every cloud on a fresh
     install rather than only rebuilding the index.

     The difference from recover() above is the credentials. A copy of the index
     sits on every account, so it cannot carry them — one compromised account
     would unlock the rest — which leaves somebody reinstalling with an
     afternoon of signing back in before the password they still remember does
     anything. A kit never touches a cloud, so it can. */
  kitStatus: () => request('/api/vault/kit'),
  /* Builds a kit and hands back the archive with the code that opens it.

     Not request(), because the body is a zip and the code rides in a header:
     it must not be in the archive, in its filename, or in anything that could
     end up in a log or a browser history. */
  exportKit: async ({ useVaultPassword = false, password = '' } = {}) => {
    const resp = await fetch('/api/vault/kit', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ use_vault_password: useVaultPassword, password }),
    })
    if (!resp.ok) {
      const payload = await resp.json().catch(() => ({}))
      throw new ApiError(payload.error || 'the kit could not be built', payload.code, resp.status)
    }
    return {
      blob: await resp.blob(),
      kitId: resp.headers.get('X-Sand-Kit-Id') || '',
      secret: resp.headers.get('X-Sand-Kit-Secret') || 'code',
      code: resp.headers.get('X-Sand-Kit-Code') || '',
      sha256: resp.headers.get('X-Sand-Kit-Sha256') || '',
    }
  },
  /* The code a kit was sealed under, for somebody who still has their working
     vault and has mislaid the slip of paper. It gives nothing away that an
     unlocked vault does not already give: anybody who can ask this could
     export a fresh kit with a fresh code instead. */
  kitCode: (kitId) => request(`/api/vault/kit/code/${encodeURIComponent(kitId)}`),
  forgetKitCode: (kitId) =>
    request(`/api/vault/kit/code/${encodeURIComponent(kitId)}`, { method: 'DELETE' }),
  /* What a kit is, without opening it — which is what lets the import screen
     label its field "Recovery code" or "Vault password" rather than guessing.
     Needs no secret and no vault. */
  inspectKit: (file) => {
    const form = new FormData()
    form.append('kit', file)
    return request('/api/vault/kit/inspect', { method: 'POST', formData: form })
  },
  /* Rebuilds this machine from a kit and signs in. `password` is what the
     recovered vault will use from now on and need not be the one the lost
     vault used; `secret` is the kit's own.

     An account that will not connect never stops this: it comes back in the
     report as a button rather than as an error. */
  importKit: ({ file, secret, password, replace = false, oldPassword = '', skipCloudIndex = false }) => {
    const form = new FormData()
    form.append('kit', file)
    form.append('secret', secret)
    form.append('password', password)
    if (replace) form.append('replace', 'true')
    if (oldPassword) form.append('old_password', oldPassword)
    if (skipCloudIndex) form.append('skip_cloud_index', 'true')
    return request('/api/vault/kit/import', { method: 'POST', formData: form })
  },
  /* The fire drill: every credential in the kit pinged, its index checked
     against what the accounts really hold, and nothing written anywhere. It
     asks for the code on purpose — the failure nothing else catches is the
     slip of paper that went missing. */
  verifyKit: ({ file, secret }) => {
    const form = new FormData()
    form.append('kit', file)
    form.append('secret', secret)
    return request('/api/vault/kit/verify', { method: 'POST', formData: form })
  },

  providerSpecs: () => request('/api/providers/specs'),
  providers: () => request('/api/providers'),
  addProvider: (kind, name, options) =>
    request('/api/providers', { method: 'POST', body: { kind, name, options } }),
  testProvider: (id) => request(`/api/providers/${encodeURIComponent(id)}/test`, { method: 'POST' }),
  /* One account taken apart: its quota against what SAND actually put there,
     what the parts belong to, and how many files could not be rebuilt without
     it. Re-pings the account on the way, so it takes as long as a Test does.

     The id trails the word rather than leading it because the router will not
     take `{id}/stats` — it collides with the sign-in status route above. */
  providerStats: (id) => request(`/api/providers/stats/${encodeURIComponent(id)}`),
  /* Which account has been winning the race every read runs, over today, this
     month, this year, or all of it. Counters the read path already keeps, so
     this is a loopback request against numbers in memory — nothing is asked of
     any cloud, which is what makes it safe for the panel to poll while it is
     open.

     Forgetting is the one destructive thing in that panel: it erases the
     history on disk as well as in memory. It answers with the board it just
     cleared, for whichever window is being looked at. */
  readStats: (window = 'today') => request(`/api/reads?window=${encodeURIComponent(window)}`),
  forgetReadStats: (window = 'today') =>
    request(`/api/reads/forget?window=${encodeURIComponent(window)}`, { method: 'POST' }),
  /* What is actually in a bucket, counted by listing it end to end.

     The one figure in the panel that costs a walk of somebody else's storage —
     a request per thousand objects, billed as a transaction at the providers
     that charge for listing — so it is asked for rather than taken, and only
     accounts flagged `measurable` have anything to count. The server keeps what
     it counted, so the account's card shows the same figure afterwards without
     paying for it again. */
  measureProvider: (id) =>
    request(`/api/providers/${encodeURIComponent(id)}/measure`, { method: 'POST' }),
  /* What an account is called, what colour it wears, how big its holder says it
     is, how much of it SAND may fill, and how it reaches the backend. Only the
     fields passed change — an absent one is left alone. A color of '' hands the
     choice back to the browser, a capacity of '' is nobody declaring one, and a
     quota of '' is nobody watching this account's share.

     The first four never leave the process: nothing is uploaded, downloaded or
     re-encrypted by renaming a cloud or by drawing a line through it. `options` is the exception and the only
     part of this that touches the account — rotated keys, a re-pasted token, a
     moved bucket — so the server connects with them before storing them, and a
     PATCH carrying them takes as long as a Test does and fails the same way.

     Only the settings named are changed, and a secret handed back as the
     placeholder the server showed means "keep the one you have": the browser is
     never given a stored secret to send back. */
  updateProvider: (id, { name, color, capacity, quota, options } = {}) =>
    request(`/api/providers/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      body: {
        ...(name === undefined ? null : { name }),
        ...(color === undefined ? null : { color }),
        ...(capacity === undefined ? null : { capacity }),
        ...(quota === undefined ? null : { quota }),
        ...(options === undefined ? null : { options }),
      },
    }),

  /* Browser sign-in. The server holds the tokens; all the app ever sees is
     where to send the user and how far along the flow is. */
  oauthStart: (kind, { clientId = '', clientSecret = '', redirectUri = '', providerId = '' } = {}) =>
    request('/api/providers/oauth/start', {
      method: 'POST',
      body: {
        kind,
        client_id: clientId,
        client_secret: clientSecret,
        redirect_uri: redirectUri,
        // Naming an account makes this a reauthorization: the backend and the
        // OAuth app both come from the account itself, which is the only way
        // the app's secret can be reused — the browser was never given it.
        provider_id: providerId,
      },
    }),
  oauthStatus: (flowId) => request(`/api/providers/oauth/${encodeURIComponent(flowId)}`),
  /* A sign-in that cannot send the browser anywhere. Proton's client prints a
     link and waits, so this returns a flow to poll and the link turns up on a
     later poll rather than here — it does not exist until the client has been
     run. Everything after that is the OAuth path: same status, same complete,
     same reauthorize. */
  protonSignIn: ({ options = {}, providerId = '' } = {}) =>
    request('/api/providers/proton/signin', {
      method: 'POST',
      body: { options, provider_id: providerId },
    }),
  oauthExchange: (flowId, url) =>
    request('/api/providers/oauth/exchange', { method: 'POST', body: { flow_id: flowId, url } }),
  oauthComplete: (flowId, name, options) =>
    request('/api/providers/oauth/complete', {
      method: 'POST',
      body: { flow_id: flowId, name, options },
    }),
  /* The same finished sign-in, spent on an account that is already connected.
     New credentials under the same ID: the account keeps its name, its colour
     and every part it holds, and the index goes on pointing at it. */
  oauthReauthorize: (flowId, providerId) =>
    request('/api/providers/oauth/reauthorize', {
      method: 'POST',
      body: { flow_id: flowId, provider_id: providerId },
    }),
  /* Folders on the machine SAND runs on, for the backends configured with a
     path. Answers with folder names only — the vault's own files are the other
     endpoints. */
  systemFolders: (path = '') =>
    request(`/api/system/folders?path=${encodeURIComponent(path)}`),

  removeProvider: (id, force) =>
    request(`/api/providers/${encodeURIComponent(id)}${force ? '?force=1' : ''}`, { method: 'DELETE' }),

  /* Every call that names a path also names which vault the path is in. The
     empty string is the main vault, which is what every one of these meant
     before sub vaults existed and still means. Calls that name a file by its
     id take no vault at all — an id is unique across all of them, so the
     server resolves it against whatever is open. */
  list: (path, vault = '') =>
    request(`/api/files?path=${encodeURIComponent(path)}${vaultParam(vault)}`),
  /* Only the server can answer this: the file index is encrypted everywhere
     else, so there is nothing on any cloud account to ask. */
  search: (query, { path = '/', type, limit, vault = '', signal } = {}) => {
    const params = new URLSearchParams({ q: query })
    if (vault) params.set('vault', vault)
    if (path && path !== '/') params.set('path', path)
    if (type) params.set('type', type)
    if (limit) params.set('limit', String(limit))
    return request(`/api/search?${params.toString()}`, { signal })
  },
  fileMeta: (id) => request(`/api/files/${encodeURIComponent(id)}`),

  /* The files behind the "missing a spare part" line in the accounts panel:
     every one whose index records fewer parts than its own scheme calls for.

     Worst first — a file down to its last usable part leads, whatever it is
     called — and paged, because what leaves files short is an account refusing
     for an afternoon rather than one file going wrong: a vault can come back
     from a bad day with thousands of them. The answer carries the whole list's
     count and weight alongside the page, so a dialog showing twenty-five of
     four hundred can say so.

     Read out of the index with no account contacted, which is what makes it
     cheap to ask again after every repair. The repair itself is relocate()
     above — a short file re-spread over clouds that are answering comes back
     whole. */
  degradedFiles: ({ offset = 0, limit = 0 } = {}) => {
    const params = new URLSearchParams()
    if (offset) params.set('offset', String(offset))
    if (limit) params.set('limit', String(limit))
    const query = params.toString()
    return request(`/api/degraded${query ? `?${query}` : ''}`)
  },

  /* Files still stored in the format SAND used before chunking. Such a file
     cannot be read at an offset — the whole thing would have to be rebuilt to
     answer for any of it — so nothing streams one until it has been converted. */
  pendingConversions: () => request('/api/conversions'),
  /* Long: a download and a re-upload of the whole file. The dialog says so
     before it starts, and holds the connection for the duration. */
  convertFile: (id) => request(`/api/files/${encodeURIComponent(id)}/convert`, { method: 'POST' }),
  fileHealth: (id) => request(`/api/files/${encodeURIComponent(id)}/health`),
  deleteFile: (id) => request(`/api/files/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  /* Move a file into another folder, rename it, or both. An empty `dir` or
     `name` leaves that half alone. Nothing is uploaded or downloaded: a file's
     folder is a field in the index, and its parts are named after the file
     rather than after where it sits, so they never move. */
  moveFile: (id, dir, name) =>
    request(`/api/files/${encodeURIComponent(id)}/move`, { method: 'POST', body: { dir, name } }),

  /* Move a file, or a folder and everything under it, onto other clouds.
     Only the parts that are not already on one of the chosen accounts are
     copied — swapping one cloud out of three moves one part, not the file — so
     `preview` is worth asking first: it answers out of the index alone, without
     contacting a single account, and says exactly how much would travel. */
  relocate: ({ id, path, accounts, scheme = '', preview = false, vault = '', signal } = {}) =>
    request('/api/relocate', {
      method: 'POST',
      body: { id, path, accounts, scheme, preview, vault },
      signal,
    }),

  createFolder: (path, vault = '') =>
    request('/api/folders', { method: 'POST', body: { path, vault } }),
  deleteFolder: (path, recursive, vault = '') =>
    request(`/api/folders?path=${encodeURIComponent(path)}${recursive ? '&recursive=1' : ''}${vaultParam(vault)}`,
      { method: 'DELETE' }),

  /* Every folder in the vault, root first — the whole tree in one answer,
     which is what picking a destination needs. Folder paths and nothing else:
     no file names, no sizes, no placements. */
  folders: (vault = '') => request(`/api/folders${vault ? `?vault=${encodeURIComponent(vault)}` : ''}`),
  /* Move a folder, and everything under it, somewhere else in the vault. Like
     moving a file this is an index change and nothing more — no part leaves the
     account it is on — and the whole subtree changes in one write, so there is
     never a moment where half of it answers to the old name. */
  moveFolder: (from, to, vault = '') =>
    request('/api/folders/move', { method: 'POST', body: { from, to, vault } }),

  /* Everything under a folder, in one walk of the index: every file at or
     below it with its kind, its size and how deep it sits, and every folder
     below it with what that folder is holding.

     This is what the organizer plans from. It reads and nothing else — the
     tidying itself runs over moveFile, deleteFile and deleteFolder above, one
     item at a time from here, so a run that stalls halfway has moved exactly
     what its progress said it had and the rest is untouched. No account is
     contacted: the index is already open on the server, and this is a walk of
     it. */
  survey: (path, vault = '') =>
    request(`/api/folders/survey?path=${encodeURIComponent(path)}${vaultParam(vault)}`),

  /* What a folder is holding, in the few figures its menu can show: the size
     of everything at or below it, how many files and folders that is, what the
     parts of those files weigh across the accounts, and which accounts they
     are. Asked when the menu opens rather than carried on every folder row —
     it is one walk of the index per folder, and a listing of forty of them
     would be forty walks nobody asked for. */
  folderStats: (path, vault = '') =>
    request(`/api/folders/stats?path=${encodeURIComponent(path)}${vaultParam(vault)}`),

  /* Which files under a folder are copies of each other, asked three ways from
     one walk of the index: the same bytes (one SHA-256, which is proof), the
     same length (which is a question), and names alike enough to be copies of
     each other (which is a guess, and says so).

     All three come back together because switching between them is the whole
     of using it — a pair that is only a size match is worth a second look, and
     finding that out should not cost another walk. Each group arrives with the
     copy it suggests keeping first and marked, and with what clearing the rest
     would free. It reads and nothing else: erasing a copy is deleteFile above,
     one file at a time, and picking them instead is the selection bar. */
  duplicates: (path, vault = '') =>
    request(`/api/folders/duplicates?path=${encodeURIComponent(path)}${vaultParam(vault)}`),

  /* The standing instruction a folder has been given: on this schedule, check
     that every cloud is answering and every part of every file under it is
     where the index says it went — and, if it says so, put back what is
     missing.

     A folder's own policy rides along with its listing already, minus the
     history; this is the full record, which is what the dialog shows. The
     no-argument form lists every policy the vault can see and says whether a
     sweep is running right now. */
  automations: () => request('/api/automation'),
  automation: (path, vault = '') =>
    request(`/api/automation?path=${encodeURIComponent(path)}${vaultParam(vault)}`),
  /* One call for creating and editing, because a folder has at most one policy.
     Editing keeps the history and the last-run time, so changing the hour does
     not make the folder immediately due. */
  setAutomation: (policy) => request('/api/automation', { method: 'POST', body: policy }),
  removeAutomation: (path, vault = '') =>
    request(`/api/automation?path=${encodeURIComponent(path)}${vaultParam(vault)}`,
      { method: 'DELETE' }),
  /* Run it now, whether or not it is due and whether or not it is switched on.
     This one contacts every account and can rebuild files, so it is the only
     call here that takes real time — minutes on a large folder, longer if it
     is putting parts back. */
  runAutomation: (path, vault = '', { signal } = {}) =>
    request('/api/automation/run', { method: 'POST', body: { path, vault }, signal }),

  /* The repositories a vault is keeping a copy of, each stored as one git
     bundle. Listing is index work and answers at once; tracking and refreshing
     borrow the machine's git and talk to somebody else's server, so both can
     take real time — the first copy of a repository is its whole history. */
  repos: (path = '/', vault = '') =>
    request(`/api/git?path=${encodeURIComponent(path)}${vaultParam(vault)}`),
  trackRepo: (body, { signal } = {}) =>
    request('/api/git/track', { method: 'POST', body, signal }),
  refreshRepo: (id, vault = '', { signal } = {}) =>
    request(`/api/git/${encodeURIComponent(id)}/refresh${vaultParam(vault, true)}`,
      { method: 'POST', signal }),
  untrackRepo: (id, vault = '') =>
    request(`/api/git/${encodeURIComponent(id)}${vaultParam(vault, true)}`, { method: 'DELETE' }),

  /* The machines a vault imports files *from*, which is the other direction
     from a connected account and deliberately not one: an account holds
     encrypted parts under names SAND invents, a source holds your own files
     under paths you browse, and nothing is ever written to it.

     Listing is index work. The rest talk to somebody else's machine: connecting
     is a handshake and a directory listing, browsing is a round trip a click,
     and importing can be a media library coming down a home connection and
     going back up to three clouds. Only the last one takes real time. */
  /* A key pair for the two things here that sign in over SSH: a machine files
     are imported from, and a connected account on a machine you have a login
     on.

     Note which half comes back. The public one — a line to paste into
     authorized_keys, and not a secret — plus a handle standing in for the
     private one, which stays on the server and is swapped back in when the
     connect form is submitted. The browser never sees a private key it did not
     already have, which is the whole reason this endpoint exists rather than
     the form telling you to go and run ssh-keygen. */
  generateSshKey: (comment = '') =>
    request('/api/ssh/keypair', { method: 'POST', body: { comment } }),

  sources: () => request('/api/remote'),
  connectSource: (body, { signal } = {}) =>
    request('/api/remote', { method: 'POST', body, signal }),
  updateSource: (id, body, { signal } = {}) =>
    request(`/api/remote/${encodeURIComponent(id)}`, { method: 'PATCH', body, signal }),
  forgetSource: (id) =>
    request(`/api/remote/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  /* One directory on a source. The path is relative to the folder the source
     is scoped to — never an absolute path on somebody's server, in either
     direction — and everything outside it is refused however it is written,
     symlinks included. */
  browseSource: (id, path = '', { signal } = {}) =>
    request(`/api/remote/${encodeURIComponent(id)}/files?path=${encodeURIComponent(path)}`, { signal }),
  /* Pull a selection into a folder of the vault. A folder in the selection
     brings everything under it, keeping its shape.

     Interrupting this loses no whole file: every file that arrived is
     committed, and running the same import again skips those and carries on.
     That is the whole of the resume story — there is no job to resume, only an
     import to repeat. A file cut off partway is fetched again from the start.

     `detach` answers 202 with the run to watch instead of holding the request
     open for the result: the import carries on with no page behind it, and what
     it came to arrives on sourceImports below. */
  importFromSource: (id, body, { signal } = {}) =>
    request(`/api/remote/${encodeURIComponent(id)}/import`, { method: 'POST', body, signal }),
  /* Where the imports running from one machine have got to, right now.

     A GET beside the POST above, and a poll rather than anything pushed: the
     import is one long request, and this is a second, short one asking what
     that first one is doing. It answers out of memory and knows nothing once an
     import is over — an empty list means nothing is running, which is the same
     answer for finished, failed and cancelled. What an import *did* comes back
     from the POST itself. */
  sourceImports: (id, { signal } = {}) =>
    request(`/api/remote/${encodeURIComponent(id)}/import`, { signal }),
  /* Stop one that is running, or forget the result of one that has finished.

     The same request for both, because from here they are the same gesture:
     stop showing me this. Stopping keeps every file that already arrived — it
     is closer to a pause than a cancel, since running the same import again
     skips those and carries on. */
  stopImport: (id, run) =>
    request(`/api/remote/${encodeURIComponent(id)}/import/${encodeURIComponent(run)}`,
      { method: 'DELETE' }),

  /* The picture a folder is drawn with, and what else it could be drawn with:
     every file under it that has a thumbnail, films first. What comes back is a
     file id — the picture itself is that file's own thumbnail, drawn through
     thumbURL like every other picture in the app, so a folder's face costs
     nothing to store and nothing to change. */
  folderArt: (path, vault = '') =>
    request(`/api/folders/art?path=${encodeURIComponent(path)}${vaultParam(vault)}`),
  /* Pick one, or pass no id to hand the choice back to the vault. */
  setFolderArt: (path, id = '') =>
    request('/api/folders/art', { method: 'POST', body: { path, id } }),
  /* --- The vaults inside the vault ------------------------------------ */

  subVaults: () => request('/api/subvaults'),
  createSubVault: (label, password) =>
    request('/api/subvaults', { method: 'POST', body: { label, password } }),
  /* Opening one is a second password on top of the session, never a way
     around it — every one of these is behind the same session cookie as the
     rest of the app. */
  unlockSubVault: (id, password) =>
    request(`/api/subvaults/${encodeURIComponent(id)}/unlock`, { method: 'POST', body: { password } }),
  lockSubVault: (id) =>
    request(`/api/subvaults/${encodeURIComponent(id)}/lock`, { method: 'POST' }),
  renameSubVault: (id, label) =>
    request(`/api/subvaults/${encodeURIComponent(id)}`, { method: 'PATCH', body: { label } }),
  /* Rotates the key the sub vault's files are stored under, so like the
     vault's own password change this can run for a long time on a full one. */
  changeSubVaultPassword: (id, password, newPassword, { migrate = true } = {}) =>
    request(`/api/subvaults/${encodeURIComponent(id)}/password`, {
      method: 'POST',
      body: { password, new_password: newPassword, migrate },
    }),
  migrateSubVault: (id) =>
    request(`/api/subvaults/${encodeURIComponent(id)}/migrate`, { method: 'POST' }),
  /* force is for a locked one, where what is about to be erased cannot be
     listed first. */
  deleteSubVault: (id, force = false) =>
    request(`/api/subvaults/${encodeURIComponent(id)}${force ? '?force=1' : ''}`, { method: 'DELETE' }),

  /* Moving a file or a folder from one vault into another. One call for both
     directions: assigning into a sub vault and taking something back out are
     the same operation with the two swapped.

     migrate defaults off here, so the move lands at once and the re-encryption
     runs behind it — assigning a folder of films should not be a progress bar. */
  assign: ({ target, from = '', to = '', migrate = false } = {}) =>
    request('/api/assign', { method: 'POST', body: { target, from, to, migrate } }),

  /* Re-encrypt whatever in a vault is still on an older key. Assignment leaves
     the moved files on the key of the vault they came from, and until this has
     run they can only be read while that vault is open — so whoever assigns is
     the one who has to start it. */
  migrateVault: (vault = '') => (vault
    ? request(`/api/subvaults/${encodeURIComponent(vault)}/migrate`, { method: 'POST' })
    : request('/api/vault/migrate', { method: 'POST' })),

  /* --- Another vault found on an account ------------------------------- */

  importVault: ({ provider, backupPassword, password, label = '', adoptBackup = true } = {}) =>
    request('/api/vaults/import', {
      method: 'POST',
      body: {
        provider,
        backup_password: backupPassword,
        password,
        label,
        adopt_backup: adoptBackup,
      },
    }),

  /* The other thing to do with a vault found on an account: erase its index,
     for an old install nobody wants back. Only the index goes — the parts stay
     on the account, and stop being withheld from the stray-parts sweep, which
     is where the room they take up is actually reclaimed. The server refuses to
     delete this vault's own index this way. */
  discardFoundVault: (provider) =>
    request(`/api/vaults/found/${encodeURIComponent(provider)}`, { method: 'DELETE' }),

  /* Replicating the index to the connected accounts. Turning it off erases the
     copies already out there, which is the point: leaving them would make it a
     setting that changed nothing. */
  setManifestBackup: (enabled) =>
    request('/api/vault/backup', { method: 'POST', body: { enabled } }),
  /* Claims the connected accounts for this vault, replacing a copy of the
     index it cannot open. What an account repaired after a recovery needs: the
     push that followed the recovery could not reach it, so it is still holding
     the index of the vault that died, and the guard that protects somebody
     else's backup would refuse it forever. */
  claimBackups: () =>
    request('/api/vault/backup', { method: 'POST', body: { enabled: true, force: true } }),

  /* A link a player outside the browser can follow. VLC has none of what
     authenticates this app — the session cookie is HttpOnly and SameSite=Strict
     — so the server mints a bearer link standing for this one file, which is
     the only credential a player can actually be handed. */
  streamLink: (id) => request(`/api/files/${encodeURIComponent(id)}/stream`, { method: 'POST' }),

  contentURL: (id, { download = false } = {}) =>
    `/api/files/${encodeURIComponent(id)}/content${download ? '?download=1' : ''}`,

  /* The stored preview image. The first row of a folder to ask for one makes
     the server gather that folder's whole pack; the rest are answered from
     memory, so a listing costs one round-trip to the accounts rather than one
     per picture.

     `version` only exists to change the address when the picture behind it
     changes — correcting a film's match replaces its poster, and an <img> whose
     src did not move keeps drawing the one it already decoded. */
  thumbURL: (id, version) => {
    const path = `/api/files/${encodeURIComponent(id)}/thumb`
    return version ? `${path}?v=${encodeURIComponent(version)}` : path
  },

  /* Stores a picture for a file that has none — one uploaded before
     thumbnails existed, or from the command line. */
  putThumb: (id, blob) => fetch(`/api/files/${encodeURIComponent(id)}/thumb`, {
    method: 'PUT',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'image/jpeg' },
    body: blob,
  }).then((resp) => {
    if (!resp.ok) throw new ApiError(`could not store the preview (${resp.status})`, 'HTTP_ERROR', resp.status)
    return true
  }),

  /* Film details, for the folders that ask for them.
     ---------------------------------------------------------------------
     Every call here goes to the local server exactly like the rest of this
     file. The server is what talks to the film database, and it stores what
     comes back — the details in the encrypted index, the poster as the file's
     thumbnail — so this page still fetches from nowhere but this machine, and
     a film matched once is never looked up again. */

  /* Whether a database key has been stored, and which folders are opted in.
     Never the key itself. */
  movieSettings: () => request('/api/movies'),
  /* Checked against the database before it is stored, so a bad key is refused
     here rather than halfway through a folder. '' clears it. */
  setMovieKey: (key) => request('/api/movies/key', { method: 'POST', body: { key } }),
  /* Turn matching on or off for a folder and everything under it. It stores
     the setting and nothing else: sweeping is a separate, explicit request. */
  setMovieLookup: (path, enabled) =>
    request('/api/movies/lookup', { method: 'POST', body: { path, enabled } }),
  /* Look up every unmatched video under a folder. Long — one search, one
     record and one poster per film — and the connection is held for the
     duration, the way converting a file is. `refresh` asks for the ones that
     already have details to be looked up again; matches corrected by hand are
     left alone either way. */
  scanMovies: (path, { refresh = false, signal } = {}) =>
    request('/api/movies/scan', { method: 'POST', body: { path, refresh }, signal }),

  movie: (id) => request(`/api/files/${encodeURIComponent(id)}/movie`),
  /* Look one file up now. With nothing passed it searches for whatever the
     filename suggests; `tmdbId` names a film outright, which is what choosing
     one from the candidate list does. */
  matchMovie: (id, { query = '', year = 0, tmdbId = 0 } = {}) =>
    request(`/api/files/${encodeURIComponent(id)}/movie`, {
      method: 'POST',
      body: { query, year, tmdb_id: tmdbId },
    }),
  /* Search without storing anything, so a wrong match is corrected against a
     list of real films rather than by retyping a query and hoping. */
  movieCandidates: (id, query, { signal } = {}) =>
    request(`/api/files/${encodeURIComponent(id)}/movie/candidates?q=${encodeURIComponent(query || '')}`,
      { signal }),
  forgetMovie: (id) =>
    request(`/api/files/${encodeURIComponent(id)}/movie`, { method: 'DELETE' }),

  /* Uploads go through XMLHttpRequest rather than fetch so the UI can show
     real progress while a large file is being split and scattered.

     `picks` is what was chosen — see upload.js. Each file carries the path it
     had inside the folder it came from, which is what lets a directory arrive
     as a directory rather than as its files in a heap; `dirs` names the folders
     of that directory that hold no file of their own. A file picked on its own
     has a path that is just its name and needs neither. */
  upload(picks, path, {
    overwrite = false, accounts = [], scheme = '', thumbs = [], dirs = [], vault = '', onProgress,
  } = {}) {
    return new Promise((resolve, reject) => {
      const form = new FormData()
      picks.forEach(({ file, path: rel }, i) => {
        form.append('files[]', file)
        /* Named for the file's position rather than its name: two files
           dropped together can share a name, and only some of them will have
           a picture at all. The path inside the folder is named the same way
           and for the same reason — twice over, since a dropped folder is
           where two files sharing a name is ordinary rather than unlucky. */
        if (rel && rel !== file.name) form.append(`rel-${i}`, rel)
        if (thumbs[i]) form.append(`thumb-${i}`, thumbs[i], 'thumb.jpg')
      })
      // The corners of the tree no file would make on the way past.
      for (const dir of dirs) form.append('dirs', dir)
      form.append('path', path)
      form.append('overwrite', String(overwrite))
      /* One field per account rather than a joined string: the server accepts
         either, and this way an ID is never mistaken for a list. Sending none
         leaves the choice to the vault's default. */
      for (const id of accounts) form.append('accounts', id)
      // Empty means no choice was made, and the count of accounts settles the
      // code the way it always did.
      if (scheme) form.append('scheme', scheme)

      const xhr = new XMLHttpRequest()
      xhr.open('POST', vault ? `/api/files?vault=${encodeURIComponent(vault)}` : '/api/files')
      xhr.withCredentials = true

      if (onProgress) {
        xhr.upload.addEventListener('progress', (e) => {
          if (e.lengthComputable) onProgress(e.loaded / e.total)
        })
      }

      xhr.addEventListener('load', () => {
        let payload
        try {
          payload = JSON.parse(xhr.responseText)
        } catch {
          reject(new ApiError(`upload failed (${xhr.status})`, 'HTTP_ERROR', xhr.status))
          return
        }
        if (xhr.status >= 200 && xhr.status < 300) resolve(payload)
        else reject(new ApiError(payload.error || 'upload failed', payload.code, xhr.status))
      })
      xhr.addEventListener('error', () => reject(new ApiError('network error during upload', 'NETWORK')))
      xhr.addEventListener('abort', () => reject(new ApiError('upload cancelled', 'ABORTED')))

      xhr.send(form)
    })
  },
}

/* The vault as an extra query parameter, for URLs that already carry one.
   Anything building a URL from scratch has to write `?vault=` itself — see
   upload(), which does. */
/* The sub vault a call is about, as a query parameter. first says this is the
   only parameter on the URL and so needs the "?" rather than the "&" — most
   callers append it to a path that already has one. */
function vaultParam(vault, first = false) {
  if (!vault) return ''
  return `${first ? '?' : '&'}vault=${encodeURIComponent(vault)}`
}

export function joinPath(dir, name) {
  return dir === '/' ? `/${name}` : `${dir}/${name}`
}

export function parentPath(dir) {
  if (dir === '/' ) return '/'
  const idx = dir.lastIndexOf('/')
  return idx <= 0 ? '/' : dir.slice(0, idx)
}
