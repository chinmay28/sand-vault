import { useEffect, useRef } from 'react'
import { api } from './api'

/* The browser's half of a sign-in.

   Two dialogs run one: the connect dialog, where a sign-in becomes a new
   account, and the edit dialog, where it replaces the credentials of an account
   already connected. Neither ever handles a credential — SAND opens the
   provider's own consent page, the provider redirects back to this server, and
   the tokens are exchanged and stored there. All the app learns is how far
   along the flow is.

   Everything in here is the part the two share: where the provider sends the
   browser back to, how to open its window, the crumb that survives a sign-in
   taking over the tab, and the poll that waits for an answer. */

const PENDING_FLOW_KEY = 'sand.oauth.flow'

/* A sign-in that took over the tab instead of opening a window leaves the app
   entirely; this is the crumb that lets it pick the flow back up on return.

   A flow with a provider_id on it belongs to an account that is already
   connected, and is resumed from that account's edit dialog rather than from
   the connect one. */
export function pendingOAuthFlow() {
  try {
    const raw = window.sessionStorage.getItem(PENDING_FLOW_KEY)
    return raw ? JSON.parse(raw) : null
  } catch {
    return null
  }
}

export function rememberFlow(flow) {
  try {
    window.sessionStorage.setItem(PENDING_FLOW_KEY, JSON.stringify(flow))
  } catch { /* private mode: the popup path still works */ }
}

export function forgetFlow() {
  try {
    window.sessionStorage.removeItem(PENDING_FLOW_KEY)
  } catch { /* nothing to clean up */ }
}

export const callbackURL = () => `${window.location.origin}/api/providers/oauth/callback`

/* Send the account holder to the provider. A popup where the browser allows
   one, and the whole tab where it does not — the flow is remembered either way,
   so returning resumes it. */
export function openAuthWindow(authURL) {
  const win = window.open(authURL, 'sand-oauth', 'width=560,height=700')
  if (!win || win.closed || typeof win.closed === 'undefined') {
    window.location.href = authURL
    return null
  }
  win.focus?.()
  return win
}

/* Wait for the provider to hand the account back.

   The server is polled rather than pushed at: the window that lands on the
   callback posts a message home, but a sign-in that took over the tab has no
   window to post from, and a popup that was closed by hand never posts at all.
   The message only makes the next poll happen sooner.

   `onReady` and `onFailed` are read through a ref, so a caller passing fresh
   closures every render does not restart the poll under itself. */
export function useSignInResult({ flowId, active, onReady, onFailed }) {
  const handlers = useRef({ onReady, onFailed })
  handlers.current = { onReady, onFailed }

  useEffect(() => {
    if (!active || !flowId) return
    let stopped = false

    const check = async () => {
      try {
        const resp = await api.oauthStatus(flowId)
        if (stopped) return
        if (resp.status === 'ready') {
          forgetFlow()
          handlers.current.onReady(resp)
        } else if (resp.status === 'error') {
          forgetFlow()
          handlers.current.onFailed(resp.error || 'the provider refused the sign-in')
        }
      } catch (err) {
        if (stopped) return
        // A flow the server has forgotten is not coming back, and neither is
        // one whose vault locked underneath it. Either way, stop spinning.
        if (err.status === 404) {
          forgetFlow()
          handlers.current.onFailed('That sign-in expired. Start it again.')
        } else if (err.status === 401) {
          forgetFlow()
          handlers.current.onFailed('The vault locked while you were signing in. Unlock it and start again.')
        }
      }
    }

    check()
    const timer = window.setInterval(check, 2000)
    const onMessage = (event) => {
      if (event.origin !== window.location.origin) return
      if (event.data && event.data.source === 'sand-oauth') check()
    }
    window.addEventListener('message', onMessage)

    return () => {
      stopped = true
      window.clearInterval(timer)
      window.removeEventListener('message', onMessage)
    }
  }, [flowId, active])
}
