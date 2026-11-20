import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { api } from '../api/client'
import DailyReturn from './DailyReturn.vue'

const start = '2020-02-07T00:00:00Z'
const end = '2020-02-08T00:00:00Z'
const asOf = '2020-02-08T01:00:00Z'
const base = { portfolio_id: 'PF1', currency: 'USD', start, end, as_of: asOf, source_digest: 'a'.repeat(64),
  measured: { portfolio_id: 'PF1', start, end, as_of: asOf, method: 'RETURN_METHOD_TIME_WEIGHTED', return_value: 0.331 } }
const first = { movement_id: 'deposit', command_digest: 'b'.repeat(64), effective_time: '2020-02-07T10:00:00.000001Z',
  signed_amount: { coefficient: '9007199254740993', exponent: -1 }, pre_flow_nav_reference: 'pre-1', post_flow_nav_reference: 'post-1' }
const second = { movement_id: 'withdrawal', command_digest: 'c'.repeat(64), effective_time: '2020-02-07T10:00:00.000002Z',
  signed_amount: { coefficient: '-4', exponent: 1 }, pre_flow_nav_reference: 'pre-2', post_flow_nav_reference: 'post-2' }
const multi = { ...base, external_flow_count: '2', intraday_flows: [first, second] }

beforeEach(() => setActivePinia(createPinia()))
afterEach(() => vi.restoreAllMocks())

async function openReport(report: object) {
  vi.spyOn(api, 'get').mockResolvedValue(report)
  const page = mount(DailyReturn, { props: { portfolio: 'PF1' } })
  const inputs = page.findAll('input')
  for (const [i, value] of ['2020-02-08', 'USD', asOf].entries()) await inputs[i]!.setValue(value)
  await page.get('form').trigger('submit')
  await flushPromises()
  return page
}

it('renders the signed order, exact amounts and mark references for every linked flow', async () => {
  const page = await openReport(multi)
  try {
    expect(page.get('output').text()).toContain('33.1%')
    expect(page.get('output').text()).toContain('3 linked subperiods')
    expect(page.findAll('tbody tr').map(row => row.findAll('th, td').map(cell => cell.text()))).toEqual([
      ['deposit', first.effective_time, '900719925474099.3', 'pre-1', 'post-1', first.command_digest],
      ['withdrawal', second.effective_time, '-40', 'pre-2', 'post-2', second.command_digest],
    ])
  } finally { page.unmount() }
})

it.each([{}, { intraday_flows: [] }])('reads a singular receipt with old or current protobuf JSON', async wire => {
  const page = await openReport({ ...base, ...wire, intraday_flow: first, external_flow_count: '1' })
  try {
    expect(page.get('output').text()).toContain('Two linked subperiods · 900719925474099.3 USD external flow at ' + first.effective_time)
    expect(page.find('table').exists()).toBe(false)
  } finally { page.unmount() }
})

it('preserves the signed cash evidence and identifies the unavailable mark on an unassessed day', async () => {
  const page = await openReport({ ...multi, measured: undefined, unassessed_reason: 'nav_at_flow_missing',
    intraday_flows: [first, { ...second, pre_flow_nav_reference: '' }] })
  try {
    expect(page.get('output').text()).toContain('Unassessed. A valuation at the cash flow is unavailable.')
    expect(page.get('output').text()).not.toContain('Time-weighted return')
    expect(page.get('output').text()).not.toContain('linked subperiods')
    expect(page.findAll('tbody tr')[1]!.findAll('td').map(cell => cell.text())).toEqual([
      second.effective_time, '-40', 'Unavailable', 'post-2', second.command_digest,
    ])
  } finally { page.unmount() }
})

it.each([
  { external_flow_count: '3' }, { intraday_flow: first }, { intraday_flows: [first] }, { intraday_flows: [second, first] },
  { intraday_flows: [first, { ...second, effective_time: first.effective_time }] },
  { intraday_flows: [first, { ...second, movement_id: first.movement_id }] },
  { intraday_flows: [first, { ...second, effective_time: end }] },
  { intraday_flows: [first, { ...second, pre_flow_nav_reference: '' }] },
  { intraday_flows: [first, { ...second, signed_amount: undefined }] },
])('refuses a measured return with inconsistent flow evidence %#', async patch => {
  const page = await openReport({ ...multi, ...patch })
  try {
    expect(page.get('[role="alert"]').text()).toBe('Unable to verify the daily return.')
    expect(page.find('output').exists()).toBe(false)
  } finally { page.unmount() }
})
