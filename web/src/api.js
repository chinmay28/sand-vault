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
  /* The accounts an upload spreads over unless it names its own. An empty list
     clears the default, which puts every upload back to picking three clouds
     at random. */
  setDefaultAccounts: (accounts) =>
    request('/api/vault/defaults', { method: 'POST', body: { accounts } }),
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

  providerSpecs: () => request('/api/providers/specs'),
  providers: () => request('/api/providers'),
  addProvider: (kind, name, options) =>
    request('/api/providers', { method: 'POST', body: { kind, name, options } }),
  testProvider: (id) => request(`/api/providers/${encodeURIComponent(id)}/test`, { method: 'POST' }),
  /* What an account is called and what colour it wears. Only the fields passed
     change — an absent one is left alone — and neither touches the credentials
     or the parts sitting on the account: nothing is uploaded, downloaded or
     re-encrypted by renaming a cloud. A color of '' hands the choice back to
     the browser. */
  updateProvider: (id, { name, color } = {}) =>
    request(`/api/providers/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      body: {
        ...(name === undefined ? null : { name }),
        ...(color === undefined ? null : { color }),
      },
    }),

  /* Browser sign-in. The server holds the tokens; all the app ever sees is
     where to send the user and how far along the flow is. */
  oauthStart: (kind, { clientId = '', clientSecret = '', redirectUri = '' } = {}) =>
    request('/api/providers/oauth/start', {
      method: 'POST',
      body: { kind, client_id: clientId, client_secret: clientSecret, redirect_uri: redirectUri },
    }),
  oauthStatus: (flowId) => request(`/api/providers/oauth/${encodeURIComponent(flowId)}`),
  oauthExchange: (flowId, url) =>
    request('/api/providers/oauth/exchange', { method: 'POST', body: { flow_id: flowId, url } }),
  oauthComplete: (flowId, name, options) =>
    request('/api/providers/oauth/complete', {
      method: 'POST',
      body: { flow_id: flowId, name, options },
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
  relocate: ({ id, path, accounts, preview = false, vault = '', signal } = {}) =>
    request('/api/relocate', { method: 'POST', body: { id, path, accounts, preview, vault }, signal }),

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

  /* Replicating the index to the connected accounts. Turning it off erases the
     copies already out there, which is the point: leaving them would make it a
     setting that changed nothing. */
  setManifestBackup: (enabled) =>
    request('/api/vault/backup', { method: 'POST', body: { enabled } }),

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
     real progress while a large file is being split and scattered. */
  upload(files, path, { overwrite = false, accounts = [], thumbs = [], vault = '', onProgress } = {}) {
    return new Promise((resolve, reject) => {
      const form = new FormData()
      Array.from(files).forEach((file, i) => {
        form.append('files[]', file)
        /* Named for the file's position rather than its name: two files
           dropped together can share a name, and only some of them will have
           a picture at all. */
        if (thumbs[i]) form.append(`thumb-${i}`, thumbs[i], 'thumb.jpg')
      })
      form.append('path', path)
      form.append('overwrite', String(overwrite))
      /* One field per account rather than a joined string: the server accepts
         either, and this way an ID is never mistaken for a list. Sending none
         leaves the choice to the vault's default. */
      for (const id of accounts) form.append('accounts', id)

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
function vaultParam(vault) {
  return vault ? `&vault=${encodeURIComponent(vault)}` : ''
}

export function joinPath(dir, name) {
  return dir === '/' ? `/${name}` : `${dir}/${name}`
}

export function parentPath(dir) {
  if (dir === '/' ) return '/'
  const idx = dir.lastIndexOf('/')
  return idx <= 0 ? '/' : dir.slice(0, idx)
}
