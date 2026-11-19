import { api } from './client'
import { isTimestamp } from './timestamp'

export interface CashReceipt {
  movement_id: string
  command_digest: string
  actor: string
  accepted_at: string
  status: 'accepted' | 'journal_recorded'
}

function commandText(value: unknown): value is string {
  return typeof value === 'string' && value !== '' && value.trim() === value &&
    !value.includes('\0') && new TextEncoder().encode(value).length <= 256
}

export function isCashIdentifier(value: string): boolean {
  // The browser resolves these path segments before sending a request.
  return commandText(value) && value !== '.' && value !== '..'
}

export async function readCashReceipt(portfolio: string, movement: string, signal: AbortSignal): Promise<CashReceipt> {
  if (!isCashIdentifier(portfolio) || !isCashIdentifier(movement)) throw new Error('Invalid cash receipt identifiers.')
  const path = `/api/v1/portfolios/${encodeURIComponent(portfolio)}/cash-movements/${encodeURIComponent(movement)}`
  const body = await api.get<unknown>(path, { signal, expectedStatus: 200 })
  if (!body || typeof body !== 'object') throw new Error('Invalid cash receipt.')
  const row = body as Record<string, unknown>
  if (row.movement_id !== movement || !commandText(row.actor) || !isTimestamp(row.accepted_at) ||
      typeof row.command_digest !== 'string' || !/^[0-9a-f]{64}$/.test(row.command_digest) ||
      (row.status !== 'accepted' && row.status !== 'journal_recorded')) throw new Error('Invalid cash receipt.')
  return { movement_id: movement, command_digest: row.command_digest, actor: row.actor,
    accepted_at: row.accepted_at, status: row.status }
}
