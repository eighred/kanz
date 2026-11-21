import { createPinia, setActivePinia } from 'pinia'
import { useSession } from '../stores/session'
import { setUnauthenticatedHandler } from '../api/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import CustodyBreaksView from './CustodyBreaksView.vue'
import { custody, type CustodyBreak, type CustodyDecision, type CustodyEvidence } from '../api/custody'
import { users } from '../api/users'
import { api, auth, ApiError } from '../api/client'

beforeEach(() => {
  setActivePinia(createPinia())
  useSession().identity = { subject: 'operator', tenant: 'fixture', expires_at: '2099-01-01T00:00:00Z' }
})
afterEach(() => { setUnauthenticatedHandler(() => {}); vi.unstubAllGlobals(); vi.restoreAllMocks() })
const row: CustodyBreak = { break_id: 'PF|CUST|quantity|BTC/USD', kind: 'quantity', key: 'BTC/USD', ibor: '1.000000000001', custodian: '1', difference: '0.000000000001', status: 'open', first_seen_at: '2026-01-01T00:00:00Z', last_seen_at: '2026-01-02T00:00:00Z', status_changed_at: '2026-01-01T00:00:00Z', age_seconds: 86400, revision: '1', values_state: 'exact', actions_enabled: true }
const receipt: CustodyEvidence = { request_id: 'test-request', break_id: row.break_id, actor: 'verified-human', action: 'claim', recorded_at: '2026-01-02T01:00:00Z', before: { status: 'open', assignee: '', explanation: '', revision: '1' }, after: { status: 'assigned', assignee: 'verified-human', explanation: '', revision: '2' }, values: { ibor: row.ibor, custodian: row.custodian, difference: row.difference } }

function button(wrapper: ReturnType<typeof mount>, text: string) {
  const result = wrapper.findAll('button').find((b) => b.text() === text)
  if (!result) throw new Error(`Missing button ${text}`)
  return result
}

