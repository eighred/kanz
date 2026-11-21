import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import { afterEach, expect, it, vi } from 'vitest'
import * as datedRisk from '../api/dated-risk'
import DatedRiskView from './DatedRiskView.vue'

afterEach(() => vi.restoreAllMocks())

it('invalidates the report and export when the operator changes portfolio or cutoff', async () => {
  const cutoff = '2026-01-02T03:04:05.000000123Z'
  const result = { portfolio_id: 'a', as_of: cutoff, set: { portfolio_id: 'a', as_of: cutoff } }
  let finish: (value: typeof result) => void = () => {}
  const read = vi.spyOn(datedRisk, 'readDatedRisk').mockResolvedValueOnce(result)
    .mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
  const router = createRouter({ history: createMemoryHistory(), routes: [
    { path: '/portfolios/:id/risk', component: DatedRiskView },
  ] })
  await router.push('/portfolios/a/risk')
  const view = mount(DatedRiskView, { global: { plugins: [router] } })
  try {
    await view.get('input').setValue(cutoff)
    await view.get('form').trigger('submit')
    await flushPromises()
    expect(view.text()).toContain('Measure unavailable')
    expect(view.text()).toContain('Download JSON')
    await view.get('input').setValue('2026-01-03T00:00:00Z')
    expect(view.text()).not.toContain('Download JSON')
    await view.get('form').trigger('submit')
    await router.push('/portfolios/b/risk')
    finish(result)
    await flushPromises()
    expect(read.mock.calls[1]![2].aborted).toBe(true)
    expect(view.text()).not.toContain('Download JSON')
    expect(view.text()).not.toContain(cutoff)
  } finally {
    view.unmount()
  }
})
