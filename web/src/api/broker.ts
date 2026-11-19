import { api, ApiError } from './client'
import { brokerAccount, brokerExecution, brokerOrder, brokerPosition, brokerState } from './broker_contract'

type OptionalField = 'text?' | 'decimal?' | 'timestamp?'
type Fields = Record<string, 'text' | 'decimal' | 'timestamp' | 'count' | OptionalField>
type BrokerRow<S extends Fields> = {
  [K in keyof S as S[K] extends OptionalField ? never : K]: S[K] extends 'count' ? number : string
} & {
  [K in keyof S as S[K] extends OptionalField ? K : never]?: string
}

export type BrokerAccount = BrokerRow<typeof brokerAccount>
export type BrokerState = BrokerRow<typeof brokerState>
export type BrokerPosition = BrokerRow<typeof brokerPosition>
export type BrokerOrder = BrokerRow<typeof brokerOrder>
export type BrokerExecution = BrokerRow<typeof brokerExecution>
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

function readRow<S extends Fields>(value: unknown, fields: S): BrokerRow<S> {
  const row = object(value, 'broker')
  const result: Record<string, string | number> = {}
  for (const [key, kind] of Object.entries(fields)) {
    const field = row[key]
    if (kind.endsWith('?') && field === undefined) continue
    if (kind === 'count') {
      if (typeof field !== 'number' || !Number.isSafeInteger(field) || field < 0) throw new Error('Invalid broker response.')
    } else {
      if (typeof field !== 'string' || (field === '' && kind !== 'text?')) throw new Error('Invalid broker response.')
      // The projection emits fixed-point text. Preserve all digits without a Number conversion.
      if ((kind === 'decimal' || kind === 'decimal?') && !/^-?[0-9]+(?:\.[0-9]+)?$/.test(field)) {
        throw new Error('Invalid broker response.')
      }
      if (kind === 'timestamp' || kind === 'timestamp?') time(field)
    }
    result[key] = field
  }
  return result as BrokerRow<S>
}

function window<T>(value: unknown, key: string, parse: (row: unknown) => T): { rows: T[]; retainedFrom?: string } {
  const body = object(value, key)
  return { rows: list(body[key], parse), retainedFrom: time(body.retainedFrom) }
}

export const broker = {
  async accounts(): Promise<BrokerAccount[]> {
    const body = object(await api.get<unknown>('/api/v1/broker/accounts'), 'broker accounts')
    return list(body.accounts, row => readRow(row, brokerAccount))
  },
  async account(id: string, signal?: AbortSignal): Promise<BrokerAccountDetail> {
    const base = `/api/v1/broker/accounts/${encodeURIComponent(id)}`
    const [state, positionsBody, ordersBody, executionsBody] = await Promise.all([
      api.get<unknown>(`${base}/state`, { signal }), api.get<unknown>(`${base}/positions`, { signal }),
      api.get<unknown>(`${base}/orders`, { signal }), api.get<unknown>(`${base}/executions`, { signal }),
    ])
    const positions = object(positionsBody, 'positions')
    const orders = window(ordersBody, 'orders', row => readRow(row, brokerOrder))
    const executions = window(executionsBody, 'executions', row => readRow(row, brokerExecution))
    const parsedState = readRow(state, brokerState)
    if (parsedState.id !== id) throw new Error('Broker account identity differs.')
    return {
      state: parsedState, positions: list(positions.positions, row => readRow(row, brokerPosition)),
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
