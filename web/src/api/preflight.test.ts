import { afterEach, expect, it, vi } from 'vitest'
import served from '../../../kanz/services/web_bff/internal/server/testdata/preflight_pass.json'
import { api } from './client'
import { preflight } from './preflight'

afterEach(() => vi.restoreAllMocks())

it('reads the BFF response with its exact instant, zero observation and omitted zero age', async () => {
  const get = vi.spyOn(api, 'get').mockResolvedValue(served)
  const actual = await preflight.status()
  expect(get).toHaveBeenCalledWith('/api/preflight')
  expect(actual).toEqual(served)
  expect(actual.observed_at).toBe('2026-09-13T12:00:00.123456789Z')
  expect(actual.checks[0]?.observed).toBe(0)
})

it.each([
  ['only one named control', { checks: served.checks.slice(0, 1) }],
  ['one name appears twice', { checks: [served.checks[0], ...served.checks.slice(0, -1)] }],
  ['an unexpected control', { checks: [{ ...served.checks[0], name: 'new_control' }, ...served.checks.slice(1)] }],
  ['an unnamed control', { checks: [{ ...served.checks[0], name: '' }, ...served.checks.slice(1)] }],
  ['missing observation', { checks: [{ name: served.checks[0]!.name, status: 'PASS' }, ...served.checks.slice(1)] }],
  ['missing verifier identity', { verifier_commit: '' }],
  ['invalid deployed identity', { deployed_commit: 'next-version' }],
  ['missing command identity', { command_id: '' }],
  ['impossible observation date', { observed_at: '2026-02-30T12:00:00Z' }],
  ['the Go zero observation instant', { observed_at: '0001-01-01T00:00:00Z' }],
  ['the zero instant at an offset', { observed_at: '0001-01-01T01:30:00.000000000+01:30' }],
  ['missing workload', { workload_images: { oms: 'fixture' } }],
  ['empty required workload', { workload_images: { ...served.workload_images, venue_okx: '' } }],
  ['inconsistent FAIL', { status: 'FAIL' }],
  ['a stale PASS', { age_seconds: served.max_age_seconds + 1 }],
  ['a negative age', { age_seconds: -1 }],
  ['a null age', { age_seconds: null }],
])('refuses a verified verdict with %s', async (_, changes) => {
  vi.spyOn(api, 'get').mockResolvedValue({ ...served, ...changes })
  expect(await preflight.status()).toMatchObject({ status: 'UNKNOWN', checks: [] })
})

it('preserves a nonzero nanosecond at the Go epoch', async () => {
  const response = { ...served, observed_at: '0001-01-01T00:00:00.000000001Z' }
  vi.spyOn(api, 'get').mockResolvedValue(response)
  expect(await preflight.status()).toEqual(response)
})

it('retains the last whole evidence behind the stale server verdict at the rounded age boundary', async () => {
  const stale = {
    ...served, status: 'UNKNOWN', reason: 'preflight evidence is stale', age_seconds: served.max_age_seconds,
    checks: served.checks.map((check, index) => index ? check : { ...check, status: 'FAIL', observed: null }),
  }
  vi.spyOn(api, 'get').mockResolvedValue(stale)
  expect(await preflight.status()).toEqual(stale)
})

it('retains the BFF unavailable response without a completed evidence record', async () => {
  const unavailable = { status: 'UNKNOWN', reason: 'preflight evidence is not configured', max_age_seconds: 900, checks: [] }
  vi.spyOn(api, 'get').mockResolvedValue(unavailable)
  expect(await preflight.status()).toEqual(unavailable)
})
