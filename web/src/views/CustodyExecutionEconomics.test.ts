import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { useSession } from '../stores/session'
import DatedCustodyResult from './DatedCustodyResult.vue'
import { afterEach, expect, it, vi } from 'vitest'
import { api } from '../api/client'
import { custody, downloadCustodyComparison } from '../api/custody'
import CustodyExecutionEconomics from './CustodyExecutionEconomics.vue'

afterEach(() => vi.restoreAllMocks())

const business_date = '2020-02-04'
const subject = { portfolio_id: 'PF1', custodian_id: 'C1', business_date }
const digest = 'a'.repeat(64)
const original = { ...subject, state: 'recorded', outcome: 'clean', tenant_id: 'pilot', run_id: 'saved', statement_id: 'custody-batch.v1.source.r1.' + digest,
  transaction_pass: 'ran', statement_reference_count: '2', book_reference_count: '2', unreferenced_book_trades: '0',
  account_assignment_verified: true, scope_mode: 'declared', journal_position: '3', completed_at: '2020-02-05T01:00:00Z' }
const amount = (coefficient: string, exponent = 0) => ({ coefficient, exponent })
const signed = { quantity: amount('1'), price: amount('9007199254740992'), gross_cash: amount('-9007199254740992'),
  commission: amount('1', -1), net_cash: amount('-90071992547409921', -1), settled_cash: amount('-90071992547409921', -1) }
const book = { ...signed, price: amount('9007199254740993'), gross_cash: amount('-9007199254740993'), net_cash: amount('-90071992547409931', -1), settled_cash: amount('-90071992547409931', -1) }
const difference = { quantity: amount('0'), price: amount('1'), gross_cash: amount('-1'), commission: amount('0'), net_cash: amount('-1'), settled_cash: amount('-1') }
const assessed = { external_ref: 'ref-a', instrument_id: 'SYN', currency_code: 'USD', convention: 'unit_price_net_cash_same_currency_commission_v1', entry_id: 'entry-1',
  status: 'mismatch', reason: '', book_settlement_date: business_date, custodian_settlement_date: business_date, book, custodian: signed, difference }
const unknown = { external_ref: 'ref-b', instrument_id: 'SYN', currency_code: 'USD', convention: '', entry_id: '',
  status: 'unknown', reason: 'convention_missing', book_settlement_date: '', custodian_settlement_date: '' }
const comparison = { unassessed_reason: '', rows: [assessed, unknown] }
const wire = { ...original, breaks: [], signed_statement_sha256: digest, execution_economics_status: 'mismatch', execution_economics: comparison }
const read = () => custody.dated(subject, 'pilot', undefined, original.run_id)

it('displays and downloads the sealed comparison with exact units, separate statuses and settled cash', async () => {
  vi.spyOn(api, 'get').mockResolvedValue(wire)
  const result = await read()
  if (result.state !== 'recorded' || !result.historical?.execution_economics) throw new Error('missing original')
  expect(result.outcome).toBe('clean')
  expect(result.execution_economics_status).toBe('mismatch')
  const section = mount(CustodyExecutionEconomics, { props: { comparison: result.historical.execution_economics } })
  try {
    const matched = section.get('[data-status="mismatch"]')
    expect(matched.text()).toContain('USD per SYN')
    const cash = matched.findAll('tr').find(r => r.text().includes('Settled cash'))!
    expect(cash.findAll('td').map(td => td.text())).toEqual(['USD', '-9007199254740993.1', '-9007199254740992.1', '-1'])
    expect(matched.text()).toContain('0.1')
    expect(section.get('[data-status="unknown"]').find('table').exists()).toBe(false)
    expect(section.get('[data-status="unknown"]').text()).toContain('no price and cash convention')
  } finally { section.unmount() }
  const create = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:fixture')
  vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {})
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
  downloadCustodyComparison(result)
  const artifact = JSON.parse(await (create.mock.calls[0]![0] as Blob).text())
  expect(artifact.execution_economics).toEqual(comparison)
  expect(artifact.signed_statement_sha256).toBe(digest)
})

it('verifies receipt identity and scope before accepting economic rows or a current status', async () => {
  const get = vi.spyOn(api, 'get')
  get.mockResolvedValueOnce({ ...original, execution_economics_status: 'mismatch' })
  expect(await custody.dated(subject, 'pilot')).toMatchObject({ execution_economics_status: 'mismatch' })
  get.mockResolvedValueOnce({ ...original, breaks: [], signed_statement_sha256: digest })
  const old = await read()
  expect(old.state === 'recorded' && old.historical).not.toHaveProperty('execution_economics')
  for (const mutation of [
    { tenant_id: 'other' }, { run_id: 'other' }, { account_assignment_verified: undefined }, { signed_statement_sha256: 'b'.repeat(64) },
    { statement_reference_count: '1' }, { execution_economics_status: 'equal' },
    ...[[unknown, assessed], [assessed, { ...unknown, book }], [{ ...assessed, book: { ...book, price: amount('01') } }, unknown],
      [{ ...assessed, book_settlement_date: '2020-02-05' }, unknown]].map(rows => ({ execution_economics: { rows, unassessed_reason: '' } })),
  ]) {
    get.mockResolvedValueOnce({ ...wire, ...mutation })
    await expect(read()).rejects.toThrow('verified')
  }
  get.mockResolvedValueOnce({ ...wire, execution_economics_status: 'unknown', statement_reference_count: '129', execution_economics: { rows: [], unassessed_reason: 'row_limit' } })
  expect(await read()).toMatchObject({ historical: { execution_economics: { rows: [], unassessed_reason: 'row_limit' } } })
})

it('opens only the signed statement selected on the current comparison', async () => {
  setActivePinia(createPinia())
  useSession().identity = { subject: 'reviewer', tenant: 'pilot', expires_at: '2099-01-01T00:00:00Z' }
  const other = 'b'.repeat(64)
  const get = vi.spyOn(api, 'get').mockResolvedValueOnce({ ...original, execution_economics_status: 'mismatch' })
    .mockResolvedValueOnce({ ...wire, statement_id: 'custody-batch.v1.source.r2.' + other, signed_statement_sha256: other })
  const page = mount(DatedCustodyResult)
  try {
    const fields = page.findAll('input')
    for (const [i, value] of [subject.portfolio_id, subject.custodian_id, business_date].entries()) await fields[i]!.setValue(value)
    await page.get('form').trigger('submit'); await flushPromises()
    expect(page.get('output').text()).toContain('Execution economics differ')
    await page.findAll('button').find(b => b.text() === 'View original comparison receipt')!.trigger('click')
    await flushPromises()
    expect(get.mock.calls[1]![0]).toContain('run_id=saved')
    expect(page.get('[role="alert"]').text()).toContain('Unable to verify')
    expect(page.find('[aria-label="Execution economics"]').exists()).toBe(false)
    expect(page.text()).not.toContain('Download comparison receipt')
  } finally { page.unmount() }
})
