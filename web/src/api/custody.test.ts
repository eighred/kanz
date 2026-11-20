import { afterEach, describe, expect, it, vi } from 'vitest'
import { users } from './users'
import { api, auth, ApiError } from './client'
import { custody, describeCustody, downloadCustodyComparison } from './custody'

afterEach(() => vi.restoreAllMocks())

const item = { break_id: 'break-1', kind: 'quantity', key: 'BTC-USD', ibor: '1.00000001', custodian: '1', difference: '0.00000001', status: 'OPEN', first_seen_at: '2026-01-01T00:00:00Z', last_seen_at: '2026-01-02T00:00:00Z', status_changed_at: '2026-01-01T00:00:00Z', age_seconds: 86400 }

describe('custody response boundary', () => {
  it('preserves the reason a trusted current comparison is pending', async () => {
    const subject = { portfolio_id: 'PF1', custodian_id: 'CUST-A', business_date: '2020-02-04' }
    const pending = { ...subject, tenant_id: 'tenant', statement_id: 's', state: 'pending' }
    const get = vi.spyOn(api, 'get')
    for (const reason of ['assignment_changed', 'assignment_unproven', 'journal_changed']) {
      get.mockResolvedValueOnce({ ...pending, reason })
      await expect(custody.dated(subject, 'tenant')).resolves.toMatchObject({ state: 'pending', reason })
    }
    get.mockResolvedValueOnce({ ...pending, reason: 'unknown_reason' })
    await expect(custody.dated(subject, 'tenant')).rejects.toThrow('Dated result could not be verified.')
  })

  const detection = { run_id: 'run-1', statement_id: 'source-r1', business_date: '2020-02-04', journal_position: '9007199254740993', scope_mode: 'declared' }
  const scoped = { ...item, portfolio_id: 'PF1', custodian_id: 'CUST-A', detection }
  it('preserves the exact retained source and optional fields', async () => {
    const get = vi.spyOn(api, 'get')
    for (const source of [detection, { ...detection, journal_position: '0' }, { run_id: 'r', statement_id: 's', business_date: '2024-02-29' }]) {
      get.mockResolvedValueOnce({ breaks: [{ ...scoped, detection: source }], count: 1 })
      expect((await custody.queue()).breaks[0]?.detection).toEqual(source)
    }
  })
  it.each([{ portfolio_id: '' }, { custodian_id: undefined }, { detection: null },
    ...[{ run_id: '' }, { statement_id: '' }, { business_date: '2025-02-29' }, { journal_position: 1 }, { journal_position: '-1' },
      { journal_position: '9223372036854775808' }, { scope_mode: [] }].map(fields => ({ detection: { ...detection, ...fields } }))])('refuses malformed detection and scope %#', async fields => {
    vi.spyOn(api, 'get').mockResolvedValueOnce({ breaks: [{ ...scoped, ...fields }], count: 1 })
    await expect(custody.queue()).rejects.toThrow('Invalid custody response.')
  })

  it('rejects an unbound comparison receipt and keeps a ledger position exact', async () => {
    const request = { portfolio_id: 'PF1', custodian_id: 'C', business_date: '2020-02-04', request_id: 'id' }
    const receipt = { ...request, actor: 'operator', run_id: 'run', completed_at: '2020-02-05T00:00:00Z', outcome: 'clean', statement_id: 'S', journal_position: '9007199254740993' }
    const post = vi.spyOn(api, 'post').mockResolvedValueOnce(receipt)
    expect((await custody.rerun(request, 'operator')).journal_position).toBe('9007199254740993')
    for (const patch of [{ actor: 'other' }, { request_id: 'other' }, { portfolio_id: 'other' }, { business_date: '2020-02-03' }, { outcome: 'no_statement' }, { journal_position: 1 }]) {
      post.mockResolvedValueOnce({ ...receipt, ...patch })
      await expect(custody.rerun(request, 'operator')).rejects.toThrow('receipt')
    }
  })

  it('requires the dated subject, exact position and a source for a successful result', async () => {
    const subject = { portfolio_id: 'PF/1', custodian_id: 'C & A', business_date: '2020-02-04' }
    const data = { ...subject, tenant_id: 'tenant', transaction_pass: 'balances_only', state: 'recorded', run_id: 'retained', outcome: 'clean', statement_id: 'r2', completed_at: '2020-02-05T01:00:00Z', journal_position: '9007199254740993', scope_mode: 'declared' }
    const get = vi.spyOn(api, 'get').mockResolvedValueOnce(data)
    const projected = await custody.dated(subject, 'tenant')
    expect(projected).toMatchObject({ state: 'recorded', journal_position: '9007199254740993', transactions: { transaction_pass: 'balances_only' } })
    expect(projected).not.toHaveProperty('account_assignment_verified', true)
    get.mockResolvedValueOnce({ ...data, account_assignment_verified: true })
    expect(await custody.dated(subject, 'tenant')).toMatchObject({ account_assignment_verified: true })
    expect(get.mock.calls[0]?.[0]).toBe('/api/v1/portfolios/PF%2F1/custody/comparison?custodian_id=C+%26+A&business_date=2020-02-04')
    for (const patch of [{ tenant_id: 'foreign' }, { tenant_id: undefined }, { transaction_pass: undefined },
      { account_assignment_verified: 'true' }, { account_assignment_verified: false },
      { account_assignment_verified: true, scope_mode: 'derived' }, { account_assignment_verified: true, outcome: 'failed' },
      { transaction_pass: 'unknown', book_reference_count: '0' }, { transaction_pass: 'ran', statement_reference_count: '1' },
      { transaction_pass: 'ran', book_reference_count: '0', statement_reference_count: 1, unreferenced_book_trades: '0' }, { portfolio_id: 'PF2' }, { statement_id: undefined }, { state: 'not_compared' }, { journal_position: 9007199254740992 }, { journal_position: '-1' }, { journal_position: undefined }, { outcome: 'no_statement' }]) {
      get.mockResolvedValueOnce({ ...data, ...patch })
      await expect(custody.dated(subject, 'tenant')).rejects.toThrow('verified')
    }
    for (const state of ['not_compared', 'unknown']) {
      get.mockResolvedValueOnce({ ...subject, tenant_id: 'tenant', state })
      expect((await custody.dated(subject, 'tenant')).state).toBe(state)
    }
  })

  it('downloads only the dated projection and preserves exact reference counts on repeated reads', async () => {
    const subject = { portfolio_id: 'PF1', custodian_id: 'C', business_date: '2020-02-04' }
    const wire = { ...subject, tenant_id: 'fixture', state: 'recorded', outcome: 'breaks', run_id: 'run-1',
      completed_at: '2020-02-05T01:00:00Z', statement_id: 'signed-r1', scope_mode: 'declared', journal_position: '9007199254740993',
      transaction_pass: 'ran', statement_reference_count: '0', book_reference_count: '9007199254740994', unreferenced_book_trades: '2' }
    const get = vi.spyOn(api, 'get').mockResolvedValue({ ...wire, raw_statement: 'discard', actor: 'discard', account_ids: ['discard'] })
    const create = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:custody')
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {})
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    const downloads: string[] = []
    for (let i = 0; i < 2; i++) {
      const result = await custody.dated(subject, 'fixture')
      if (result.state !== 'recorded') throw new Error('expected a recorded comparison')
      downloadCustodyComparison(result)
      const blob = create.mock.calls[i]?.[0]
      if (!(blob instanceof Blob)) throw new Error('expected a downloadable JSON file')
      downloads.push(await blob.text())
    }
    expect(downloads[0]).toBe(downloads[1])
    expect(JSON.parse(downloads[0]!)).toEqual({ format: 'kanz.custody-comparison.v1', ...wire, transaction_comparison_basis: 'external_reference_membership' })
    expect(click).toHaveBeenCalledTimes(2)
    for (const value of ['-1', '01', '9223372036854775808', null, 0]) {
      get.mockResolvedValueOnce({ ...wire, unreferenced_book_trades: value })
      await expect(custody.dated(subject, 'fixture')).rejects.toThrow('verified')
    }
  })

  it('binds the saved run, signed digest and exact immutable differences to the requested portfolio and date', async () => {
    const subject = { portfolio_id: 'P', custodian_id: 'C', business_date: '2020-02-04' }
    const digest = 'a'.repeat(64)
    const row = { break_id: 'P|C|quantity|F', kind: 'quantity', key: 'ΔF', values_state: 'exact', ibor: '10.5000001', custodian: '9.5', difference: '1.0000001' }
    const request = 'old+run:1'
    const wire = { ...subject, tenant_id: 'tenant', state: 'recorded', outcome: 'breaks', run_id: request, completed_at: '2020-02-05T01:00:00Z',
      statement_id: 'custody-batch.v1.source.r1.' + digest, scope_mode: 'declared', journal_position: '1',
      transaction_pass: 'unknown', signed_statement_sha256: digest, breaks: [{ ...row, assignee: 'discard' }] }
    const get = vi.spyOn(api, 'get').mockResolvedValueOnce(wire)
    const read = () => custody.dated(subject, 'tenant', undefined, request)
    const result = await read()
    expect(get.mock.calls[0]?.[0]).toContain('run_id=old%2Brun%3A1')
    expect(result.state === 'recorded' && result.historical?.breaks).toEqual([row])
    for (const patch of [{ run_id: 'other' }, { signed_statement_sha256: 'b'.repeat(64) }, { statement_id: 'unrelated.' + digest }, { transaction_pass: undefined },
      { outcome: 'clean' }, { breaks: [] }, { breaks: [row, row] }, { breaks: [{ ...row, ibor: 10.5 }] }, { breaks: [{ ...row, values_state: 'unavailable' }] }]) {
      get.mockResolvedValueOnce({ ...wire, ...patch })
      await expect(read()).rejects.toThrow('verified')
    }
    get.mockResolvedValueOnce({ ...wire, breaks: [{ ...row, values_state: 'unavailable', ibor: '', custodian: '', difference: '' }] })
    expect((await read()).state).toBe('recorded')
    get.mockResolvedValueOnce({ ...wire, outcome: 'clean', breaks: [] })
    expect((await read()).state).toBe('recorded')
    get.mockResolvedValueOnce({ ...subject, state: 'unknown', tenant_id: 'tenant', breaks: [] })
    await expect(read()).rejects.toThrow('verified')
    get.mockResolvedValueOnce(wire)
    await expect(custody.dated(subject, 'tenant')).rejects.toThrow('verified')
  })

  it('only exports measured same-instrument amounts on a bound saved run', async () => {
    const subject = { portfolio_id: 'P', custodian_id: 'C', business_date: '2020-02-04' }
    const digest = 'a'.repeat(64)
    const base = { ...subject, tenant_id: 'fixture', state: 'recorded', outcome: 'clean', run_id: 'R',
      completed_at: '2020-02-05T01:00:00Z', statement_id: 'custody-batch.v1.src.r1.' + digest,
      journal_position: '1', scope_mode: 'declared', transaction_pass: 'ran',
      statement_reference_count: '4', book_reference_count: '3', unreferenced_book_trades: '0' }
    const wire = { ...base, signed_statement_sha256: digest, breaks: [] }
    const amount = (coefficient: string, exponent = 0) => ({ coefficient, exponent })
    const difference = { external_ref: 'ref', instrument_id: 'SYN', ibor: amount('-9007199254740993', -2),
      custodian: amount('0'), difference: amount('-9007199254740993', -2) }
    const section = { compared: 1, unassessed: 2, differences: [difference] }
    const get = vi.spyOn(api, 'get')
    const read = () => custody.dated(subject, 'fixture', undefined, 'R')
    get.mockResolvedValueOnce({ ...wire, execution_quantities: { ...section, actor: 'discard', differences: [
      { ...difference, price: 999, ibor: { ...difference.ibor, account: 'discard' } },
    ] } })
    const result = await read()
    if (result.state !== 'recorded') throw new Error('recorded result required')
    const create = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:fixture')
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {})
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    downloadCustodyComparison(result)
    const blob = create.mock.calls[0]?.[0]
    expect(blob).toBeInstanceOf(Blob)
    const exported = JSON.parse(await (blob as Blob).text())
    expect(exported.execution_quantities).toEqual(section)
    expect(exported.transaction_comparison_basis).toBe('external_reference_membership')
    get.mockResolvedValueOnce(wire)
    const previous = await read()
    expect(previous.state === 'recorded' && previous.historical).not.toHaveProperty('execution_quantities')
    get.mockResolvedValueOnce({ ...wire, execution_quantities: { compared: 0, unassessed: 0, differences: [] } })
    const zero = await read()
    expect(zero.state === 'recorded' && zero.historical?.execution_quantities?.compared).toBe(0)
    // UTF-8 byte order (Go) and JavaScript UTF-16 string order differ on this pair.
    const unicode = [{ ...difference, external_ref: '\uE000' }, { ...difference, external_ref: '\u{10000}' }]
    get.mockResolvedValueOnce({ ...wire, execution_quantities: { compared: 2, unassessed: 0, differences: unicode } })
    expect((await read()).state).toBe('recorded')
    for (const bad of [null, {}, { ...section, compared: 1.5 }, { ...section, unassessed: -1 },
      { ...section, compared: 4294967296 }, { ...section, unassessed: 3 }, { ...section, compared: '1' },
      { ...section, compared: 0 }, { compared: 2, unassessed: 0, differences: [...unicode].reverse() },
      { compared: 2, unassessed: 0, differences: [difference, difference] },
      ...[{ instrument_id: '' }, { external_ref: '' }, { ibor: {} }, { ibor: null }, { custodian: { coefficient: '0' } },
        { ibor: amount('0', -3) }, { ibor: amount('01') }, { ibor: amount('9223372036854775808') },
        { ibor: amount('-9223372036854775809') }, { ibor: { coefficient: 10, exponent: 0 } },
        { ibor: amount('-1', -61) }, { custodian: amount('1', 61) }, { ibor: amount('10', -1) }, { difference: amount('0') }].map(
        patch => ({ ...section, differences: [{ ...difference, ...patch }] }))]) {
      get.mockResolvedValueOnce({ ...wire, execution_quantities: bad })
      await expect(read()).rejects.toThrow('verified')
    }
    get.mockResolvedValueOnce({ ...wire, statement_reference_count: '1048577', book_reference_count: '1048577',
      execution_quantities: { compared: 1048577, unassessed: 0, differences: [] } })
    await expect(read()).rejects.toThrow('verified')
    get.mockResolvedValueOnce({ ...wire, execution_quantities: { ...section, differences: [{ ...difference, external_ref: '長'.repeat(350000) }] } })
    await expect(read()).rejects.toThrow('verified')
    for (const fields of [{ ...base }, { ...subject, tenant_id: 'fixture', state: 'unknown' }]) {
      get.mockResolvedValueOnce({ ...fields, execution_quantities: section })
      await expect(custody.dated(subject, 'fixture')).rejects.toThrow('verified')
    }
    get.mockResolvedValueOnce({ ...wire, tenant_id: 'foreign', execution_quantities: section })
    await expect(read()).rejects.toThrow('verified')
    get.mockResolvedValueOnce({ ...wire, transaction_pass: 'balances_only', execution_quantities: section })
    await expect(read()).rejects.toThrow('verified')
  })

  it('preserves exact decimal text', async () => {
    vi.spyOn(api, 'get').mockResolvedValue({ breaks: [item], count: 1 })
    expect((await custody.queue()).breaks[0]?.difference).toBe('0.00000001')
  })

  it.each([{ breaks: [], count: 1 }, { breaks: [{ ...item, difference: 0.1 }], count: 1 }, { breaks: [{ ...item, age_seconds: -1 }], count: 1 }])('rejects an invalid queue rather than reporting success', async (body) => {
    vi.spyOn(api, 'get').mockResolvedValue(body)
    await expect(custody.queue()).rejects.toThrow('Invalid custody response')
  })

  it('distinguishes an unavailable control from an empty queue', () => {
    expect(describeCustody(new ApiError(404, 'hidden'))).toContain('not enabled')
    expect(describeCustody(new ApiError(403, 'hidden'))).toContain('not permitted')
  })

  it('requires exact data, a revision and durable action capability together', async () => {
    const get = vi.spyOn(api, 'get')
    for (const body of [
      { breaks: [item], count: 1, actions_enabled: true },
      { breaks: [{ ...item, revision: '1', values_state: 'legacy_unverified' }], count: 1, actions_enabled: true },
      { breaks: [{ ...item, status: 'open', revision: '1', values_state: 'exact' }], count: 1, actions_enabled: false },
    ]) {
      get.mockResolvedValueOnce(body)
      expect((await custody.queue()).breaks[0]?.actions_enabled).toBe(false)
    }
    vi.spyOn(auth, 'me').mockResolvedValue({ tenant: 'fixture', subject: 'operator', expires_at: '2099-01-01T00:00:00Z' })
    vi.spyOn(users, 'gatewayPermissions').mockResolvedValue({ tenant: 'fixture', subject: 'operator', portfolios: ['PF1'], capabilities: ['fund'], routes: [{ Pattern: 'POST /v1/custody/breaks/{id}/actions', Capability: 'fund' }] })
    get.mockResolvedValueOnce({ breaks: [{ ...item, status: 'open', revision: '1', values_state: 'exact' }], count: 1, actions_enabled: true })
    expect((await custody.queue()).breaks[0]?.actions_enabled).toBe(true)
    vi.mocked(users.gatewayPermissions).mockResolvedValue({ tenant: 'fixture', subject: 'operator', portfolios: ['PF1'], capabilities: ['custody_read'], routes: [] })
    get.mockResolvedValueOnce({ breaks: [{ ...item, status: 'open', revision: '1', values_state: 'exact' }], count: 1, actions_enabled: true })
    expect((await custody.queue()).breaks[0]?.actions_enabled).toBe(false)
  })

  it('encodes the break id and refuses mismatched action evidence', async () => {
    const row = { ...item, break_id: 'BTC/USD?x#y', status: 'open', revision: '1', values_state: 'exact' as const, actions_enabled: true }
    const post = vi.spyOn(api, 'post').mockResolvedValue({ actor: 'someone' })
    const decision = { action: 'claim' as const, request_id: 'request-1', expected_revision: '1', review_contract: 'exact-v1' as const }
    await expect(custody.act(row, decision)).rejects.toThrow('evidence could not be verified')
    expect(post).toHaveBeenCalledWith('/api/v1/custody/breaks/BTC%2FUSD%3Fx%23y/actions', decision, undefined, undefined)
  })

  it.each(['claim', 'explain'] as const)('requires %s evidence to preserve the untouched field', async action => {
    const row = { ...item, status: 'explained', assignee: 'previous-owner', explanation: 'Wire pending',
      revision: '9007199254740993', values_state: 'exact' as const, actions_enabled: true }
    const decision = { action, request_id: 'request-1', expected_revision: row.revision, review_contract: 'exact-v1' as const,
      ...(action === 'explain' ? { explanation: 'Wire reconciled' } : {}) }
    const before = { status: row.status, assignee: row.assignee, explanation: row.explanation, revision: row.revision }
    const after = { ...before, revision: '9007199254740994', ...(action === 'claim'
      ? { status: 'assigned', assignee: 'operator' } : { status: 'explained', explanation: 'Wire reconciled' }) }
    const receipt = { request_id: decision.request_id, break_id: row.break_id, actor: 'operator', action,
      recorded_at: '2026-01-02T01:00:00Z', before, after, values: { ibor: row.ibor, custodian: row.custodian, difference: row.difference } }
    const post = vi.spyOn(api, 'post').mockResolvedValueOnce(receipt)
    expect(await custody.act(row, decision)).toEqual(receipt)
    post.mockResolvedValueOnce({ ...receipt, after: { ...after, [action === 'claim' ? 'explanation' : 'assignee']: '' } })
    await expect(custody.act(row, decision)).rejects.toThrow('evidence could not be verified')
  })

  it('accepts only evidence for the exact reviewed figures and state', async () => {
    const row = { ...item, status: 'open', revision: '9007199254740993', values_state: 'exact' as const, actions_enabled: true }
    const decision = { action: 'claim' as const, request_id: 'request-1', expected_revision: row.revision, review_contract: 'exact-v1' as const }
    const result = { request_id: decision.request_id, break_id: row.break_id, actor: 'verified-human', action: 'claim', recorded_at: '2026-01-02T01:00:00Z', before: { status: 'open', assignee: '', explanation: '', revision: row.revision }, after: { status: 'assigned', assignee: 'verified-human', explanation: '', revision: '9007199254740994' }, values: { ibor: row.ibor, custodian: row.custodian, difference: row.difference } }
    const post = vi.spyOn(api, 'post').mockResolvedValue(result)
    expect(await custody.act(row, decision)).toEqual(result)
    post.mockResolvedValue({ ...result, values: { ...result.values, difference: '0' } })
    await expect(custody.act(row, decision)).rejects.toThrow('evidence could not be verified')
  })
})
