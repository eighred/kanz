import { defineStore } from 'pinia'
import { auth, Unauthenticated, type Identity } from '../api/client'

// WHO THE USER IS, NOT WHAT THEY MAY DO.
//
// This store never decides authority. The gateway's capability mux is the only
// thing that does, and it is consulted on every call — so a screen that renders
// because this store said so, and then 403s, is CORRECT behaviour rather than a
// bug. Hiding a control the caller cannot use is a courtesy; it is never the
// control itself.
export const useSession = defineStore('session', {
  state: () => ({
    identity: null as Identity | null,
    resolved: false, // whether we have asked the server yet
  }),
  getters: {
    signedIn: (s) => s.identity !== null,
  },
  actions: {
    /** Ask the BFF who this browser is. Safe to call before every guarded route. */
    async resolve() {
      if (this.resolved) return
      try {
        this.identity = await auth.me()
      } catch (e) {
        if (!(e instanceof Unauthenticated)) throw e
        this.identity = null
      } finally {
        this.resolved = true
      }
    },
    async signIn(subject: string, credential: string) {
      this.identity = await auth.login(subject, credential)
      this.resolved = true
    },
    /**
     * Accept an invitation: creates the account AND signs in, in one step.
     * The invitation is single-use, so a failed attempt is not replayable.
     */
    async redeem(token: string, credential: string) {
      this.identity = await auth.redeem(token, credential)
      this.resolved = true
    },
    /**
     * Forget who this browser is, WITHOUT calling the server.
     *
     * Used when any request comes back 401: the session expired or was revoked
     * server-side, and the store is now describing somebody who is no longer
     * signed in. Leaving it in place makes the router guard wave the user
     * through to a screen where every call fails — an app that looks broken
     * rather than one that asks them to sign in again.
     */
    forget() {
      this.identity = null
      this.resolved = true
    },
    async signOut() {
      await auth.logout()
      this.identity = null
      this.resolved = true
    },
  },
})
