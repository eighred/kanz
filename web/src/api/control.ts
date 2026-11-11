import { api, ApiError } from './client'

// The estate control plane, as the browser sees it (#371).
//
// TWO JSON CASINGS COEXIST ON /v1 AND THAT IS NOT A MISTAKE TO TIDY HERE. The
// control routes are protojson with default options — lowerCamelCase, and zero
// values OMITTED — while the risk routes use UseProtoNames + EmitDefaultValues,
// so they are snake_case with zeros present. These types describe the control
// shape only; a risk screen must not reuse them.
//
// Because zero values are omitted, every optional field below is genuinely
// optional in the wire data: `schedulable` absent means false, and
// `evictablePods` absent means zero. Reading them as `?? false` / `?? 0` is
// required, not defensive.

/** NodeStatus is the protobuf enum, serialised as its name. */
export type NodeStatus = 'NODE_STATUS_UNSPECIFIED' | 'NODE_STATUS_READY' | 'NODE_STATUS_NOT_READY'

export interface Node {
  name: string
  status?: NodeStatus
  roles?: string[]
  region?: string
  kubeletVersion?: string
  createdAt?: string
  schedulable?: boolean
  evictablePods?: number
}

export interface Cluster {
  region?: string
  online?: number
  offline?: number
}

/** ProvisionStatus is the protobuf enum, serialised as its name. */
export type ProvisionStatus =
  | 'PROVISION_STATUS_UNSPECIFIED'
  | 'PROVISION_STATUS_PENDING'
  | 'PROVISION_STATUS_INSTALLING'
  | 'PROVISION_STATUS_JOINED'
  | 'PROVISION_STATUS_FAILED'

export interface Provision {
  id: string
  hostname?: string
  status?: ProvisionStatus
  /** The failure reason when FAILED; absent otherwise. */
  message?: string
}

export interface VenueKeyStatus {
  venue: string
  configured?: boolean
}

/**
 * ready reports whether a node may be treated as healthy.
 *
 * DENY BY DEFAULT, and this is the whole reason the helper exists rather than a
 * `!== 'NODE_STATUS_NOT_READY'` at each call site. A node whose Ready condition
 * is absent or Unknown serialises as NODE_STATUS_UNSPECIFIED — and because
 * protojson omits zero values, it may not appear in the JSON at all. Anything
 * other than an explicit READY must render as not-ready: an unknown node shown
 * as green is the one error here with an operational cost.
 */
export function ready(n: Node): boolean {
  return n.status === 'NODE_STATUS_READY'
}

/**
 * node is the path segment for one node's actions.
 *
 * THE NAME GOES IN THE PATH AND NOWHERE ELSE. The gateway takes it from the URL
 * and overwrites whatever a body carried, with the reason written beside the
 * code: "two spellings of the same identity is a way to drain the node you were
 * not looking at". Encoding it here keeps this client on the same single
 * spelling — a node name is a Kubernetes object name, so it needs no escaping
 * today, and relying on that instead of saying it is how it stops being true.
 */
function node(name: string): string {
  return `/api/v1/control/nodes/${encodeURIComponent(name)}`
}

/**
 * settled reports whether a provision has stopped moving.
 *
 * DENY BY DEFAULT, like ready() above and for the same reason: protojson omits
 * the zero value, so a provision whose status is UNSPECIFIED may carry no status
 * field at all. Anything that is not an explicit terminal state is treated as
 * still running — a run shown as finished when nobody knows is how a half-joined
 * node gets forgotten.
 */
export function settled(p: Provision): boolean {
  return p.status === 'PROVISION_STATUS_JOINED' || p.status === 'PROVISION_STATUS_FAILED'
}

export const control = {
  nodes: () => api.get<{ nodes?: Node[] }>('/api/v1/control/nodes').then((r) => r.nodes ?? []),
  clusters: () => api.get<{ clusters?: Cluster[] }>('/api/v1/control/clusters').then((r) => r.clusters ?? []),
  venues: () => api.get<{ venues?: VenueKeyStatus[] }>('/api/v1/control/venues').then((r) => r.venues ?? []),
  provisions: () =>
    api.get<{ provisions?: Provision[] }>('/api/v1/control/provisions').then((r) => r.provisions ?? []),

  /**
   * setVenueKeys writes trading credentials for one venue.
   *
   * THE MOST DANGEROUS BODY ON THIS SURFACE, and the gateway says so beside its
   * own handler: nothing in that path logs the request, on success or on error.
   * The same rule applies here — the credential is passed straight through and
   * is never put in a URL, a query string, a thrown error or a console.
   *
   * The reply carries exchange_account_id, which is the exchange's OWN id for
   * the account these keys spend, as reported during the pre-write proof. It is
   * a public fact and never key material — it is the only evidence the caller
   * gets that the credential actually works, so it is returned rather than
   * discarded.
   */
  setVenueKeys: (venue: string, apiKey: string, apiSecret: string, passphrase: string) =>
    api.put<{ exchange_account_id?: string }>(
      `/api/v1/control/venues/${encodeURIComponent(venue)}/keys`,
      { api_key: apiKey, api_secret: apiSecret, passphrase },
    ),

  // THE FOUR NODE ACTIONS. Each returns an empty message on success — the
  // operator.v1 responses carry no fields — so the caller's only job afterwards
  // is to re-read the estate rather than to trust a returned state.
  //
  // Drain is ASYNCHRONOUS and the proto says so: it returns once the node is
  // cordoned, and eviction proceeds in the background honouring
  // PodDisruptionBudgets. A screen that reported "drained" when this resolves
  // would be reporting something the platform has not claimed.
  cordon: (name: string) => api.post<Record<string, never>>(`${node(name)}/cordon`),
  uncordon: (name: string) => api.post<Record<string, never>>(`${node(name)}/uncordon`),
  drain: (name: string) => api.post<Record<string, never>>(`${node(name)}/drain`),
  setRegion: (name: string, region: string) =>
    api.post<Record<string, never>>(`${node(name)}/region`, { region }),
}

/**
 * describe turns a failure into something an operator can act on.
 *
 * THE 404 IS THE ONE THAT MATTERS. The gateway registers the control routes
 * only when it is configured with a control plane; without one they simply do
 * not exist. Rendered as an empty list — the obvious thing — that reads as a
 * healthy estate with no nodes, which is the most dangerous screen this
 * application could draw. It has to say that the gateway fronts no control
 * plane instead.
 *
 * 403 is likewise not an error in the system: the account is real and signed in,
 * it just does not carry operator authority. Saying so stops an operator
 * debugging a gateway that is behaving exactly as configured.
 */
export function describe(e: unknown): string {
  if (!(e instanceof ApiError)) {
    return e instanceof Error ? e.message : 'the request failed'
  }
  switch (e.status) {
    case 403:
      return 'Your account does not carry operator authority. Ask an operator to grant it.'
    case 404:
      return 'This gateway fronts no control plane, so there is nothing to list — the estate is not unreachable, it is not configured.'
    case 501:
      return 'This capability is switched off on the gateway.'
    case 412:
      return 'The venue refused the credential.'
    case 502:
    case 503:
      return 'The control plane is unreachable. This says nothing about the estate itself.'
    default:
      return e.message
  }
}
