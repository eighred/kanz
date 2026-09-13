import HouseholdView from '../views/HouseholdView.vue'
import EvidenceView from '../views/EvidenceView.vue'
import CopilotView from '../views/CopilotView.vue'
import OverviewView from '../views/OverviewView.vue'
import AuditView from '../views/AuditView.vue'
import AuditEventView from '../views/AuditEventView.vue'
import NotFoundView from '../views/NotFoundView.vue'
import { createRouter, createWebHistory } from 'vue-router'
import { useSession } from '../stores/session'
import LoginView from '../views/LoginView.vue'
import ApprovalsView from '../views/ApprovalsView.vue'
import EstateView from '../views/EstateView.vue'
import NodesView from '../views/NodesView.vue'
import ExposureView from '../views/ExposureView.vue'
import OrdersView from '../views/OrdersView.vue'
import OverridesView from '../views/OverridesView.vue'
import MandateChangesView from '../views/MandateChangesView.vue'
import MandateProposalView from '../views/MandateProposalView.vue'
import InstrumentsView from '../views/InstrumentsView.vue'
import PortfoliosView from '../views/PortfoliosView.vue'
import ProvisionsView from '../views/ProvisionsView.vue'
import RedeemView from '../views/RedeemView.vue'
import VenuesView from '../views/VenuesView.vue'
import InvitationsView from '../views/InvitationsView.vue'
import PreflightView from '../views/PreflightView.vue'
import BrokerAccountsView from '../views/BrokerAccountsView.vue'
import BrokerAccountView from '../views/BrokerAccountView.vue'
import CustodyBreaksView from '../views/CustodyBreaksView.vue'
import ReferenceDataView from '../views/ReferenceDataView.vue'

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/households', name: 'households', component: HouseholdView },
    { path: '/copilot', name: 'copilot', component: CopilotView },
    { path: '/audit-evidence', name: 'audit-evidence', component: EvidenceView },
    { path: '/', redirect: '/overview' },
    { path: '/overview', name: 'overview', component: OverviewView },
    { path: '/audit', name: 'audit', component: AuditView },
    { path: '/audit/events/:id', name: 'audit-event', component: AuditEventView },
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
    // ACT TWO OF THE SAME CONTROL, on the same terms as the two routes above.
    // The gateway decides who may read it and answers three different things —
    // 404 where no MANDATE role is configured, 403 where one is and this account
    // does not hold it, 401 where the session carries no tenant — and the view
    // renders those as different sentences.
    //
    // NOTE THE CAPABILITY IS authz.Mandate AND NOT authz.Approve, deliberately:
    // under one capability, the second signature on a mandate change and the
    // second signature on a held order come from the same pool, so one signatory
    // could sign away a limit and then sign the trade that limit existed to stop.
    // So this link being visible to an order approver who then gets a 403 is the
    // control working, and that is what the view says.
    { path: '/mandate-changes', name: 'mandate-changes', component: MandateChangesView },
    { path: '/mandate-proposal', name: 'mandate-proposal', component: MandateProposalView },
    { path: '/portfolios', name: 'portfolios', component: PortfoliosView },
    { path: '/portfolios/:id/exposure', name: 'exposure', component: ExposureView },
    { path: '/portfolios/:id/orders', name: 'orders', component: OrdersView },
    { path: '/instruments', name: 'instruments', component: InstrumentsView },
    { path: '/invitations', name: 'invitations', component: InvitationsView },
    { path: '/preflight', name: 'preflight', component: PreflightView },
    { path: '/broker-accounts', name: 'broker-accounts', component: BrokerAccountsView },
    { path: '/broker-accounts/:id', name: 'broker-account', component: BrokerAccountView },
    { path: '/custody-breaks', name: 'custody-breaks', component: CustodyBreaksView },
    { path: '/reference-data', name: 'reference-data', component: ReferenceDataView },
    { path: '/:pathMatch(.*)*', name: 'not-found', component: NotFoundView },
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
