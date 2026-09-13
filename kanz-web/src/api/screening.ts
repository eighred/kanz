import { api, ApiError } from './client'

export interface ScreenPosition { instrument_id: string; quantity: string; market_value: string; currency: string }
export interface ScreenInput { portfolio_id: string; base_currency: string; as_of: string; positions: ScreenPosition[]; excluded_sectors: string[]; excluded_issuers: string[] }
export interface ScreenViolation { rule_id: string; message: string; evidence: Record<string, string>; unavailable: boolean }
export interface ScreenResult { portfolio_id: string; evaluated_at: string; status: 'COMPLIANCE_STATUS_PASS' | 'COMPLIANCE_STATUS_BREACH'; violations: ScreenViolation[] }
const exact = /^-?\d+(?:\.\d+)?$/
const timestamp = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$/
function str(v: unknown, max = 256): v is string { return typeof v === 'string' && v.length <= max }
function object(v: unknown): Record<string, unknown> {
  if (!v || typeof v !== 'object' || Array.isArray(v)) throw new Error('Invalid screen response')
  return v as Record<string, unknown>
}
export function validScreenInput(input: ScreenInput): boolean {
  return !!input.portfolio_id.trim() && input.portfolio_id.length <= 256 && !!input.base_currency.trim()
    && input.base_currency.length <= 16 && timestamp.test(input.as_of) && Number.isFinite(Date.parse(input.as_of))
    && input.positions.length <= 4096 && input.excluded_sectors.length + input.excluded_issuers.length > 0
    && [input.excluded_sectors, input.excluded_issuers].every(list => list.length <= 256 && list.every(s => !!s.trim() && s.length <= 256))
    && input.positions.every(p => !!p.instrument_id.trim() && p.instrument_id.length <= 256 && p.currency.length <= 16
      && [p.quantity, p.market_value].every(s => s === '' || (s.length <= 400 && exact.test(s))))
}
const evidenceKeys = new Set(['dimension', 'bucket', 'mode', 'instrument', 'issuer', 'classifier', 'holdings', 'unresolved', 'unresolved_instruments', 'unmarked_holdings', 'unmarked_sample', 'unmarked_reasons'])
export function parseScreenResult(value: unknown, input: ScreenInput): ScreenResult {
  const r = object(value)
  if (r.portfolio_id !== input.portfolio_id || r.mandate_id !== 'esg-exclusion' || r.mandate_version !== '1'
    || !str(r.evaluated_at, 64) || !timestamp.test(r.evaluated_at) || Date.parse(r.evaluated_at) !== Date.parse(input.as_of)
    || !['COMPLIANCE_STATUS_PASS', 'COMPLIANCE_STATUS_BREACH'].includes(String(r.status))
    || !Array.isArray(r.violations) || r.violations.length > 2) throw new Error('Invalid screen response')
  const violations = r.violations.map(raw => {
    const v = object(raw)
    const expected = v.rule_id === 'esg-sector-exclusion' ? 'RULE_TYPE_RESTRICTION' : v.rule_id === 'esg-issuer-exclusion' ? 'RULE_TYPE_ISSUER_EXCLUSION' : null
    if (!expected || v.rule_type !== expected || v.severity !== 'COMPLIANCE_STATUS_BREACH' || !str(v.message, 4096)) throw new Error('Invalid violation')
    if ((v.rule_id === 'esg-sector-exclusion' && !input.excluded_sectors.length) || (v.rule_id === 'esg-issuer-exclusion' && !input.excluded_issuers.length)) throw new Error('Unrequested rule')
    const evidence = object(v.evidence)
    if (Object.entries(evidence).some(([key, item]) => !evidenceKeys.has(key) || !str(item, 16384))) throw new Error('Invalid evidence')
    const unavailable = evidence.classifier === 'unavailable' || evidence.unresolved !== undefined || evidence.unmarked_holdings !== undefined
    return { rule_id: String(v.rule_id), message: v.message, evidence: { ...evidence } as Record<string, string>, unavailable }
  })
  if ((r.status === 'COMPLIANCE_STATUS_PASS') !== (violations.length === 0) || new Set(violations.map(v => v.rule_id)).size !== violations.length) throw new Error('Inconsistent screen result')
  return { portfolio_id: input.portfolio_id, evaluated_at: r.evaluated_at, status: r.status as ScreenResult['status'], violations }
}
export const screening = {
  async esg(input: ScreenInput): Promise<ScreenResult> {
    if (!validScreenInput(input)) throw new Error('Invalid screening input')
    return parseScreenResult(await api.post<unknown>('/api/v2/screening/esg', input), input)
  },
}
export function describeScreenError(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.status === 403) return 'Screening access was denied.'
    if (error.status === 404) return 'The exact screening contract is not available on this deployment.'
    if ([400, 413, 422].includes(error.status)) return 'The screen was refused. Check the explicit policy, timestamp and exact representable amounts.'
    if (error.status >= 500) return 'Screening is unavailable. No result is displayed.'
  }
  return 'The screening response could not be verified. No result is displayed.'
}
