import { api, ApiError } from './client'
import { brokerAccount, brokerExecution, brokerOrder, brokerPosition, brokerState, brokerReads } from './broker_contract'
import { isTimestamp } from './timestamp'

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
  if (!isTimestamp(value)) throw new Error('Invalid broker response.')
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

type DetailRoute = (typeof brokerReads)[Exclude<keyof typeof brokerReads, 'accounts'>]
type AccountQuery = Partial<Record<DetailRoute['query'][number], string>>

function accountPath(route: DetailRoute, id: string, query: AccountQuery): string {
  const path = '/api/v1' + route.path.replace('{id}', () => encodeURIComponent(id))
  const params = new URLSearchParams()
  for (const key of route.query) {
    const value = query[key]
    if (value !== undefined) params.set(key, value)
  }
  return params.size ? path + '?' + params.toString() : path
}

function readList<S extends Fields>(body: Record<string, unknown>, route: { list: string; fields: S }): BrokerRow<S>[] {
  return list(body[route.list], row => readRow(row, route.fields))
}

function window<S extends Fields>(value: unknown, route: { list: string; fields: S; horizon: string }) {
  const body = object(value, route.list)
  return { rows: readList(body, route), retainedFrom: time(body[route.horizon]) }
}

export const broker = {
  async accounts(): Promise<BrokerAccount[]> {
    const route = brokerReads.accounts
    return readList(object(await api.get<unknown>('/api/v1' + route.path), route.list), route)
  },
  async account(id: string, signal?: AbortSignal, query: AccountQuery = {}): Promise<BrokerAccountDetail> {
    if (query.as_of !== undefined && !isTimestamp(query.as_of)) throw new Error('Invalid broker knowledge time.')
    const read = (route: DetailRoute) => api.get<unknown>(accountPath(route, id, query), { signal })
    const [state, positionsBody, ordersBody, executionsBody] = await Promise.all([
      read(brokerReads.state), read(brokerReads.positions), read(brokerReads.orders), read(brokerReads.executions),
    ])
    const positions = readList(object(positionsBody, brokerReads.positions.list), brokerReads.positions)
    const orders = window(ordersBody, brokerReads.orders)
    const executions = window(executionsBody, brokerReads.executions)
    const parsedState = readRow(state, brokerReads.state.fields)
    if (parsedState.id !== id) throw new Error('Broker account identity differs.')
    return {
      state: parsedState, positions,
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
