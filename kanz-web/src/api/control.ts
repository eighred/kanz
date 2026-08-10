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

export const control = {
  nodes: () => api.get<{ nodes?: Node[] }>('/api/v1/control/nodes').then((r) => r.nodes ?? []),
  clusters: () => api.get<{ clusters?: Cluster[] }>('/api/v1/control/clusters').then((r) => r.clusters ?? []),
  venues: () => api.get<{ venues?: VenueKeyStatus[] }>('/api/v1/control/venues').then((r) => r.venues ?? []),
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
