import { createRouter, createWebHistory } from 'vue-router'
import { useSession } from '../stores/session'
import LoginView from '../views/LoginView.vue'
import ApprovalsView from '../views/ApprovalsView.vue'
import EstateView from '../views/EstateView.vue'
import NodesView from '../views/NodesView.vue'
import ExposureView from '../views/ExposureView.vue'
import OrdersView from '../views/OrdersView.vue'
import OverridesView from '../views/OverridesView.vue'
import InstrumentsView from '../views/InstrumentsView.vue'
import PortfoliosView from '../views/PortfoliosView.vue'
import ProvisionsView from '../views/ProvisionsView.vue'
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
    { path: '/provisions', name: 'provisions', component: ProvisionsView },
    // REGISTERED FOR EVERY SIGNED-IN VISITOR, like every other route here. The
    // gateway decides who may read the queue — 403 without authz.Approve, 404
    // where no approver role is configured at all — and the view renders those
    // as different sentences. A router that hid the route would be this SPA
    // making an authorization decision it cannot make, and would hide the
    // "dual control is not switched on here" answer from the operator who most
    // needs it.
    { path: '/approvals', name: 'approvals', component: ApprovalsView },
    // ACT ONE OF THE SAME CONTROL, and registered on the same terms and for the
    // same reasons as /approvals above. The gateway decides who may read the
    // override queue — 403 without authz.Approve, 404 where no approver role is
    // configured at all — and the view renders those as different sentences.
    { path: '/overrides', name: 'overrides', component: OverridesView },
    { path: '/portfolios', name: 'portfolios', component: PortfoliosView },
    { path: '/portfolios/:id/exposure', name: 'exposure', component: ExposureView },
    { path: '/portfolios/:id/orders', name: 'orders', component: OrdersView },
    { path: '/instruments', name: 'instruments', component: InstrumentsView },
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
