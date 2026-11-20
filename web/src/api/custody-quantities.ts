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

function invalid(): never { throw new Error('Execution quantity evidence could not be verified.') }

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
