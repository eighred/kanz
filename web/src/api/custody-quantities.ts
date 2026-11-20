import type { Decimal } from './decimal'

// The signed input and retained accounting completion are each bounded at 1 MiB.
const receiptByteLimit = 1 << 20

interface QuantityDifference {
  external_ref: string; instrument_id: string
  ibor: Required<Decimal>; custodian: Required<Decimal>; difference: Required<Decimal>
}

export interface ExecutionQuantities {
  compared: number; unassessed: number; differences: QuantityDifference[]
}

function invalid(): never { throw new Error('Execution comparison evidence could not be verified.') }

function record(value: unknown): Record<string, unknown> {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) return invalid()
  return value as Record<string, unknown>
}

function measured(value: unknown): Required<Decimal> {
  const { coefficient, exponent } = record(value)
  if (typeof coefficient !== 'string' || coefficient.length > 20 || !/^(?:0|-?[1-9]\d*)$/.test(coefficient) ||
      typeof exponent !== 'number' || !Number.isInteger(exponent) || Math.abs(exponent) > 60) return invalid()
  // dec.ToProtoExact folds every nonzero trailing power of ten into the exponent.
  const amount = BigInt(coefficient)
  if (BigInt.asIntN(64, amount) !== amount || (amount === 0n ? exponent !== 0 : coefficient.endsWith('0'))) return invalid()
  return { coefficient, exponent }
}

export interface ReportedExecutionFigures {
  basis: 'signed_custodian_statement'; signed_statement_sha256: string
  rows: { external_ref: string; instrument_id: string; cash_currency: string; reported_price?: string; reported_cash?: string }[]
}

export function readReportedFigures(value: unknown, digest: string, referenceCount: string): ReportedExecutionFigures {
  const section = record(value)
  if (section.basis !== 'signed_custodian_statement' || section.signed_statement_sha256 !== digest || !Array.isArray(section.rows) ||
      section.rows.length > 4096 || String(section.rows.length) !== referenceCount) return invalid()
  const utf8 = new TextEncoder()
  let previous = new Uint8Array()
  const decimalText = (amount: unknown) => {
    if (amount === undefined) return undefined
    if (typeof amount !== 'string' || amount.length > 256 || !/^-?(?:0|[1-9]\d*)(?:\.\d*[1-9])?$/.test(amount) || amount === '-0') return invalid()
    return amount
  }
  const rows = section.rows.map(item => {
    const { external_ref, instrument_id, cash_currency, reported_price, reported_cash } = record(item)
    const id = /^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$/
    if (typeof external_ref !== 'string' || !external_ref || external_ref.length > receiptByteLimit || /^\p{White_Space}|\p{White_Space}$|\p{Cs}/u.test(external_ref) ||
        typeof instrument_id !== 'string' || !id.test(instrument_id) || typeof cash_currency !== 'string' || !id.test(cash_currency)) return invalid()
    const ref = utf8.encode(external_ref)
    if (!precedes(previous, ref)) return invalid()
    previous = ref
    return { external_ref, instrument_id, cash_currency, reported_price: decimalText(reported_price), reported_cash: decimalText(reported_cash) }
  })
  return { basis: 'signed_custodian_statement', signed_statement_sha256: digest, rows }
}

export interface QuantityResolution {
  break_id: string; detection_run_id: string; entry_id: string; external_ref: string; instrument_id: string
  agreed_quantity: Required<Decimal>
}

export function validCustodyRunID(value: unknown): value is string {
  return typeof value === 'string' && value.length > 0 && value.length <= 512 && value.trim() === value &&
    !/\p{Cc}|\p{Cs}/u.test(value) && new TextEncoder().encode(value).length <= 512
}

export function readQuantityResolutions(value: unknown): QuantityResolution[] {
  if (!Array.isArray(value) || value.length === 0 || value.length > receiptByteLimit / 64) return invalid()
  let previous = ''
  let bytes = 0
  const utf8 = new TextEncoder()
  return value.map(item => {
    const { break_id, detection_run_id, entry_id, external_ref, instrument_id, agreed_quantity } = record(item)
    if (typeof break_id !== 'string' || !/^q1:[a-f0-9]{64}$/.test(break_id) || previous >= break_id ||
        !validCustodyRunID(detection_run_id) || typeof entry_id !== 'string' || !entry_id || /\p{Cs}/u.test(entry_id) ||
        typeof external_ref !== 'string' || !external_ref || /^\p{White_Space}|\p{White_Space}$|\p{Cs}/u.test(external_ref) ||
        typeof instrument_id !== 'string' || !/^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$/.test(instrument_id)) return invalid()
    bytes += break_id.length + utf8.encode(detection_run_id).length + utf8.encode(entry_id).length + utf8.encode(external_ref).length + instrument_id.length
    if (bytes > receiptByteLimit) return invalid()
    previous = break_id
    return { break_id, detection_run_id, entry_id, external_ref, instrument_id, agreed_quantity: measured(agreed_quantity) }
  })
}

function count(value: unknown): number {
  if (typeof value !== 'number' || !Number.isInteger(value) || value < 0 || value > 0xffffffff) return invalid()
  return value
}

// The accounting writer sorts opaque references by their UTF-8 bytes.
function precedes(previous: Uint8Array, next: Uint8Array): boolean {
  for (let i = 0; i < Math.min(previous.length, next.length); i++) {
    if (previous[i] !== next[i]) return previous[i]! < next[i]!
  }
  return previous.length < next.length
}

