import { afterEach, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ScreeningView from './ScreeningView.vue'
import { screening } from '../api/screening'
import { ApiError } from '../api/client'

afterEach(() => vi.restoreAllMocks())
async function ready() {
  const w = mount(ScreeningView)
  await w.get('[name="portfolio"]').setValue('test')
  await w.get('[name="currency"]').setValue('USD')
  await w.get('[name="as-of"]').setValue('2026-01-01T00:00:00Z')
  await w.get('[name="sectors"]').setValue('TEST-SECTOR')
  return w
}
it('states an empty snapshot assessed no holdings and invalidates results on editing', async () => {
  vi.spyOn(screening, 'esg').mockResolvedValue({ portfolio_id: 'test', evaluated_at: '2026-01-01T00:00:00Z', status: 'COMPLIANCE_STATUS_PASS', violations: [] })
  const w = await ready()
  await w.get('form').trigger('submit')
  await flushPromises()
  expect(w.text()).toContain('No holding was assessed')
  await w.get('[name="sectors"]').setValue('CHANGED')
  expect(w.find('[aria-label="Screening result"]').exists()).toBe(false)
})
it('shows rolling-deployment refusal without raw backend text', async () => {
  vi.spyOn(screening, 'esg').mockRejectedValue(new ApiError(404, 'PRIVATE'))
  const w = await ready()
  await w.get('form').trigger('submit')
  await flushPromises()
  expect(w.text()).toContain('exact screening contract is not available')
  expect(w.text()).not.toContain('PRIVATE')
})
it('separates unavailable data from an identified excluded holding', async () => {
  vi.spyOn(screening, 'esg').mockResolvedValue({ portfolio_id: 'test', evaluated_at: '2026-01-01T00:00:00Z', status: 'COMPLIANCE_STATUS_BREACH', violations: [{ rule_id: 'esg-sector-exclusion', message: 'Cannot verify', evidence: { classifier: 'unavailable' }, unavailable: true }] })
  const w = await ready()
  await w.get('form').trigger('submit')
  await flushPromises()
  expect(w.text()).toContain('Required data unavailable')
  expect(w.text()).not.toContain('Excluded holding')
})
