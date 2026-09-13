import { api, ApiError } from './client'

export interface AuditEvent {
  event_id: string
  correlation_id: string
  event_type: string
  kind: string
  occurred_at: string
  source: string
}

export interface AuditRecord extends AuditEvent {
  causation_id: string
  domain: string
  event_class: string
  recorded_at: string
  schema_ref: string
  prev_hash: string
  hash: string
}

export interface AuditLineage { target: AuditRecord; ancestry: AuditRecord[] }

const eventKeys = ['event_id', 'correlation_id', 'event_type', 'kind', 'occurred_at', 'source'] as const
const recordKeys = [...eventKeys, 'causation_id', 'domain', 'event_class', 'recorded_at', 'schema_ref', 'prev_hash', 'hash'] as const

function projected<T>(value: unknown, keys: readonly string[], dates: readonly string[]): T {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Invalid audit event.')
  const row = value as Record<string, unknown>
  if (keys.some((key) => typeof row[key] !== 'string') || !row.event_id ||
      dates.some((key) => !Number.isFinite(Date.parse(row[key] as string)))) throw new Error('Invalid audit event.')
  return Object.fromEntries(keys.map((key) => [key, row[key]])) as T
}

export const audit = {
  async events(correlation: string, eventType: string): Promise<AuditEvent[]> {
    const query = new URLSearchParams({ limit: '100' })
    if (correlation.trim()) query.set('correlation', correlation.trim())
    if (eventType.trim()) query.set('event_type', eventType.trim())
    const body = await api.get<{ records: unknown; count: unknown }>(`/api/audit/events?${query}`)
    // Go encodes an empty nil slice as null. Other malformed results cannot
    // become an empty audit trail and falsely suggest nothing happened.
    const rows = body?.records === null && body.count === 0 ? [] : body?.records
    if (!Array.isArray(rows) || body.count !== rows.length || rows.length > 100) {
      throw new Error('The audit service returned an invalid result. No events are shown.')
    }
    return rows.map((row: unknown) => projected<AuditEvent>(row, eventKeys, ['occurred_at']))
  },
  async event(id: string): Promise<AuditRecord> {
    const record = projected<AuditRecord>(
      await api.get<unknown>(`/api/v1/audit/events/${encodeURIComponent(id)}`),
      recordKeys, ['occurred_at', 'recorded_at'],
    )
    if (record.event_id !== id) throw new Error('Invalid audit event.')
    return record
  },
  async lineage(id: string): Promise<AuditLineage> {
    const body = await api.get<{ target: unknown; ancestry: unknown }>(`/api/v1/audit/lineage/${encodeURIComponent(id)}`)
    const target = projected<AuditRecord>(body?.target, recordKeys, ['occurred_at', 'recorded_at'])
    if (!Array.isArray(body?.ancestry)) throw new Error('Invalid audit lineage.')
    const ancestry = body.ancestry.map((row) => projected<AuditRecord>(row, recordKeys, ['occurred_at', 'recorded_at']))
    if (target.event_id !== id || ancestry.at(-1)?.event_id !== id) throw new Error('Invalid audit lineage.')
    // The server also returns a recursive correlation tree. The detail page uses
    // the bounded direct ancestry and does not render arbitrary record payloads.
    return { target, ancestry }
  },
}

export function describeAudit(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.status === 401) return 'Your session expired. Sign in again.'
    if (error.status === 403) return 'Your account does not have permission to read audit events.'
    if (error.status === 404 || error.status === 503) return 'Audit search is unavailable in this deployment.'
    return 'Audit search failed. Try again.'
  }
  return 'Audit results could not be verified. Try again.'
}
