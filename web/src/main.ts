import { initializeBrowserRenderer } from './api/browser-renderer-loader'
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
  // A rejected request on a public page can already be handling sign-in.
  if (wasSignedIn && !router.currentRoute.value.meta.public) {
    void router.push({ name: 'login', query: { next: router.currentRoute.value.fullPath } })
  }
})

app.mount('#app')
void initializeBrowserRenderer()
