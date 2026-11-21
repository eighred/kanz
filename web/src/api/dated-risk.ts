import { api } from './client'
import { formatDecimal, type Decimal } from './decimal'
import { checkWireDecimal } from './financial-wire'
import type { QualityFlag } from './risk'
import { isTimestamp } from './timestamp'

export const lossMeasures = [
  { key: 'VaR99', label: 'Value at risk' },
  { key: 'ES99', label: 'Expected shortfall' },
] as const

interface LossMeasure {
  name: string
  value?: Decimal
  coverage?: {
    contributed: number
    excluded_count: number
    exclusions?: { instrument_id: string; reason: string }[]
  }
  provenance?: {
    method: string
    model_id?: string
    input_manifest_digest: string
    params?: Record<string, string>
  }
}

interface RetainedEvaluation {
  base_currency: string
  code_revision: string
  input_manifest_digest: string
}

export interface DatedRiskResponse {
  portfolio_id: string
  as_of: string
  set: { portfolio_id: string; as_of: string; measures?: LossMeasure[] }
  quality_flags?: QualityFlag[]
  retained_evaluation?: RetainedEvaluation
  source_position?: { topic: string; partition: number; offset: string }
}

// Compare UTC timestamps at wire precision, including observations in the same millisecond.
function utcKey(value: string) {
  if (!isTimestamp(value) || !value.endsWith('Z') || value.startsWith('0000')) {
    throw new Error('Enter a UTC timestamp ending in Z.')
  }
  return value.slice(0, 19) + value.slice(19, -1).replace('.', '').padEnd(9, '0')
}

export async function readDatedRisk(id: string, cutoff: string, signal: AbortSignal) {
  const requested = utcKey(cutoff)
  const query = new URLSearchParams({ as_of: cutoff })
  for (const measure of lossMeasures) query.append('measure', measure.key)
  const response = await api.get<DatedRiskResponse>(
    `/api/v1/portfolios/${encodeURIComponent(id)}/measures?${query}`, { signal, expectedStatus: 200 },
  )
  if (response?.portfolio_id !== id || response.set?.portfolio_id !== id ||
      !isTimestamp(response.as_of) || !isTimestamp(response.set.as_of) ||
      utcKey(response.as_of) !== utcKey(response.set.as_of) || utcKey(response.as_of) > requested) {
    throw new Error('The returned evaluation does not match this portfolio and cutoff.')
  }
  const names = new Set<string>()
  for (const row of response.set.measures ?? []) {
    if (!lossMeasures.some(m => m.key === row.name) || names.has(row.name)) {
      throw new Error('The returned measure set does not match the request.')
    }
    names.add(row.name)
    checkWireDecimal(row.value)
  }
  return response
}

export function reportedLoss(row: LossMeasure | undefined, source?: RetainedEvaluation): string {
  if (!row) return 'Measure unavailable'
  if (row.provenance?.method !== 'historical_simulation') {
    return row.provenance?.method?.includes('placeholder') ? 'Placeholder model' : 'Historical model unavailable'
  }
  const coverage = row.coverage
  if (!coverage || !Number.isSafeInteger(coverage.contributed) || coverage.contributed < 0 ||
      !Number.isSafeInteger(coverage.excluded_count) || coverage.excluded_count < 0) return 'Coverage unknown'
  if (!coverage.contributed) return 'No contributing positions'
  if (coverage.exclusions?.some(exclusion => !exclusion.instrument_id)) return 'Evaluation unavailable'
  if (!source?.base_currency?.trim()) return 'Historical currency unavailable'
  if (!source.code_revision?.trim() || !/^[0-9a-f]{64}$/.test(source.input_manifest_digest) ||
      row.provenance.input_manifest_digest !== source.input_manifest_digest) return 'Unverified measure origin'
  const value = formatDecimal(row.value)
  return value === null ? 'Amount unavailable' : `${value} ${source.base_currency}`
}