export function readExecutionQuantities(value: unknown, statementReferences: string, bookReferences: string): ExecutionQuantities {
  const section = record(value)
  const compared = count(section.compared)
  const unassessed = count(section.unassessed)
  const matched = BigInt(compared + unassessed)
  if (compared + unassessed > receiptByteLimit || matched > BigInt(statementReferences) || matched > BigInt(bookReferences) ||
      !Array.isArray(section.differences) || section.differences.length > compared) return invalid()
  let previous = new Uint8Array()
  let identifierBytes = 0
  const utf8 = new TextEncoder()
  const differences = section.differences.map((item: unknown) => {
    const { external_ref, instrument_id, ibor, custodian, difference } = record(item)
    if (typeof external_ref !== 'string' || !external_ref.length || external_ref.length > receiptByteLimit ||
        /^\p{White_Space}|\p{White_Space}$|\p{Cs}/u.test(external_ref) || typeof instrument_id !== 'string' || !/^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$/.test(instrument_id)) return invalid()
    const ref = utf8.encode(external_ref)
    identifierBytes += ref.length + instrument_id.length
    if (identifierBytes > receiptByteLimit || !precedes(previous, ref)) return invalid()
    previous = ref
    const delta = measured(difference)
    if (delta.coefficient === '0') return invalid()
    return { external_ref, instrument_id, ibor: measured(ibor), custodian: measured(custodian), difference: delta }
  })
  return { compared, unassessed, differences }
}
// All these figures are sealed by accounting on the original comparison.
export const economicsMeasures = ['quantity', 'price', 'gross_cash', 'commission', 'net_cash', 'settled_cash'] as const
export type EconomicsMeasure = typeof economicsMeasures[number]
export type EconomicsStatus = 'equal' | 'mismatch' | 'unknown'
type EconomicsFigures = Record<EconomicsMeasure, Required<Decimal>>
type EconomicsRow = {
  external_ref: string; instrument_id: string; convention: string; currency_code: string; entry_id: string
  book_settlement_date: string; custodian_settlement_date: string; reason: string
} & (
  | { status: 'equal' | 'mismatch'; book: EconomicsFigures; custodian: EconomicsFigures; difference: EconomicsFigures }
  | { status: 'unknown'; book?: never; custodian?: never; difference?: never }
)
export interface ExecutionEconomics { rows: EconomicsRow[]; unassessed_reason: string }

export function readExecutionEconomics(value: unknown, referenceCount: string, summary: EconomicsStatus, businessDate: string): ExecutionEconomics {
  const section = record(value)
  if (!Array.isArray(section.rows) || section.rows.length > 128 || typeof section.unassessed_reason !== 'string') return invalid()
  if (section.unassessed_reason) {
    if (section.unassessed_reason !== 'row_limit' || BigInt(referenceCount) <= 128n || section.rows.length || summary !== 'unknown') return invalid()
    return { rows: [], unassessed_reason: section.unassessed_reason }
  }
  if (String(section.rows.length) !== referenceCount) return invalid()
  const utf8 = new TextEncoder()
  let previous = new Uint8Array()
  let bytes = 0
  function text(field: unknown): string {
    if (typeof field !== 'string' || field.length > receiptByteLimit || /\p{Cs}/u.test(field)) return invalid()
    bytes += utf8.encode(field).length
    if (bytes > receiptByteLimit) return invalid()
    return field
  }
  function figures(value: unknown): EconomicsFigures {
    const f = record(value)
    return { quantity: measured(f.quantity), price: measured(f.price), gross_cash: measured(f.gross_cash),
      commission: measured(f.commission), net_cash: measured(f.net_cash), settled_cash: measured(f.settled_cash) }
  }
  const rows: EconomicsRow[] = section.rows.map(item => {
    const row = record(item)
    const external_ref = text(row.external_ref)
    const ref = utf8.encode(external_ref)
    if (!precedes(previous, ref) || /^\p{White_Space}|\p{White_Space}$/u.test(external_ref)) return invalid()
    previous = ref
    const fields = { external_ref, instrument_id: text(row.instrument_id), convention: text(row.convention),
      currency_code: text(row.currency_code), entry_id: text(row.entry_id), reason: text(row.reason),
      book_settlement_date: text(row.book_settlement_date), custodian_settlement_date: text(row.custodian_settlement_date) }
    if (row.status === 'unknown') {
      if (!fields.reason || fields.entry_id || fields.book_settlement_date || fields.custodian_settlement_date || row.book !== undefined || row.custodian !== undefined || row.difference !== undefined) return invalid()
      return { ...fields, status: 'unknown' }
    }
    if ((row.status !== 'equal' && row.status !== 'mismatch') || fields.reason || !fields.instrument_id || !fields.currency_code || !fields.entry_id ||
        fields.convention !== 'unit_price_net_cash_same_currency_commission_v1' ||
        fields.book_settlement_date !== businessDate || fields.custodian_settlement_date !== businessDate) return invalid()
    const book = figures(row.book), custodian = figures(row.custodian), difference = figures(row.difference)
    if ((row.status === 'equal') !== economicsMeasures.every(k => difference[k].coefficient === '0')) return invalid()
    return { ...fields, status: row.status, book, custodian, difference }
  })
  const expected = rows.some(r => r.status === 'mismatch') ? 'mismatch' : rows.length && rows.every(r => r.status === 'equal') ? 'equal' : 'unknown'
  if (expected !== summary) return invalid()
  return { rows, unassessed_reason: '' }
}
