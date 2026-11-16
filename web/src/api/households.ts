import { api, ApiError } from './client'

export interface HouseholdDrift {
  evaluated: boolean
  outcome: 'in_band' | 'breached' | 'no_profile' | 'no_model' | 'catalogue_unarmed' | 'ambiguous_model' | 'invalid_model' | 'invalid_valuation' | 'undefined_weights'
  model_id?: string
  tolerance?: string
  max?: string
  total?: string
  by_instrument?: Record<string, string>
  breached?: boolean
}
export interface Household {
  arithmetic_version: 2
  household_id: string
  total_value: string
  cash: string
  holdings: Record<string, string>
  weights: Record<string, string> | null
  asset_class: Record<string, string> | null
  weights_state: 'exact' | 'unavailable'
  currency_code: string
  as_of: string
  recorded_by: string
  source_reason: string
  risk_profile: string
  drift: HouseholdDrift
}
function invalid(): never { throw new Error('Household response could not be verified.') }
function record(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return invalid()
  return value as Record<string, unknown>
}
function text(value: unknown, limit = 256): string {
  if (typeof value !== 'string' || !value.trim() || value.length > limit) return invalid()
  return value
}
// Financial strings never pass through Number, parseFloat, or Intl rounding.
// Ratios such as 1/3 retain their exact fraction instead of a made-up last digit.
function rational(value: unknown): { text: string; numerator: bigint; denominator: bigint } {
  const s = text(value, 400)
  if (!/^-?[0-9]+(?:\.[0-9]+|\/[1-9][0-9]*)?$/.test(s)) return invalid()
  let numerator: bigint, denominator: bigint
  if (s.includes('/')) {
    const parts = s.split('/')
    numerator = BigInt(parts[0]!)
    denominator = BigInt(parts[1]!)
  } else {
    const parts = s.split('.')
    numerator = BigInt(parts.join(''))
    denominator = 10n ** BigInt(parts[1]?.length ?? 0)
  }
  let a = numerator < 0n ? -numerator : numerator, b = denominator
  while (b) { const remainder = a % b; a = b; b = remainder }
  numerator /= a; denominator /= a
  if ((numerator < 0n ? -numerator : numerator).toString(2).length > 512 || denominator.toString(2).length > 512) return invalid()
  return { text: s, numerator, denominator }
}
function amounts(value: unknown, emptyKey = false): Record<string, string> {
  const rows = record(value), entries = Object.entries(rows)
  if (entries.length > 4096) return invalid()
  const result: Record<string, string> = Object.create(null)
  for (const [key, value] of entries) {
    if ((!emptyKey && !key) || key.length > 256) return invalid()
    result[key] = rational(value).text
  }
  return result
}
const outcomes = ['in_band', 'breached', 'no_profile', 'no_model', 'catalogue_unarmed', 'ambiguous_model', 'invalid_model', 'invalid_valuation', 'undefined_weights'] as const
function drift(value: unknown): HouseholdDrift {
  const row = record(value)
  if (typeof row.evaluated !== 'boolean' || !outcomes.includes(row.outcome as typeof outcomes[number])) return invalid()
  const outcome = row.outcome as HouseholdDrift['outcome']
  if (!row.evaluated) {
    if (outcome === 'in_band' || outcome === 'breached' || ['max', 'total', 'tolerance', 'by_instrument', 'breached'].some(key => row[key] !== undefined)) return invalid()
    return { evaluated: false, outcome }
  }
  if ((outcome !== 'in_band' && outcome !== 'breached') || row.breached !== (outcome === 'breached')) return invalid()
  const band = rational(row.tolerance), max = rational(row.max), total = rational(row.total)
  if (band.numerator <= 0n || band.numerator > band.denominator || max.numerator < 0n || total.numerator < 0n) return invalid()
  if ((max.numerator * band.denominator > band.numerator * max.denominator) !== row.breached) return invalid()
  return { evaluated: true, outcome, model_id: text(row.model_id), tolerance: band.text, max: max.text, total: total.text, by_instrument: amounts(row.by_instrument), breached: row.breached as boolean }
}
export function parseHousehold(value: unknown, expectedID: string): Household {
  const row = record(value)
  if (row.arithmetic_version !== 2 || row.household_id !== expectedID || !['exact', 'unavailable'].includes(row.weights_state as string)) return invalid()
  const total = rational(row.total_value), weightsState = row.weights_state as Household['weights_state']
  if (weightsState === 'exact' && total.numerator <= 0n) return invalid()
  if (weightsState === 'unavailable' && (row.weights !== null || row.asset_class !== null)) return invalid()
  const asOf = text(row.as_of, 64)
  if (!Number.isFinite(Date.parse(asOf))) return invalid()
  const evaluated = drift(row.drift)
  if (weightsState === 'unavailable' && evaluated.evaluated) return invalid()
  const holdings = amounts(row.holdings)
  const weights = weightsState === 'exact' ? amounts(row.weights) : null
  if (weights && (Object.keys(weights).length !== Object.keys(holdings).length || Object.keys(holdings).some(id => weights[id] === undefined))) return invalid()
  return {
    arithmetic_version: 2, household_id: text(row.household_id), total_value: total.text, cash: rational(row.cash).text,
    holdings, weights,
    asset_class: weightsState === 'exact' ? amounts(row.asset_class, true) : null, weights_state: weightsState,
    currency_code: text(row.currency_code, 16), as_of: asOf, recorded_by: text(row.recorded_by, 4096), source_reason: text(row.source_reason, 4096),
    risk_profile: text(row.risk_profile, 64), drift: evaluated,
  }
}
export const households = {
  async get(id: string): Promise<Household> {
    text(id)
    return parseHousehold(await api.get<unknown>(`/api/v2/households/${encodeURIComponent(id)}`), id)
  },
}
export function describeHousehold(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.status === 403) return 'Your account is not permitted to read household valuations.'
    if (error.status === 404) return 'This household was not found, or the exact household view is not enabled in this deployment.'
    if (error.status === 503 && error.code === 'legacy_unverified') return 'This household has an unverified legacy valuation. Replay from its exact source is required before financial values can be shown.'
    if (error.status === 503) return 'The exact household valuation service is currently unavailable.'
  }
  return 'Household data could not be verified. No financial values are shown.'
}
export const driftDescriptions: Record<HouseholdDrift['outcome'], string> = {
  in_band: 'Within the published model band', breached: 'Published model band breached',
  no_profile: 'No risk profile supplied', no_model: 'No model published for this profile', catalogue_unarmed: 'Model catalogue is not ready',
  ambiguous_model: 'Multiple models claim this profile', invalid_model: 'Model policy is invalid or uses unverified legacy precision',
  invalid_valuation: 'Exact drift could not be established', undefined_weights: 'Weights are unavailable for this valuation',
}
