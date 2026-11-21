// Code generated from tv_sync projection DTOs by go generate. DO NOT EDIT.

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
  "feeCurrency": 'text?',
  "baseCurrency": 'text?',
  "quoteCurrency": 'text?',
  "inventoryChange": 'decimal?',
  "cashChange": 'decimal?',
  "time": 'timestamp',
} as const