describe('custody exact review', () => {
  it.each(['session', 'unmount'])('cancels both dated and queue reads on %s before late 401 responses arrive', async reason => {
    const replies: ((r: Response) => void)[] = []
    const signals: AbortSignal[] = []
    vi.stubGlobal('fetch', vi.fn((_path: string, options: RequestInit) => {
      signals.push(options.signal!)
      return new Promise<Response>(resolve => replies.push(resolve))
    }))
    const forgotten = vi.fn()
    setUnauthenticatedHandler(forgotten)
    const wrapper = mount(CustodyBreaksView)
    const form = wrapper.get('[aria-label="Dated custody result"] form')
    for (const [i, value] of ['PF1', 'C', '2020-02-04'].entries()) await form.findAll('input')[i]!.setValue(value)
    await form.trigger('submit')
    expect(signals).toHaveLength(2)
    if (reason === 'session') useSession().identity!.tenant = 'other'
    else wrapper.unmount()
    for (const signal of signals.slice(0, 2)) expect(signal.aborted).toBe(true)
    for (const reply of replies.slice(0, 2)) reply(Response.json({}, { status: 401 }))
    await flushPromises()
    expect(forgotten).not.toHaveBeenCalled()
    if (reason === 'session') {
      expect(form.get('input').element.value).toBe('')
      expect(wrapper.find('output').exists()).toBe(false)
      wrapper.unmount()
    }
  })

  it('opens only the discovered signed correction and clears its exact quantities on session change', async () => {
    vi.spyOn(custody, 'queue').mockResolvedValue({ breaks: [] })
    const scope = { portfolio_id: 'P', custodian_id: 'C', business_date: '2020-02-04', tenant_id: 'fixture' }
    const statement = 'custody-batch.v1.src.r2.' + 'a'.repeat(64)
    const pending = { ...scope, state: 'pending' as const, statement_id: statement, historical_quantity_correction_run_id: 'correction' }
    const saved = { ...scope, state: 'recorded' as const, outcome: 'historical_quantity_correction' as const,
      run_id: 'correction', completed_at: '2020-02-07T00:00:00Z', scope_mode: 'declared', journal_position: '2', statement_id: statement,
      historical: { signed_statement_sha256: 'a'.repeat(64), historical_quantity_resolutions: [
        { break_id: 'q1:' + 'b'.repeat(64), detection_run_id: 'detection', entry_id: 'entry', external_ref: 'external',
          instrument_id: 'SYN', agreed_quantity: { coefficient: '0', exponent: 0 } }] } }
    const get = vi.spyOn(custody, 'dated').mockResolvedValueOnce(pending).mockResolvedValueOnce({ ...saved, statement_id: 'another-source' })
      .mockResolvedValueOnce(pending).mockResolvedValueOnce(saved)
    const wrapper = mount(CustodyBreaksView)
    try {
      const form = wrapper.get('[aria-label="Dated custody result"] form')
      for (const [i, value] of ['P', 'C', scope.business_date].entries()) await form.findAll('input')[i]!.setValue(value)
      for (const success of [false, true]) {
        await form.findAll('input')[3]!.setValue('')
        await form.trigger('submit')
        await flushPromises()
        await button(wrapper, 'View corrected execution quantities').trigger('click')
        await flushPromises()
        expect(get.mock.lastCall?.[3]).toBe('correction')
        if (!success) {
          expect(wrapper.text()).toContain('Unable to verify')
          expect(wrapper.find('[aria-label="Execution quantity resolutions"]').exists()).toBe(false)
        }
      }
      expect(wrapper.get('[aria-label="Execution quantity resolutions"] tbody tr').findAll('td').map(cell => cell.text())).toContain('0')
      expect(wrapper.text()).toContain('Historical execution quantities corrected')
      useSession().identity!.tenant = 'other'
      await flushPromises()
      expect(wrapper.find('[aria-label="Execution quantity resolutions"]').exists()).toBe(false)
      expect(wrapper.text()).not.toContain('Download comparison receipt')
    } finally { wrapper.unmount() }
  })

  it('shows the selected date even with an empty queue and clears the last result on subject change', async () => {
    vi.spyOn(custody, 'queue').mockResolvedValue({ breaks: [] })
    const select = vi.spyOn(custody, 'dated')
    const wrapper = mount(CustodyBreaksView)
    try {
      const region = wrapper.get('[aria-label="Dated custody result"]')
      const fields = region.findAll('input')
      for (const [i, value] of ['PF1', 'C', '2020-02-04'].entries()) await fields[i]!.setValue(value)
      const subject = { tenant_id: 'fixture', portfolio_id: 'PF1', custodian_id: 'C', business_date: '2020-02-04' }
      const receipt = { ...subject, run_id: 'run-r2', state: 'recorded' as const, completed_at: '2020-02-05T01:00:00Z' }
      for (const [result, status] of [
        [{ ...receipt, outcome: 'clean', statement_id: 'signed-r2', scope_mode: 'declared', journal_position: '9007199254740993',
          transactions: { transaction_pass: 'ran', statement_reference_count: '1', book_reference_count: '1', unreferenced_book_trades: '0' } }, 'Balances and transaction references agree'],
        [{ ...receipt, outcome: 'clean', statement_id: 'signed-r2', transactions: { transaction_pass: 'balances_only' } }, 'Transactions were not compared'],
        [{ ...receipt, outcome: 'clean', statement_id: 'signed-r2', transactions: { transaction_pass: 'unknown' } }, 'Transaction coverage unknown'],
        [{ ...receipt, outcome: 'historical_clean', account_assignment_verified: true, statement_id: 'custody-batch.v1.source.r2.' + 'a'.repeat(64),
          transactions: { transaction_pass: 'balances_only' } }, 'Historical close comparison · Balances agree'],
        [{ ...receipt, outcome: 'historical_breaks', account_assignment_verified: true, statement_id: 'custody-batch.v1.source.r2.' + 'a'.repeat(64),
          transactions: { transaction_pass: 'unknown' } }, 'Historical close comparison · Differences found'],
        [{ ...receipt, outcome: 'clean', statement_id: 'signed-r2', transactions: { transaction_pass: 'ran', statement_reference_count: '0',
          book_reference_count: '0', unreferenced_book_trades: '2' } }, 'Incomplete coverage'],
        [{ ...receipt, outcome: 'no_statement' }, 'No statement received'],
        [{ ...subject, state: 'not_compared' }, 'No comparison recorded'],
      ] as const) {
        select.mockResolvedValueOnce(result)
        await region.get('form').trigger('submit'); await flushPromises()
        expect(region.get('output').text()).toContain(status)
        if ('outcome' in result && (result.outcome === 'historical_clean' || result.outcome === 'historical_breaks')) {
          expect(region.get('output').text()).toContain('A later close controls the outstanding break queue.')
          expect(region.get('output').text()).toContain('Custody account assignment verified')
          expect(region.findAll('button').some(b => b.text() === 'View previous statement comparison')).toBe(true)
        }
        expect(region.findAll('button').some(b => b.text() === 'Download comparison receipt')).toBe(result.state === 'recorded')
        if ('journal_position' in result) expect(region.get('output').text()).toContain(result.journal_position)
        if ('transactions' in result && (result.transactions.transaction_pass !== 'ran' || result.transactions.unreferenced_book_trades !== '0')) {
          expect(region.get('output').text()).not.toContain('Balances and transaction references agree')
        }
      }
      await fields[1]!.setValue('different')
      expect(region.find('output').exists()).toBe(false)
    } finally { wrapper.unmount() }
  })

  it.each(['breaks', 'historical_breaks'])('opens the signed predecessor of a correction with outcome %s', async outcome => {
    vi.spyOn(custody, 'queue').mockResolvedValue({ breaks: [] })
    const scope = { portfolio_id: 'PF1', custodian_id: 'C', business_date: '2020-02-04', tenant_id: 'fixture' }
    const base = { ...scope, state: 'recorded', account_assignment_verified: true, scope_mode: 'declared', journal_position: '1',
      completed_at: '2020-02-06T00:00:00Z', transaction_pass: 'balances_only' }
    const r2 = { ...base, outcome, run_id: 'corrected', statement_id: 'custody-batch.v1.src.r2.' + 'b'.repeat(64) }
    const r1 = { ...base, outcome: 'breaks', run_id: 'original', statement_id: 'custody-batch.v1.src.r1.' + 'a'.repeat(64),
      previous_of_statement_id: r2.statement_id, previous_of_run_id: r2.run_id, signed_statement_sha256: 'a'.repeat(64),
      breaks: [{ break_id: 'PF1|C|quantity|SYN', kind: 'quantity', key: 'SYN', ibor: '11.5', custodian: '9.5', difference: '2', values_state: 'exact' }] }
    const get = vi.spyOn(api, 'get').mockResolvedValueOnce(r2).mockResolvedValueOnce(r1)
    const wrapper = mount(CustodyBreaksView)
    try {
      const region = wrapper.get('[aria-label="Dated custody result"]')
      const form = region.get('form')
      for (const [i, value] of [scope.portfolio_id, scope.custodian_id, scope.business_date].entries()) await form.findAll('input')[i]!.setValue(value)
      await form.trigger('submit')
      await flushPromises()
      expect(region.get('output').text()).toContain(r2.statement_id)
      await button(wrapper, 'View previous statement comparison').trigger('click')
      await flushPromises()
      expect(get.mock.lastCall?.[0]).toBe('/api/v1/portfolios/PF1/custody/comparison?custodian_id=C&business_date=2020-02-04&run_id=corrected&include=previous_comparison')
      expect(region.get('output').text()).toContain(r1.statement_id)
      expect(region.get('[aria-label="Differences at comparison"] tbody tr').findAll('td').map(td => td.text())).toEqual(['quantity', 'SYN', '11.5', '9.5', '2'])
      expect(region.findAll('button').some(b => b.text() === 'View previous statement comparison')).toBe(false)
    } finally { wrapper.unmount() }
  })

  it('keeps the current receipt on an unavailable earlier book lookup and can open a verified result for revision one', async () => {
    vi.spyOn(custody, 'queue').mockResolvedValue({ breaks: [] })
    const scope = { portfolio_id: 'P', custodian_id: 'C', business_date: '2020-02-04', tenant_id: 'fixture' }
    const digest = 'a'.repeat(64), statement = 'custody-batch.v1.source.r1.' + digest
    const current = { ...scope, state: 'recorded', outcome: 'breaks', run_id: 'after-fee', completed_at: '2020-02-06T00:00:00Z',
      scope_mode: 'declared', account_assignment_verified: true, statement_id: statement, journal_position: '2', transaction_pass: 'balances_only' }
    const before = { ...current, outcome: 'clean', run_id: 'before-fee', journal_position: '1', signed_statement_sha256: digest, breaks: [],
      previous_of_run_id: current.run_id, previous_of_statement_id: statement, previous_of_journal_position: current.journal_position }
    const get = vi.spyOn(api, 'get').mockResolvedValueOnce(current).mockResolvedValueOnce({ ...scope, state: 'unknown' }).mockResolvedValueOnce(before)
    const wrapper = mount(CustodyBreaksView)
    try {
      const region = wrapper.get('[aria-label="Dated custody result"]')
      const form = region.get('form')
      for (const [i, value] of ['P', 'C', scope.business_date].entries()) await form.findAll('input')[i]!.setValue(value)
      await form.trigger('submit'); await flushPromises()
      expect(region.findAll('button').some(b => b.text() === 'View previous statement comparison')).toBe(false)
      await button(wrapper, 'Find earlier book comparison').trigger('click'); await flushPromises()
      expect(region.get('[role="alert"]').text()).toBe('No verified earlier book comparison is available.')
      expect(region.get('output').text()).toContain(current.run_id)
      expect(form.findAll('input')[3]!.element.value).toBe('')
      await button(wrapper, 'Find earlier book comparison').trigger('click'); await flushPromises()
      expect(get.mock.lastCall?.[0]).toContain('run_id=after-fee&include=previous_book_comparison')
      expect(form.findAll('input')[3]!.element.value).toBe(before.run_id)
      expect(region.get('output').text()).toContain('Balances agree')
      expect(region.get('output').text()).toContain(statement)
      expect(region.find('[role="alert"]').exists()).toBe(false)
    } finally { wrapper.unmount() }
  })

  it('keeps the signed base download only when optional figures exceed a limit on the same authorized receipt', async () => {
    vi.spyOn(custody, 'queue').mockResolvedValue({ breaks: [] })
    const get = vi.spyOn(api, 'get')
    const digest = 'a'.repeat(64)
    const base = { portfolio_id: 'P', custodian_id: 'C', business_date: '2020-02-04', tenant_id: 'fixture',
      state: 'recorded', outcome: 'clean', run_id: 'run', completed_at: '2020-02-05T01:00:00Z', statement_id: 'custody-batch.v1.src.r1.' + digest,
      scope_mode: 'declared', account_assignment_verified: true, journal_position: '1', signed_statement_sha256: digest, breaks: [],
      transaction_pass: 'ran', statement_reference_count: '1', book_reference_count: '1', unreferenced_book_trades: '0' }
    const row = { external_ref: 'x', instrument_id: 'SYN', cash_currency: 'USD', reported_cash: '-2' }
    const section = { basis: 'signed_custodian_statement', signed_statement_sha256: digest, rows: [row] }
    const expanded = { ...base, reported_execution_figures: section }
    const limit = 1 << 20
    row.external_ref = 'x'.repeat(limit - JSON.stringify(expanded).length - 100)
    expect(JSON.stringify(expanded).length + 1).toBeLessThan(limit)
    expect(JSON.stringify(expanded, null, 2).length).toBeGreaterThan(limit)
    const create = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:custody')
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {})
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    const wrapper = mount(CustodyBreaksView)
    try {
      const region = wrapper.get('[aria-label="Dated custody result"]')
      const form = region.get('form')
      for (const [i, value] of ['P', 'C', base.business_date, 'run'].entries()) await form.findAll('input')[i]!.setValue(value)
      async function openBase() {
        get.mockResolvedValueOnce(base)
        await form.trigger('submit')
        await flushPromises()
      }
      async function openFigures() {
        await button(wrapper, 'View custodian-reported price and cash').trigger('click')
        await flushPromises()
      }
      await openBase()
      for (const response of [expanded, new ApiError(422, 'signed execution figures exceed receipt limit')]) {
        if (response instanceof Error) get.mockRejectedValueOnce(response)
        else get.mockResolvedValueOnce(response)
        await openFigures()
        expect(region.get('[role="alert"]').text()).toContain('receipt limit')
        expect(region.get('output').text()).toContain(base.statement_id)
        await button(wrapper, 'Download comparison receipt').trigger('click')
        const artifact = JSON.parse(await (create.mock.lastCall![0] as Blob).text())
        expect(artifact.signed_statement_sha256).toBe(digest)
        expect(artifact).not.toHaveProperty('reported_execution_figures')
      }
      const changed = 'b'.repeat(64)
      for (const response of [new ApiError(403, 'forbidden'), new ApiError(503, 'unavailable'),
        { ...expanded, tenant_id: 'other' }, { ...expanded, signed_statement_sha256: changed, statement_id: 'custody-batch.v1.src.r1.' + changed,
          reported_execution_figures: { ...section, signed_statement_sha256: changed } }]) {
        await openBase()
        if (response instanceof Error) get.mockRejectedValueOnce(response)
        else get.mockResolvedValueOnce(response)
        await openFigures()
        expect(region.find('output').exists()).toBe(false)
        expect(region.text()).not.toContain('Download comparison receipt')
      }
      await openBase()
      let finish: (value: typeof expanded) => void = () => { throw new Error('Read did not start') }
      get.mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
      await button(wrapper, 'View custodian-reported price and cash').trigger('click')
      useSession().identity!.tenant = 'other'
      finish(expanded)
      await flushPromises()
      expect(region.find('output').exists()).toBe(false)
      expect(region.text()).not.toContain('Download comparison receipt')
    } finally { wrapper.unmount() }
  })

  it('shows signed execution units separately and clears them when a later saved lookup fails', async () => {
    vi.spyOn(custody, 'queue').mockResolvedValue({ breaks: [] })
    const get = vi.spyOn(api, 'get')
    const digest = 'a'.repeat(64)
    const base = { portfolio_id: 'P', custodian_id: 'C', business_date: '2020-02-04', tenant_id: 'fixture',
      state: 'recorded', outcome: 'clean', run_id: 'run', completed_at: '2020-02-05T01:00:00Z', statement_id: 'custody-batch.v1.src.r1.' + digest,
      journal_position: '1', scope_mode: 'declared', transaction_pass: 'ran', statement_reference_count: '2',
      book_reference_count: '2', unreferenced_book_trades: '0', signed_statement_sha256: digest, breaks: [] }
    const negative = { coefficient: '-9007199254740993', exponent: -2 }
    const section = { compared: 1, unassessed: 1, differences: [{ external_ref: '独立-7', instrument_id: 'SYN',
      ibor: negative, custodian: { coefficient: '0', exponent: 0 }, difference: negative }] }
    const wrapper = mount(CustodyBreaksView)
    try {
      const region = wrapper.get('[aria-label="Dated custody result"]')
      const form = region.get('form')
      for (const [i, value] of ['P', 'C', base.business_date, 'run'].entries()) await form.findAll('input')[i]!.setValue(value)
      async function load(response: unknown) {
        get.mockResolvedValueOnce(response)
        await form.trigger('submit'); await flushPromises()
      }
      await load({ ...base, execution_quantities: section })
      expect(region.get('output').text()).toContain('Balances and transaction references agree · Execution quantity differences found')
      const table = region.get('table[aria-label="Execution quantity differences"]')
      expect(table.findAll('tbody tr > *').map(cell => cell.text())).toEqual(['独立-7', 'SYN', '-90071992547409.93', '0', '-90071992547409.93'])
      expect(region.text()).toContain('Matched references compared: 1 · Unassessed: 1')
      expect(region.text()).toContain('Some matched references lack comparable quantity evidence.')
      expect(region.text()).toContain('Trade prices and cash amounts remain unverified.')
      await load({ ...base, execution_quantities: { compared: 0, unassessed: 2, differences: [] } })
      expect(region.text()).toContain('Matched references compared: 0 · Unassessed: 2')
      expect(region.text()).not.toContain('Compared quantities agree.')
      await load(base)
      expect(region.text()).toContain('Individual trade quantities, prices and cash amounts remain unverified.')
      expect(region.find('[aria-label="Execution quantities"]').exists()).toBe(false)
      await load({ ...base, execution_quantities: section })
      get.mockRejectedValueOnce(new ApiError(404, 'unavailable'))
      await form.trigger('submit'); await flushPromises()
      expect(region.text()).toContain('Access to this dated result is unavailable.')
      expect(region.find('output').exists()).toBe(false)
      expect(region.text()).not.toContain('独立-7')
      expect(region.text()).not.toContain('Download comparison receipt')
    } finally { wrapper.unmount() }
  })

  it.each([
    ['no_statement', 'no_statement'],
    ['historical_clean', 'Balances agree at the historical close'],
    ['historical_breaks', 'Differences found at the historical close'],
  ] as const)('recovers a failed comparison with the same reviewed identity and shows the committed %s receipt', async (outcome, label) => {
    vi.spyOn(custody, 'queue').mockResolvedValue({ breaks: [], rerunActor: 'operator' })
    const compare = vi.spyOn(custody, 'rerun').mockRejectedValueOnce(new ApiError(503, 'pending')).mockImplementationOnce(async req => ({
      ...req, actor: 'operator', run_id: 'retained-run', completed_at: '2020-02-05T10:00:00Z', outcome,
      ...(outcome !== 'no_statement' && { statement_id: 'custody-batch.v1.source.r2.' + 'a'.repeat(64), journal_position: '1', scope_mode: 'declared' }),
    }))
    const wrapper = mount(CustodyBreaksView)
    try {
      await flushPromises()
      const form = wrapper.get('form')
      for (const [i, value] of ['PF1', 'CUST-A', '2020-02-04'].entries()) await form.findAll('input')[i]!.setValue(value)
      await form.trigger('submit')
      expect(compare).not.toHaveBeenCalled()
      expect(button(wrapper, 'Reload queue').attributes('disabled')).toBeDefined()
      await button(wrapper, 'Run comparison').trigger('click')
      await flushPromises()
      await button(wrapper, 'Retry same request').trigger('click')
      await flushPromises()
      expect(compare.mock.calls[0]?.slice(0, 2)).toEqual(compare.mock.calls[1]?.slice(0, 2))
      expect(wrapper.get('[aria-label="Recorded custody comparison"]').text()).toContain(label)
      expect(wrapper.text()).toContain('retained-run')
    } finally { wrapper.unmount() }
  })

  it('shows the reference, instrument and exact working figures without write actions', async () => {
    const quantity = { ...row, kind: 'execution_quantity', late_correction_statement_id: 'signed-late-r2', key: 'external-7', instrument_id: 'SYN', revision: undefined,
      portfolio_id: 'PF1', custodian_id: 'CUST-A', detection: { run_id: 'original', statement_id: 'signed-r1', business_date: '2020-02-04', scope_mode: 'declared', journal_position: '1' },
      ibor: '-10.5', custodian: '0', difference: '-10.5' }
    const get = vi.spyOn(api, 'get').mockResolvedValue({ breaks: [quantity], count: 1, actions_enabled: true })
    const wrapper = mount(CustodyBreaksView)
    try {
      await flushPromises()
      const item = wrapper.get('tbody tr')
      expect(item.text()).toContain('external-7')
      expect(item.text()).toContain('Instrument SYN')
      expect(item.text()).toContain('Signed revision signed-late-r2 awaits historical quantity review.')
      expect(item.text()).toContain('signed-r1')
      expect(item.findAll('td').slice(2, 5).map(cell => cell.text())).toEqual(['-10.5', '0', '-10.5'])
      expect(item.find('button').exists()).toBe(false)
      for (const fields of [{ instrument_id: '' }, { revision: '1' }, { values_state: 'unavailable' }, { detection: undefined }]) {
        get.mockResolvedValueOnce({ breaks: [{ ...quantity, ...fields }], count: 1 })
        await expect(custody.queue()).rejects.toThrow('Invalid custody response.')
      }
    } finally { wrapper.unmount() }
  })

  it('reads detection again on reload and preserves unknown historical fields', async () => {
    const detection = { run_id: 'recorded-run', statement_id: 'signed-statement-r1', business_date: '2020-02-04', journal_position: '9007199254740993' }
    const get = vi.spyOn(custody, 'queue').mockResolvedValueOnce({ breaks: [{ ...row, portfolio_id: 'PF1', custodian_id: 'CUST-A', detection }] }).mockResolvedValueOnce({ breaks: [row] })
    const wrapper = mount(CustodyBreaksView)
    try {
      await flushPromises()
      const source = wrapper.get('details').text()
      for (const value of ['PF1', 'CUST-A', ...Object.values(detection)]) expect(source).toContain(value)
      expect(source).toContain('Account scopeUnavailable')
      await button(wrapper, 'Reload queue').trigger('click')
      await flushPromises()
      expect(get).toHaveBeenCalledTimes(2)
      expect(wrapper.text()).not.toContain(detection.statement_id)
      expect(wrapper.text()).toContain('Detection source unavailable.')
    } finally { wrapper.unmount() }
  })

  it('identifies the selected break from the queue through confirmation and the recorded receipt', async () => {
    const other = { ...row, break_id: 'PF2|CUST|quantity|BTC/USD' }
    vi.spyOn(auth, 'me').mockResolvedValue({ tenant: 'fixture', subject: 'operator', expires_at: '2099-01-01T00:00:00Z' })
    vi.spyOn(users, 'gatewayPermissions').mockResolvedValue({ tenant: 'fixture', subject: 'operator', portfolios: ['PF1'], capabilities: ['fund'], routes: [{ Pattern: 'POST /v1/custody/breaks/{id}/actions', Capability: 'fund' }] })
    vi.spyOn(api, 'get').mockResolvedValue({ breaks: [row, other], count: 2, actions_enabled: true })
    const post = vi.spyOn(api, 'post').mockImplementation(async (_path, body) => ({
      ...receipt, break_id: other.break_id, request_id: (body as CustodyDecision).request_id,
    }))
    const wrapper = mount(CustodyBreaksView)
    try {
      await flushPromises()
      const rows = wrapper.findAll('tbody tr')
      expect(rows.map(entry => entry.get('th[scope="row"]').text())).toEqual([row.break_id, other.break_id])
      await rows[1]!.get('button').trigger('click')
      const review = wrapper.get('[aria-label="Custody action review"]')
      expect(review.text()).toContain(other.break_id)
      await button(wrapper, 'Review action').trigger('click')
      expect(review.text()).toContain(other.break_id)
      expect(post).not.toHaveBeenCalled()
      await button(wrapper, 'Confirm reviewed action').trigger('click')
      await flushPromises()
      expect(post).toHaveBeenCalledTimes(1)
      const [path, decision] = post.mock.calls[0]!
      expect(path).toBe('/api/v1/custody/breaks/PF2%7CCUST%7Cquantity%7CBTC%2FUSD/actions')
      const recorded = wrapper.get('[role="status"]').text()
      expect(recorded).toContain(other.break_id)
      expect(recorded).toContain((decision as CustodyDecision).request_id)
      expect(recorded).not.toContain(row.break_id)
    } finally { wrapper.unmount() }
  })

  it('requires review, preserves the retry identity, and reloads after evidence', async () => {
    const read = vi.spyOn(custody, 'queue').mockResolvedValue({ breaks: [row] })
    const act = vi.spyOn(custody, 'act').mockRejectedValueOnce(new Error('network lost')).mockResolvedValueOnce(receipt)
    const wrapper = mount(CustodyBreaksView, { attachTo: document.body })
    await flushPromises()
    await button(wrapper, 'Claim').trigger('click')
    expect(act).not.toHaveBeenCalled()
    await button(wrapper, 'Review action').trigger('click')
    expect(wrapper.find('[aria-label="Custody action review"]').text()).toContain('0.000000000001')
    expect(act).not.toHaveBeenCalled()
    await button(wrapper, 'Confirm reviewed action').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('Retry this same reviewed request')
    const first = act.mock.calls[0]?.[1]
    expect(document.activeElement).toBe(button(wrapper, 'Confirm reviewed action').element)
    expect(first?.review_contract).toBe('exact-v1')
    expect(first).not.toHaveProperty('actor')
    expect(first).not.toHaveProperty('assignee')
    await button(wrapper, 'Confirm reviewed action').trigger('click')
    await flushPromises()
    expect(act.mock.calls[1]?.[1]).toEqual(first)
    expect(read).toHaveBeenCalledTimes(2)
    expect(wrapper.find('[role="status"]').text()).toContain('verified-human')
    expect(document.activeElement).toBe(wrapper.get('[role="status"]').element)
    expect(wrapper.find('[aria-label="Custody action review"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('moves keyboard focus through both reviews and returns to the invoking row on cancel', async () => {
    vi.spyOn(custody, 'queue').mockResolvedValue({ breaks: [row, { ...row, break_id: 'PF|CUST|quantity|ETH/USD', key: 'ETH/USD' }] })
    const act = vi.spyOn(custody, 'act')
    const wrapper = mount(CustodyBreaksView, { attachTo: document.body })
    try {
      await flushPromises()
      for (const action of ['Claim', 'Explain']) {
        const trigger = wrapper.findAll('tbody button').filter(b => b.text() === action)[1]!
        const triggerElement = trigger.element as HTMLButtonElement
        triggerElement.focus()
        await trigger.trigger('click')
        expect(document.activeElement).toBe(action === 'Explain' ? wrapper.get('textarea').element : button(wrapper, 'Review action').element)
        if (action === 'Explain') await wrapper.get('textarea').setValue('Cash settlement in progress')
        await button(wrapper, 'Review action').trigger('click')
        expect(document.activeElement).toBe(button(wrapper, 'Confirm reviewed action').element)
        await button(wrapper, 'Cancel').trigger('click')
        expect(document.activeElement).toBe(trigger.element)
        expect(act).not.toHaveBeenCalled()
      }
    } finally { wrapper.unmount() }
  })

  it('requires a fresh review after a revision conflict', async () => {
    vi.spyOn(custody, 'queue').mockResolvedValue({ breaks: [row] })
    vi.spyOn(custody, 'act').mockRejectedValue(new ApiError(409, 'private database detail'))
    const wrapper = mount(CustodyBreaksView, { attachTo: document.body })
    await flushPromises()
    await button(wrapper, 'Explain').trigger('click')
    expect(button(wrapper, 'Review action').attributes('disabled')).toBeDefined()
    await wrapper.find('textarea').setValue('Settlement remains pending')
    await button(wrapper, 'Review action').trigger('click')
    await button(wrapper, 'Confirm reviewed action').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('review a fresh action')
    expect(document.activeElement).toBe(wrapper.get('[role="alert"]').element)
    expect(wrapper.text()).not.toContain('private database detail')
    expect(wrapper.find('[aria-label="Custody action review"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('does not offer actions or present legacy rounded numbers as exact', async () => {
    vi.spyOn(custody, 'queue').mockResolvedValue({ breaks: [{ ...row, values_state: 'legacy_unverified', actions_enabled: false }] })
    const wrapper = mount(CustodyBreaksView)
    await flushPromises()
    expect(wrapper.text()).toContain('source replay and reconciliation required')
    expect(wrapper.text()).not.toContain('1.000000000001')
    expect(wrapper.findAll('button').some((b) => b.text() === 'Claim')).toBe(false)
  })
})

describe('custody queue navigation', () => {
  it('pages in both directions, indicates incompleteness, and reloads from the beginning without writing', async () => {
    const read = vi.spyOn(custody, 'queue').mockImplementation(async (_signal, cursor) => cursor
      ? { breaks: [{ ...row, break_id: 'second', actions_enabled: false }] }
      : { breaks: [{ ...row, break_id: 'first', actions_enabled: false }], nextCursor: 'next' })
    const post = vi.spyOn(api, 'post')
    const wrapper = mount(CustodyBreaksView)
    const click = async (name: string) => { await button(wrapper, name).trigger('click'); await flushPromises() }
    try {
      await flushPromises()
      expect(wrapper.text()).toContain('More results are available.')
      await click('Next page')
      expect(wrapper.get('th[scope="row"]').text()).toBe('second')
      expect(wrapper.text()).toContain('End of queue.')
      expect(wrapper.findAll('button').map(b => b.text())).not.toContain('Claim')
      expect(wrapper.findAll('button').map(b => b.text())).not.toContain('Explain')
      await click('Previous page')
      expect(wrapper.get('th[scope="row"]').text()).toBe('first')
      await click('Next page')
      await click('Reload queue')
      expect(read.mock.calls.map(([, after]) => after)).toEqual([undefined, 'next', undefined, 'next', undefined])
      expect(post).not.toHaveBeenCalled()
    } finally { wrapper.unmount() }
  })
  it('clears a pending continuation and its data on tenant change', async () => {
    const read = vi.spyOn(custody, 'queue').mockResolvedValueOnce({ breaks: [row], nextCursor: 'oldscope' })
    let finish!: (value: { breaks: CustodyBreak[]; nextCursor: string }) => void
    read.mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    read.mockResolvedValueOnce({ breaks: [] })
    const wrapper = mount(CustodyBreaksView)
    try {
      await flushPromises()
      await button(wrapper, 'Next page').trigger('click')
      const old = read.mock.calls[1]![0]!
      useSession().identity!.tenant = 'new-tenant'
      await flushPromises()
      expect(old.aborted).toBe(true)
      expect(read.mock.calls.map(([, after]) => after)).toEqual([undefined, 'oldscope', undefined])
      finish({ breaks: [{ ...row, break_id: 'foreign-result' }], nextCursor: 'foreign-next' })
      await flushPromises()
      expect(wrapper.text()).not.toContain('foreign-result')
      expect(wrapper.find('nav[aria-label="Custody queue pages"]').exists()).toBe(false)
      expect(wrapper.findAll('button').map(b => b.text())).not.toContain('Claim')
    } finally { wrapper.unmount() }
  })
  it('clears writable rows when the backend denies the next page', async () => {
    vi.spyOn(custody, 'queue').mockResolvedValueOnce({ breaks: [row], nextCursor: 'next' })
      .mockRejectedValueOnce(new ApiError(403, 'forbidden'))
    const post = vi.spyOn(api, 'post')
    const wrapper = mount(CustodyBreaksView)
    try {
      await flushPromises()
      expect(button(wrapper, 'Claim').exists()).toBe(true)
      await button(wrapper, 'Next page').trigger('click')
      await flushPromises()
      expect(wrapper.get('[role="alert"]').text()).toContain('not permitted')
      expect(wrapper.find('th[scope="row"]').exists()).toBe(false)
      expect(wrapper.findAll('button').map(b => b.text())).not.toContain('Claim')
      expect(post).not.toHaveBeenCalled()
    } finally { wrapper.unmount() }
  })
  it('distinguishes a rejected or now-empty continuation from an empty queue', async () => {
    const first = { breaks: [row], nextCursor: 'next' }
    vi.spyOn(custody, 'queue').mockResolvedValueOnce(first).mockRejectedValueOnce(new ApiError(400, 'invalid cursor'))
      .mockResolvedValueOnce(first).mockResolvedValueOnce({ breaks: [] })
    const wrapper = mount(CustodyBreaksView)
    const click = async (name: string) => { await button(wrapper, name).trigger('click'); await flushPromises() }
    try {
      await flushPromises()
      await click('Next page')
      expect(wrapper.get('[role="alert"]').text()).toContain('Reload the queue')
      expect(wrapper.text()).not.toContain('No outstanding custody breaks')
      await click('Reload queue')
      await click('Next page')
      expect(wrapper.text()).toContain('No outstanding breaks remain on this page')
      expect(wrapper.text()).not.toContain('No outstanding custody breaks')
    } finally { wrapper.unmount() }
  })
})
