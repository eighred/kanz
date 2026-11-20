import { api, auth, ApiError } from './client'
import { users } from './users'
import { downloadJson } from './evidence'

export interface CustodyDetection {
  run_id: string; statement_id: string; business_date: string
  journal_position?: string; scope_mode?: string
}

export interface CustodyBreak {
  portfolio_id?: string; custodian_id?: string; detection?: CustodyDetection
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

function nonempty(value: unknown): value is string {
  return typeof value === 'string' && value.trim().length > 0
}

function validDetection(value: unknown): value is CustodyDetection {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false
  const source = value as Record<string, unknown>
  if (!nonempty(source.run_id) || !nonempty(source.statement_id) || !nonempty(source.business_date)) return false
  const day = source.business_date
  if (!/^\d{4}-\d{2}-\d{2}$/.test(day)) return false
  const instant = new Date(day + 'T00:00:00Z')
  if (!Number.isFinite(instant.getTime()) || instant.toISOString().slice(0, 10) !== day) return false
  const position = source.journal_position
  if (position !== undefined && (typeof position !== 'string' || !/^(0|[1-9]\d{0,18})$/.test(position) || BigInt(position) > 9223372036854775807n)) return false
  return source.scope_mode === undefined || nonempty(source.scope_mode)
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
  if ((row.portfolio_id !== undefined || row.custodian_id !== undefined || row.detection !== undefined) &&
      (!nonempty(row.portfolio_id) || !nonempty(row.custodian_id))) throw new Error('Invalid custody response.')
  if (row.detection !== undefined && !validDetection(row.detection)) throw new Error('Invalid custody response.')
  return row as unknown as CustodyBreak
}

export interface CustodyRerunRequest { portfolio_id: string; custodian_id: string; business_date: string; request_id: string }
export interface CustodyRerunReceipt extends CustodyRerunRequest {
  actor: string; run_id: string; completed_at: string; outcome: 'clean' | 'breaks' | 'no_statement' | 'failed'
  statement_id?: string; scope_mode?: string; journal_position?: string; failure_code?: string
}

export interface CustodySubject { portfolio_id: string; custodian_id: string; business_date: string }
export type TransactionComparison =
  | { transaction_pass: 'ran'; statement_reference_count: string; book_reference_count: string; unreferenced_book_trades: string }
  | { transaction_pass: 'balances_only' | 'unknown' }
export type DatedCustodyResult = CustodySubject & { tenant_id: string } & (
  | { state: 'not_compared' | 'pending' | 'unknown'; statement_id?: string; reason?: 'journal_changed' }
  | { state: 'recorded'; outcome: CustodyRerunReceipt['outcome']; run_id: string; completed_at: string; statement_id?: string; scope_mode?: string; journal_position?: string; transactions?: TransactionComparison }
)

export function downloadCustodyComparison(receipt: Extract<DatedCustodyResult, { state: 'recorded' }>): void {
  const { transactions, ...comparison } = receipt
  downloadJson({
    format: 'kanz.custody-comparison.v1',
    ...comparison,
    ...(transactions?.transaction_pass === 'ran' ? { transaction_comparison_basis: 'external_reference_membership' } : {}),
    ...transactions,
  }, `custody-comparison-${receipt.business_date}.json`)
}

export const custody = {
  async dated(subject: CustodySubject, tenant: string, signal?: AbortSignal): Promise<DatedCustodyResult> {
    const query = new URLSearchParams({ custodian_id: subject.custodian_id, business_date: subject.business_date })
    const result = await api.get<Record<string, unknown>>('/api/v1/portfolios/' + encodeURIComponent(subject.portfolio_id) + '/custody/comparison?' + query, { expectedStatus: 200, signal })
    const invalid = () => new Error('Dated result could not be verified.')
    if (!nonempty(tenant) || !result || result.tenant_id !== tenant || result.portfolio_id !== subject.portfolio_id || result.custodian_id !== subject.custodian_id ||
        result.business_date !== subject.business_date || (result.statement_id !== undefined && !nonempty(result.statement_id))) throw invalid()
    if (result.reason !== undefined && (result.state !== 'pending' || result.reason !== 'journal_changed')) throw invalid()
    const scope = { ...subject, tenant_id: tenant }
    const referenceCounts = ['statement_reference_count', 'book_reference_count', 'unreferenced_book_trades'] as const
    const verified = result.state === 'recorded' && (result.outcome === 'clean' || result.outcome === 'breaks')
    let transactions: TransactionComparison | undefined
    if (verified) {
      if (result.transaction_pass === 'ran') {
        const count = (key: typeof referenceCounts[number]): string => {
          const value = result[key]
          if (typeof value !== 'string' || !/^(0|[1-9]\d{0,18})$/.test(value) || BigInt(value) > 9223372036854775807n) throw invalid()
          return value
        }
        transactions = { transaction_pass: 'ran', statement_reference_count: count('statement_reference_count'),
          book_reference_count: count('book_reference_count'), unreferenced_book_trades: count('unreferenced_book_trades') }
      } else if (result.transaction_pass === 'balances_only' || result.transaction_pass === 'unknown') {
        transactions = { transaction_pass: result.transaction_pass }
      } else throw invalid()
    } else if (result.transaction_pass !== undefined) throw invalid()
    if (transactions?.transaction_pass !== 'ran' && referenceCounts.some(key => result[key] !== undefined)) throw invalid()
    if (result.state === 'recorded') {
      if (!nonempty(result.run_id) || !nonempty(result.completed_at) || !Number.isFinite(Date.parse(result.completed_at)) ||
          !['clean', 'breaks', 'no_statement', 'failed'].some(value => value === result.outcome)) throw invalid()
      if (result.outcome === 'no_statement' && result.statement_id !== undefined) throw invalid()
      const scoped = result.journal_position !== undefined || result.scope_mode !== undefined
      if (scoped && (!['declared', 'derived'].some(value => value === result.scope_mode) || typeof result.journal_position !== 'string' ||
          !/^(0|[1-9]\d{0,18})$/.test(result.journal_position) || BigInt(result.journal_position) > 9223372036854775807n)) throw invalid()
      if ((result.outcome === 'clean' || result.outcome === 'breaks') && (!result.statement_id || !scoped)) throw invalid()
      return { ...scope, state: 'recorded', outcome: result.outcome as CustodyRerunReceipt['outcome'], run_id: result.run_id,
        completed_at: result.completed_at, statement_id: result.statement_id as string | undefined,
        scope_mode: result.scope_mode as string | undefined, journal_position: result.journal_position as string | undefined, transactions }
    } else if (!['not_compared', 'pending', 'unknown'].some(value => value === result.state) ||
        (result.state === 'pending' && !result.statement_id) || (result.state === 'unknown' && result.statement_id !== undefined) ||
        ['run_id', 'outcome', 'completed_at', 'scope_mode', 'journal_position'].some(key => result[key] !== undefined)) throw invalid()
    return { ...scope, state: result.state as 'not_compared' | 'pending' | 'unknown',
      statement_id: result.statement_id as string | undefined, reason: result.reason as 'journal_changed' | undefined }
  },
  async queue(signal?: AbortSignal): Promise<{ breaks: CustodyBreak[]; rerunActor?: string }> {
    const body = await api.get<{ breaks: unknown; count: unknown; actions_enabled?: unknown; rerun_enabled?: unknown }>('/api/v1/custody/breaks', { signal })
    const rows = body?.breaks === null ? [] : body?.breaks
    if (!Array.isArray(rows) || body.count !== rows.length) throw new Error('Invalid custody response.')
    if (body.actions_enabled !== undefined && typeof body.actions_enabled !== 'boolean') throw new Error('Invalid custody response.')
    if (body.rerun_enabled !== undefined && typeof body.rerun_enabled !== 'boolean') throw new Error('Invalid custody response.')
    const breaks = rows.map((value) => {
      const row = parseBreak(value)
      return { ...row, actions_enabled: body.actions_enabled === true && row.values_state === 'exact' && revision(row.revision) && BigInt(row.revision) < 9223372036854775807n && statuses.slice(0, 3).includes(row.status) }
    })
    if (body.rerun_enabled === true || breaks.some(row => row.actions_enabled)) {
      let canAct = false
      let rerunActor: string | undefined
      try {
        const identity = await auth.me(signal)
        const grants = await users.gatewayPermissions(identity, signal)
        canAct = grants.routes.some(r => r.Capability === 'fund' && r.Pattern === 'POST /v1/custody/breaks/{id}/actions')
        if (body.rerun_enabled === true && grants.routes.some(r => r.Capability === 'custody_rerun' && r.Pattern === 'POST /v1/portfolios/{id}/custody/reruns')) rerunActor = identity.subject
      } catch { signal?.throwIfAborted() }
      if (!canAct) for (const row of breaks) row.actions_enabled = false
      return { breaks, rerunActor }
    }
    return { breaks }
  },
  async rerun(request: CustodyRerunRequest, actor: string, signal?: AbortSignal): Promise<CustodyRerunReceipt> {
    const { portfolio_id, ...command } = request
    const receipt = await api.post<CustodyRerunReceipt>('/api/v1/portfolios/' + encodeURIComponent(portfolio_id) + '/custody/reruns', command, 200, signal)
    if (!receipt || receipt.request_id !== request.request_id || receipt.portfolio_id !== portfolio_id ||
        receipt.custodian_id !== request.custodian_id || receipt.business_date !== request.business_date || receipt.actor !== actor ||
        !nonempty(receipt.run_id) || !Number.isFinite(Date.parse(receipt.completed_at)) ||
        !['clean', 'breaks', 'no_statement', 'failed'].includes(receipt.outcome) ||
        (receipt.failure_code !== undefined && (receipt.outcome !== 'failed' || !['historical_lifecycle', 'comparison_failed'].includes(receipt.failure_code))) ||
        (['clean', 'breaks'].includes(receipt.outcome) && !nonempty(receipt.statement_id)) ||
        (receipt.outcome === 'no_statement' && receipt.statement_id !== undefined) ||
        (receipt.scope_mode !== undefined && !nonempty(receipt.scope_mode)) ||
        (receipt.journal_position !== undefined && (typeof receipt.journal_position !== 'string' || !/^(0|[1-9]\d{0,18})$/.test(receipt.journal_position) || BigInt(receipt.journal_position) > 9223372036854775807n))) throw new Error('Comparison receipt could not be verified.')
    return receipt
  },
  async act(row: CustodyBreak, decision: CustodyDecision, signal?: AbortSignal): Promise<CustodyEvidence> {
    if (!row.actions_enabled || row.values_state !== 'exact' || !revision(row.revision) || BigInt(row.revision) >= 9223372036854775807n || decision.expected_revision !== row.revision || !decision.request_id || decision.review_contract !== 'exact-v1' || !['claim', 'explain'].includes(decision.action) || (decision.action === 'explain' && !decision.explanation?.trim())) throw new Error('Invalid custody review.')
    const value = await api.post<CustodyEvidence>(`/api/v1/custody/breaks/${encodeURIComponent(row.break_id)}/actions`, decision, undefined, signal)
    const state = (s: ActionState) => s && statuses.includes(s.status) && typeof s.assignee === 'string' && typeof s.explanation === 'string' && revision(s.revision)
    if (!value || value.request_id !== decision.request_id || value.break_id !== row.break_id || value.action !== decision.action || typeof value.actor !== 'string' || !value.actor.trim() || !Number.isFinite(Date.parse(value.recorded_at)) || !state(value.before) || !state(value.after) || value.before.revision !== row.revision || BigInt(value.after.revision) !== BigInt(row.revision) + 1n || value.before.status !== row.status || value.before.assignee !== (row.assignee || '') || value.before.explanation !== (row.explanation || '') || !value.values || value.values.ibor !== row.ibor || value.values.custodian !== row.custodian || value.values.difference !== row.difference) throw new Error('Custody action evidence could not be verified.')
    const { before, after } = value
    const matchesDecision = decision.action === 'claim'
      ? after.status === 'assigned' && after.assignee === value.actor && after.explanation === before.explanation
      : after.status === 'explained' && after.explanation === decision.explanation && after.assignee === before.assignee
    if (!matchesDecision) throw new Error('Custody action evidence could not be verified.')
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
