import { mount, flushPromises } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { afterEach, expect, it, vi } from 'vitest'
import { api } from '../api/client'
import { isDepthInput, spotRequest } from '../api/spot_depth'
import example from '../api/spot_depth_example.json'
import { useSession } from '../stores/session'
import SpotDepthView from './SpotDepthView.vue'

afterEach(() => vi.restoreAllMocks())

it('clears private assumptions on recapture and route changes and projects each portfolio binding', async () => {
  if (!isDepthInput(example)) throw new Error('Invalid fixture')
  const books = spotRequest(example).spot_depth.venues.reverse()
  const post = vi.spyOn(api, 'post').mockImplementation(async (path) => {
    const portfolio = path.split('/')[4]
    return {
      portfolio_id: portfolio, owner_tenant: 'pilot',
      spot_books: {
        status: 'SUPPORTED', as_of: example.cutoff, instrument_id: example.instrument,
        product_type: 'CRYPTO_SPOT', source: 'demo', producer_version: 'v1', rights_reference: 'demo-only',
        base_asset_ref: 'bitcoin', quote_asset_ref: 'tether',
        venues: books.map((row, i) => ({
          book: row.book, known_at: row.known_at, event_id: 'event-' + i,
          portfolio_account_ref: i === 0 ? 'binance-' + portfolio : '',
        })),
      },
    }
  })
  const pinia = createPinia()
  useSession(pinia).identity = { tenant: 'pilot', subject: 'operator', expires_at: '2030-01-01T00:00:00Z' }
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/portfolios/:id/spot-depth', component: SpotDepthView },
      { path: '/portfolios/:id/exposure', name: 'exposure', component: { template: '<div />' } },
    ],
  })
  await router.push('/portfolios/alpha/spot-depth')
  const view = mount(SpotDepthView, { global: { plugins: [router, pinia] } })
  const capture = async () => {
    await view.get('button').trigger('click')
    await flushPromises()
  }
  const field = (venue: number, label: string) => {
    const group = view.findAll('fieldset')[venue]!
    return group.findAll('label').find((item) => item.text() === label)!.get('input')
  }
  try {
    await capture()
    expect(field(0, 'Portfolio account').element.value).toBe('binance-alpha')
    expect(field(0, 'Portfolio account').element.readOnly).toBe(true)
    await field(0, 'Base available').setValue('7.5')
    await field(1, 'Scenario account').setValue('hypothetical')
    await field(1, 'Fee (bps)').setValue('2')
    await capture()
    expect(field(0, 'Base available').element.value).toBe('')
    expect(field(1, 'Fee (bps)').element.value).toBe('')
    expect(field(1, 'Scenario account').element.value).toBe('')
    await router.push('/portfolios/beta/spot-depth')
    expect(view.find('fieldset').exists()).toBe(false)
    await capture()
    expect(field(0, 'Portfolio account').element.value).toBe('binance-beta')
    const session = useSession(pinia)
    session.forget()
    await flushPromises()
    expect(view.find('fieldset').exists()).toBe(false)
    expect(post).toHaveBeenCalledTimes(3)
  } finally {
    view.unmount()
  }
})
