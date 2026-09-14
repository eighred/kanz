// The browser's only way to reach the estate (#371).
//
// THERE IS NO TOKEN HERE, AND THERE MUST NEVER BE ONE. Authentication is the
// httpOnly session cookie the BFF set; the BFF attaches the bearer server-side
// when it proxies to the gateway. A token this code could read is a token an XSS
// could read, and a kanz token carries kanz-trader — so the absence of any
// Authorization header below is the design, not an omission.
//
// Same origin in every deployed shape, so `credentials: 'same-origin'` is enough
// and no CORS mode is set. In `npm run dev` Vite proxies /api and /auth to the
// BFF, which keeps the dev shape same-origin too — otherwise the cookie stops
// working the moment you stop using the dev server.

export class ApiError extends Error {
  constructor(readonly status: number, message: string, readonly code?: string) {
    super(message)
    this.name = 'ApiError'
  }
}

/** Raised on 401 so the router can send the user to /login exactly once. */
export class Unauthenticated extends ApiError {
  constructor(message = 'not authenticated') {
    super(401, message)
    this.name = 'Unauthenticated'
  }
}

// A 401 CAN ARRIVE AT ANY MOMENT, not only at sign-in: sessions expire, and one
// can be revoked server-side while a page sits open. Whoever holds the session
// state registers here so it can be dropped the instant the server disowns it.
//
// It is a REGISTRATION rather than a direct import because the session store
// imports this module; calling into it from here would be a cycle. The handler
// must not navigate on its own — /auth/me answers 401 for every signed-out
// visitor, and a redirect from inside that call would fight the router guard
// that made it.
let onUnauthenticated: (() => void) | null = null

/** Register the callback invoked whenever a request is refused with 401. */
export function setUnauthenticatedHandler(fn: () => void): void {
  onUnauthenticated = fn
}

async function request<T>(method: string, path: string, body?: unknown, acceptConflict = false): Promise<T> {
  const res = await fetch(path, {
    method,
    credentials: 'same-origin',
    headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })

  if (res.status === 401) {
    onUnauthenticated?.()
    throw new Unauthenticated()
  }
  if (!res.ok && !(acceptConflict && res.status === 409)) {
    // The server's message is shown as-is when it is JSON we recognise; the raw
    // body is NOT surfaced otherwise, because an upstream error page is not a
    // message for a user and may carry internals.
    let message = `request failed (${res.status})`
    let code: string | undefined
    try {
      const data = await res.json()
      if (typeof data?.error === 'string') message = data.error
      if (typeof data?.code === 'string' && /^[a-z][a-z0-9_]{0,63}$/.test(data.code)) code = data.code
    } catch {
      /* not JSON — keep the generic message */
    }
    throw new ApiError(res.status, message, code)
  }
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

export const api = {
  get: <T>(path: string, options?: { acceptConflict?: boolean }) => request<T>('GET', path, undefined, options?.acceptConflict),
  post: <T>(path: string, body?: unknown) => request<T>('POST', path, body),
  put: <T>(path: string, body?: unknown) => request<T>('PUT', path, body),
}

export interface Identity {
  subject: string
  tenant: string
  expires_at: string
}

export const auth = {
  /** Exchange a credential for a session. The token stays on the server. */
  login: (subject: string, credential: string) =>
    api.post<Identity>('/auth/login', { subject, credential }),
  /** Accept an invitation: sets the credential AND signs in, in one step. */
  redeem: (token: string, credential: string) =>
    api.post<Identity>('/auth/redeem', { token, credential }),
  logout: () => api.post<void>('/auth/logout'),
  me: () => api.get<Identity>('/auth/me'),
}
