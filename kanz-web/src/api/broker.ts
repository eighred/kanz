import { api, ApiError } from './client'

export interface BrokerAccount { id: string; name: string; currency: string }
export interface BrokerState {
  id: string; currency: string; balance: string; equity: string
  realizedPl: string; unrealizedPl: string; openPositions: number
}
export interface BrokerPosition {
  instrument: string; side: string; qty: string; avgPrice: string
  realizedPl: string; unrealizedPl?: string
}
export interface BrokerOrder {
  id: string; instrument: string; venue?: string; side: string; type: string; status: string
  qty: string; filledQty: string; leavesQty: string; avgFillPrice?: string; limitPrice?: string
  updatedAt: string; parentOrderId?: string
}
export interface BrokerExecution {
  id: string; orderId: string; instrument: string; venue: string; side: string
  qty: string; price: string; fee?: string; time: string
}
export interface BrokerAccountDetail {
  state: BrokerState
  positions: BrokerPosition[]
  orders: BrokerOrder[]
  executions: BrokerExecution[]
  ordersRetainedFrom?: string
  executionsRetainedFrom?: string
}

function object(value: unknown, label: string): Record<string, unknown> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error(`Invalid ${label} response.`)
  return value as Record<string, unknown>
}

function strings(row: Record<string, unknown>, required: string[], optional: string[] = []): void {
  if (required.some((key) => typeof row[key] !== 'string' || row[key] === '')) throw new Error('Invalid broker response.')
  if (optional.some((key) => row[key] !== undefined && typeof row[key] !== 'string')) throw new Error('Invalid broker response.')
}

function list<T>(value: unknown, parse: (row: unknown) => T): T[] {
  if (value === null) return [] // Go nil slices are JSON null.
  if (!Array.isArray(value)) throw new Error('Invalid broker response.')
  return value.map(parse)
}

function time(value: unknown): string | undefined {
  if (value === undefined) return undefined
  if (typeof value !== 'string' || !Number.isFinite(Date.parse(value))) throw new Error('Invalid broker response.')
  return value
}

function parseAccount(value: unknown): BrokerAccount {
  const row = object(value, 'broker account')
  strings(row, ['id', 'name', 'currency'])
  return { id: row.id as string, name: row.name as string, currency: row.currency as string }
}

function parseState(value: unknown): BrokerState {
  const row = object(value, 'broker state')
  strings(row, ['id', 'currency', 'balance', 'equity', 'realizedPl', 'unrealizedPl'])
  if (!Number.isSafeInteger(row.openPositions) || (row.openPositions as number) < 0) throw new Error('Invalid broker response.')
  return row as unknown as BrokerState
}

function parsePosition(value: unknown): BrokerPosition {
  const row = object(value, 'broker position')
  strings(row, ['instrument', 'side', 'qty', 'avgPrice', 'realizedPl'], ['unrealizedPl'])
  return row as unknown as BrokerPosition
}

function parseOrder(value: unknown): BrokerOrder {
  const row = object(value, 'broker order')
  strings(row, ['id', 'instrument', 'side', 'type', 'status', 'qty', 'filledQty', 'leavesQty', 'updatedAt'], ['venue', 'avgFillPrice', 'limitPrice', 'parentOrderId'])
  time(row.updatedAt)
  return row as unknown as BrokerOrder
}

function parseExecution(value: unknown): BrokerExecution {
  const row = object(value, 'broker execution')
  strings(row, ['id', 'orderId', 'instrument', 'venue', 'side', 'qty', 'price', 'time'], ['fee'])
  time(row.time)
  return row as unknown as BrokerExecution
}

function window<T>(value: unknown, key: string, parse: (row: unknown) => T): { rows: T[]; retainedFrom?: string } {
  const body = object(value, key)
  return { rows: list(body[key], parse), retainedFrom: time(body.retainedFrom) }
}

export const broker = {
  async accounts(): Promise<BrokerAccount[]> {
    const body = object(await api.get<unknown>('/api/v1/broker/accounts'), 'broker accounts')
    return list(body.accounts, parseAccount)
  },
  async account(id: string): Promise<BrokerAccountDetail> {
    const base = `/api/v1/broker/accounts/${encodeURIComponent(id)}`
    const [state, positionsBody, ordersBody, executionsBody] = await Promise.all([
      api.get<unknown>(`${base}/state`), api.get<unknown>(`${base}/positions`),
      api.get<unknown>(`${base}/orders`), api.get<unknown>(`${base}/executions`),
    ])
    const positions = object(positionsBody, 'positions')
    const orders = window(ordersBody, 'orders', parseOrder)
    const executions = window(executionsBody, 'executions', parseExecution)
    return {
      state: parseState(state), positions: list(positions.positions, parsePosition),
      orders: orders.rows, executions: executions.rows,
      ordersRetainedFrom: orders.retainedFrom, executionsRetainedFrom: executions.retainedFrom,
    }
  },
}

export function describeBroker(error: unknown): string {
  if (error instanceof ApiError && error.status === 403) return 'Your account is not permitted to read broker accounts.'
  if (error instanceof ApiError && error.status === 404) return 'This broker account or broker read surface is unavailable.'
  if (error instanceof ApiError && error.status === 503) return 'Broker account data is currently unavailable.'
  return 'Broker account data could not be verified.'
}
