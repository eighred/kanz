import { afterEach, expect, it, vi } from 'vitest'
import { api } from './client'
import { parseScreenResult, screening, validScreenInput, type ScreenInput } from './screening'

const input: ScreenInput = { portfolio_id: 'test', base_currency: 'USD', as_of: '2026-01-01T00:00:00Z', positions: [{ instrument_id: 'TEST', quantity: '0.000000000001', market_value: '9007199254740993', currency: 'USD' }], excluded_sectors: ['TEST-SECTOR'], excluded_issuers: [] }
const result = { portfolio_id: 'test', mandate_id: 'esg-exclusion', mandate_version: '1', evaluated_at: input.as_of, status: 'COMPLIANCE_STATUS_PASS', violations: [] }
afterEach(() => vi.restoreAllMocks())
it('sends exact strings to the versioned endpoint only', async () => {
  const post = vi.spyOn(api, 'post').mockResolvedValue(result)
  expect((await screening.esg(input)).status).toBe('COMPLIANCE_STATUS_PASS')
  expect(post).toHaveBeenCalledWith('/api/v2/screening/esg', input)
})
it.each([{ ...result, mandate_version: 1 }, { ...result, portfolio_id: 'other' }, { ...result, evaluated_at: '2025-01-01T00:00:00Z' }, { ...result, status: 'COMPLIANCE_STATUS_BREACH' }, { ...result, violations: null }])('refuses a mismatched or legacy response', value => {
  expect(() => parseScreenResult(value, input)).toThrow()
})
it.each([{ classifier: 'unavailable', dimension: 'DIMENSION_SECTOR', holdings: '1' }, { unmarked_holdings: '1', unmarked_sample: 'TEST', unmarked_reasons: 'missing_amount' }, { classifier: 'present', dimension: 'DIMENSION_SECTOR', holdings: '1', unresolved: '1', unresolved_instruments: 'TEST' }])('retains explicit unavailable data', evidence => {
  const value = { ...result, status: 'COMPLIANCE_STATUS_BREACH', violations: [{ rule_id: 'esg-sector-exclusion', rule_type: 'RULE_TYPE_RESTRICTION', severity: 'COMPLIANCE_STATUS_BREACH', message: 'Cannot verify', evidence }] }
  expect(parseScreenResult(value, input).violations[0]?.unavailable).toBe(true)
})
it.each([{}, { unmarked_holdings: '0' }, { classifier: 'unavailable', holdings: '0' }])('refuses empty or inconsistent breach evidence', evidence => {
  expect(() => parseScreenResult({ ...result, status: 'COMPLIANCE_STATUS_BREACH', violations: [{ rule_id: 'esg-sector-exclusion', rule_type: 'RULE_TYPE_RESTRICTION', severity: 'COMPLIANCE_STATUS_BREACH', message: 'No evidence', evidence }] }, input)).toThrow()
})
it('requires an explicit policy and time, but preserves missing amounts', () => {
  expect(validScreenInput({ ...input, excluded_sectors: [] })).toBe(false)
  expect(validScreenInput({ ...input, as_of: '' })).toBe(false)
  expect(validScreenInput({ ...input, positions: [{ ...input.positions[0]!, market_value: '' }] })).toBe(true)
})
