import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import BrokerAccountView from './BrokerAccountView.vue'
import CustodyBreaksView from './CustodyBreaksView.vue'
import { broker } from '../api/broker'
import { custody } from '../api/custody'
import { ApiError } from '../api/client'

afterEach(() => vi.restoreAllMocks())

async function routeFor(path: string) {
  const router = createRouter({ history: createMemoryHistory(), routes: [
    { path: '/broker-accounts/:id', component: BrokerAccountView },
    { path: '/broker-accounts', component: { template: '<p>Accounts</p>' } },
  ] })
  await router.push(path)
  return router
}

describe('operations read screens', () => {
  it('labels broker history windows and unknown observation freshness', async () => {
    vi.spyOn(broker, 'account').mockResolvedValue({
      state: { id: 'PF1', currency: 'USD', balance: '0', equity: '0', realizedPl: '0', unrealizedPl: '0', openPositions: 0 },
      positions: [], orders: [], executions: [], ordersRetainedFrom: '2026-01-01T00:00:00Z',
    })
    const wrapper = mount(BrokerAccountView, { global: { plugins: [await routeFor('/broker-accounts/PF1')] } })
    await flushPromises()
    expect(wrapper.text()).toContain('freshness cannot be established')
    expect(wrapper.text()).toContain('Earlier orders are not shown')
    expect(wrapper.text()).toContain('No open positions')
  })

  it('does not render a denied custody queue as empty', async () => {
    vi.spyOn(custody, 'breaks').mockRejectedValue(new ApiError(403, 'private detail'))
    const wrapper = mount(CustodyBreaksView)
    await flushPromises()
    expect(wrapper.find('[role=alert]').text()).toContain('not permitted')
    expect(wrapper.text()).not.toContain('No outstanding custody breaks')
    expect(wrapper.text()).not.toContain('private detail')
  })
})
