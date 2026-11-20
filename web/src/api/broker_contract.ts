// Code generated from tv_sync account reads by go generate. DO NOT EDIT.

export const brokerAccount = {
  "id": 'text',
  "name": 'text',
  "currency": 'text',
} as const

export const brokerState = {
  "id": 'text',
  "currency": 'text',
  "balance": 'decimal',
  "equity": 'decimal',
  "realizedPl": 'decimal',
  "unrealizedPl": 'decimal',
  "openPositions": 'count',
} as const

export const brokerPosition = {
  "instrument": 'text',
  "side": 'text',
  "qty": 'decimal',
  "avgPrice": 'decimal',
  "realizedPl": 'decimal',
  "unrealizedPl": 'decimal?',
} as const

export const brokerOrder = {
  "id": 'text',
  "instrument": 'text',
  "venue": 'text?',
  "side": 'text',
  "type": 'text',
  "status": 'text',
  "qty": 'decimal',
  "filledQty": 'decimal',
  "leavesQty": 'decimal',
  "avgFillPrice": 'decimal?',
  "limitPrice": 'decimal?',
  "updatedAt": 'timestamp',
  "parentOrderId": 'text?',
} as const

export const brokerExecution = {
  "id": 'text',
  "orderId": 'text',
  "instrument": 'text',
  "venue": 'text',
  "side": 'text',
  "qty": 'decimal',
  "price": 'decimal',
  "fee": 'decimal?',
  "time": 'timestamp',
} as const

export const brokerReads = {
  accounts: { path: "/broker/accounts", fields: brokerAccount, list: "accounts" },
  state: { path: "/broker/accounts/{id}/state", fields: brokerState, query: ["as_of"] },
  positions: { path: "/broker/accounts/{id}/positions", fields: brokerPosition, query: ["as_of"], list: "positions" },
  orders: { path: "/broker/accounts/{id}/orders", fields: brokerOrder, query: ["as_of"], list: "orders", horizon: "retainedFrom" },
  executions: { path: "/broker/accounts/{id}/executions", fields: brokerExecution, query: ["as_of","instrument"], list: "executions", horizon: "retainedFrom" },
} as const
