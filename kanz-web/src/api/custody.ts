import { api, ApiError } from './client'

export interface CustodyBreak {
  break_id: string; kind: string; key: string; ibor: string; custodian: string; difference: string
  status: string; assignee?: string; explanation?: string; first_seen_at: string
  last_seen_at: string; status_changed_at: string; age_seconds: number
}

function parseBreak(value: unknown): CustodyBreak {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Invalid custody response.')
  const row = value as Record<string, unknown>
  const required = ['break_id', 'kind', 'key', 'ibor', 'custodian', 'difference', 'status', 'first_seen_at', 'last_seen_at', 'status_changed_at']
  if (required.some((key) => typeof row[key] !== 'string' || row[key] === '')) throw new Error('Invalid custody response.')
  if (['assignee', 'explanation'].some((key) => row[key] !== undefined && typeof row[key] !== 'string')) throw new Error('Invalid custody response.')
  if (['first_seen_at', 'last_seen_at', 'status_changed_at'].some((key) => !Number.isFinite(Date.parse(row[key] as string)))) throw new Error('Invalid custody response.')
  if (!Number.isSafeInteger(row.age_seconds) || (row.age_seconds as number) < 0) throw new Error('Invalid custody response.')
  return row as unknown as CustodyBreak
}

export const custody = {
  async breaks(): Promise<CustodyBreak[]> {
    const body = await api.get<{ breaks: unknown; count: unknown }>('/api/v1/custody/breaks')
    const rows = body?.breaks === null ? [] : body?.breaks
    if (!Array.isArray(rows) || body.count !== rows.length) throw new Error('Invalid custody response.')
    return rows.map(parseBreak)
  },
}

export function describeCustody(error: unknown): string {
  if (error instanceof ApiError && error.status === 403) return 'Your account is not permitted to read the custody-break queue.'
  if (error instanceof ApiError && error.status === 404) return 'Custody reconciliation is not enabled in this deployment.'
  if (error instanceof ApiError && error.status === 503) return 'Custody reconciliation is currently unavailable.'
  return 'Custody-break data could not be verified.'
}
