import { api, ApiError } from './client'

export interface CustodyBreak {
  break_id: string; kind: string; key: string; ibor: string; custodian: string; difference: string
  status: string; assignee?: string; explanation?: string; first_seen_at: string
  last_seen_at: string; status_changed_at: string; age_seconds: number
  revision?: string; values_state?: 'exact' | 'legacy_unverified' | 'unavailable'; actions_enabled?: boolean
}

const decimal = /^-?\d+(?:\.\d+)?$/
const revision = (v: unknown): v is string => typeof v === 'string' && /^[1-9]\d{0,18}$/.test(v) && BigInt(v) <= 9223372036854775807n
const exact = (v: unknown): v is string => typeof v === 'string' && v.length <= 256 && decimal.test(v)
const statuses = ['open', 'assigned', 'explained', 'resolved']

export interface CustodyDecision { request_id: string; expected_revision: string; action: 'claim' | 'explain'; explanation?: string; review_contract: 'exact-v1' }
interface ActionState { status: string; assignee: string; explanation: string; revision: string }
export interface CustodyEvidence {
  request_id: string; break_id: string; actor: string; action: 'claim' | 'explain'; recorded_at: string
  before: ActionState; after: ActionState; values: { ibor: string; custodian: string; difference: string }
}

function parseBreak(value: unknown): CustodyBreak {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Invalid custody response.')
  const row = value as Record<string, unknown>
  const required = ['break_id', 'kind', 'key', 'status', 'first_seen_at', 'last_seen_at', 'status_changed_at']
  if (required.some((key) => typeof row[key] !== 'string' || row[key] === '')) throw new Error('Invalid custody response.')
  if (['assignee', 'explanation'].some((key) => row[key] !== undefined && typeof row[key] !== 'string')) throw new Error('Invalid custody response.')
  if (['first_seen_at', 'last_seen_at', 'status_changed_at'].some((key) => !Number.isFinite(Date.parse(row[key] as string)))) throw new Error('Invalid custody response.')
  if (!Number.isSafeInteger(row.age_seconds) || (row.age_seconds as number) < 0) throw new Error('Invalid custody response.')
  if (row.values_state !== undefined && !['exact', 'legacy_unverified', 'unavailable'].includes(row.values_state as string)) throw new Error('Invalid custody response.')
  if (['ibor', 'custodian', 'difference'].some((key) => typeof row[key] !== 'string')) throw new Error('Invalid custody response.')
  if ((row.values_state === 'exact' || row.values_state === undefined) && ['ibor', 'custodian', 'difference'].some((key) => !exact(row[key]))) throw new Error('Invalid custody response.')
  if (row.revision !== undefined && !revision(row.revision)) throw new Error('Invalid custody response.')
  return row as unknown as CustodyBreak
}

export const custody = {
  async breaks(): Promise<CustodyBreak[]> {
    const body = await api.get<{ breaks: unknown; count: unknown; actions_enabled?: unknown }>('/api/v1/custody/breaks')
    const rows = body?.breaks === null ? [] : body?.breaks
    if (!Array.isArray(rows) || body.count !== rows.length) throw new Error('Invalid custody response.')
    if (body.actions_enabled !== undefined && typeof body.actions_enabled !== 'boolean') throw new Error('Invalid custody response.')
    return rows.map((value) => {
      const row = parseBreak(value)
      return { ...row, actions_enabled: body.actions_enabled === true && row.values_state === 'exact' && revision(row.revision) && BigInt(row.revision) < 9223372036854775807n && statuses.slice(0, 3).includes(row.status) }
    })
  },
  async act(row: CustodyBreak, decision: CustodyDecision): Promise<CustodyEvidence> {
    if (!row.actions_enabled || row.values_state !== 'exact' || !revision(row.revision) || BigInt(row.revision) >= 9223372036854775807n || decision.expected_revision !== row.revision || !decision.request_id || decision.review_contract !== 'exact-v1' || !['claim', 'explain'].includes(decision.action) || (decision.action === 'explain' && !decision.explanation?.trim())) throw new Error('Invalid custody review.')
    const value = await api.post<CustodyEvidence>(`/api/v1/custody/breaks/${encodeURIComponent(row.break_id)}/actions`, decision)
    const state = (s: ActionState) => s && statuses.includes(s.status) && typeof s.assignee === 'string' && typeof s.explanation === 'string' && revision(s.revision)
    if (!value || value.request_id !== decision.request_id || value.break_id !== row.break_id || value.action !== decision.action || typeof value.actor !== 'string' || !value.actor.trim() || !Number.isFinite(Date.parse(value.recorded_at)) || !state(value.before) || !state(value.after) || value.before.revision !== row.revision || BigInt(value.after.revision) !== BigInt(row.revision) + 1n || value.before.status !== row.status || value.before.assignee !== (row.assignee || '') || value.before.explanation !== (row.explanation || '') || !value.values || value.values.ibor !== row.ibor || value.values.custodian !== row.custodian || value.values.difference !== row.difference) throw new Error('Custody action evidence could not be verified.')
    if (decision.action === 'claim' ? value.after.status !== 'assigned' || value.after.assignee !== value.actor : value.after.status !== 'explained' || value.after.explanation !== decision.explanation) throw new Error('Custody action evidence could not be verified.')
    return value
  },
}

export function describeCustodyAction(error: unknown): string {
  if (error instanceof ApiError && error.status === 409) return 'The break or request changed. Reload the queue and review a fresh action.'
  if (error instanceof ApiError && error.status === 403) return 'Your account is not permitted to record custody actions.'
  if (error instanceof ApiError && error.status === 404) return 'This break or the reviewed-action service is unavailable.'
  if (error instanceof ApiError && error.status === 503) return 'Durable evidence or verified exact values are unavailable. Source replay and reconciliation may be required.'
  if (error instanceof ApiError && error.status === 400) return 'This deployment refused the review contract. Reload before reviewing another action.'
  return 'The outcome could not be verified. Retry this same reviewed request to recover its evidence safely.'
}

export function describeCustody(error: unknown): string {
  if (error instanceof ApiError && error.status === 403) return 'Your account is not permitted to read the custody-break queue.'
  if (error instanceof ApiError && error.status === 404) return 'Custody reconciliation is not enabled in this deployment.'
  if (error instanceof ApiError && error.status === 503) return 'Custody reconciliation is currently unavailable.'
  return 'Custody-break data could not be verified.'
}
