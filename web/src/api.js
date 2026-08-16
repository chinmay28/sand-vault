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

  providerSpecs: () => request('/api/providers/specs'),
  providers: () => request('/api/providers'),
  addProvider: (kind, name, options) =>
    request('/api/providers', { method: 'POST', body: { kind, name, options } }),
  testProvider: (id) => request(`/api/providers/${encodeURIComponent(id)}/test`, { method: 'POST' }),

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

  list: (path) => request(`/api/files?path=${encodeURIComponent(path)}`),
  /* Only the server can answer this: the file index is encrypted everywhere
     else, so there is nothing on any cloud account to ask. */
  search: (query, { path = '/', type, limit, signal } = {}) => {
    const params = new URLSearchParams({ q: query })
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
  moveFile: (id, dir, name) =>
    request(`/api/files/${encodeURIComponent(id)}/move`, { method: 'POST', body: { dir, name } }),

  /* Move a file, or a folder and everything under it, onto other clouds.
     Only the parts that are not already on one of the chosen accounts are
     copied — swapping one cloud out of three moves one part, not the file — so
     `preview` is worth asking first: it answers out of the index alone, without
     contacting a single account, and says exactly how much would travel. */
  relocate: ({ id, path, accounts, preview = false, signal } = {}) =>
    request('/api/relocate', { method: 'POST', body: { id, path, accounts, preview }, signal }),

  createFolder: (path) => request('/api/folders', { method: 'POST', body: { path } }),
  deleteFolder: (path, recursive) =>
    request(`/api/folders?path=${encodeURIComponent(path)}${recursive ? '&recursive=1' : ''}`,
      { method: 'DELETE' }),

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
     per picture. */
  thumbURL: (id) => `/api/files/${encodeURIComponent(id)}/thumb`,

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

  /* Uploads go through XMLHttpRequest rather than fetch so the UI can show
     real progress while a large file is being split and scattered. */
  upload(files, path, { overwrite = false, accounts = [], thumbs = [], onProgress } = {}) {
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
      xhr.open('POST', '/api/files')
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

export function joinPath(dir, name) {
  return dir === '/' ? `/${name}` : `${dir}/${name}`
}

export function parentPath(dir) {
  if (dir === '/' ) return '/'
  const idx = dir.lastIndexOf('/')
  return idx <= 0 ? '/' : dir.slice(0, idx)
}
