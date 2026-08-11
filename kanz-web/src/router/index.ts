import { createRouter, createWebHistory } from 'vue-router'
import { useSession } from '../stores/session'
import LoginView from '../views/LoginView.vue'
import EstateView from '../views/EstateView.vue'
import NodesView from '../views/NodesView.vue'
import PortfoliosView from '../views/PortfoliosView.vue'
import RedeemView from '../views/RedeemView.vue'
import VenuesView from '../views/VenuesView.vue'

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', redirect: '/venues' },
    { path: '/login', name: 'login', component: LoginView, meta: { public: true } },
    // PUBLIC BY NECESSITY: whoever opens this holds an invitation and nothing
    // else — requiring a session to accept one would be a loop with no entry.
    { path: '/redeem', name: 'redeem', component: RedeemView, meta: { public: true } },
    { path: '/venues', name: 'venues', component: VenuesView },
    { path: '/estate', name: 'estate', component: EstateView },
    { path: '/nodes', name: 'nodes', component: NodesView },
    { path: '/portfolios', name: 'portfolios', component: PortfoliosView },
  ],
})

// SENDS AN UNAUTHENTICATED VISITOR TO THE SIGN-IN PAGE. It is NOT a security
// control — the gateway refuses every call this browser has no authority for,
// whatever the router allows. Its only job is that arriving at /venues with no
// session shows a login form instead of an empty page and a console error.
router.beforeEach(async (to) => {
  if (to.meta.public) return true
  const session = useSession()
  await session.resolve()
  return session.signedIn ? true : { name: 'login', query: { next: to.fullPath } }
})
