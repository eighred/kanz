import { api, ApiError } from './client'

export interface AuditEvent {
  event_id: string
  correlation_id: string
  event_type: string
  kind: string
  occurred_at: string
  source: string
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
    return rows.map((row: unknown) => {
      if (!row || typeof row !== 'object') throw new Error('Invalid audit event.')
      const r = row as Record<string, unknown>
      const keys = ['event_id', 'correlation_id', 'event_type', 'kind', 'occurred_at', 'source'] as const
      if (keys.some((key) => typeof r[key] !== 'string') || !r.event_id ||
          !Number.isFinite(Date.parse(r.occurred_at as string))) throw new Error('Invalid audit event.')
      // Explicit projection: do not render arbitrary event attributes or payloads.
      return Object.fromEntries(keys.map((key) => [key, r[key]])) as unknown as AuditEvent
    })
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
