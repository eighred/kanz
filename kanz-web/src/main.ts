import './styles.css'
import { createApp } from 'vue'
import { createPinia } from 'pinia'
import App from './App.vue'
import { router } from './router'
import { setUnauthenticatedHandler } from './api/client'
import { useSession } from './stores/session'

const app = createApp(App)
app.use(createPinia()).use(router)

// THE SESSION CAN END WHILE A PAGE IS OPEN — it expires, or an operator revokes
// it. Without this the store still says "signed in", so the router guard waves
// the user through and every call on the page fails: an application that looks
// broken instead of one that asks them to sign in again.
//
// Registered here, after Pinia is installed, because useSession needs the
// active instance.
setUnauthenticatedHandler(() => {
  const session = useSession()
  const wasSignedIn = session.signedIn
  session.forget()
  // Only navigate if we thought we were signed in. A 401 from the guard's own
  // /auth/me on a signed-out visitor is the ORDINARY case, and redirecting
  // there would fight the guard that issued the request.
  if (wasSignedIn && !router.currentRoute.value.meta.public) {
    void router.push({ name: 'login', query: { next: router.currentRoute.value.fullPath } })
  }
})

app.mount('#app')
