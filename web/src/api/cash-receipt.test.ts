import { afterEach, expect, it, vi } from 'vitest'
import { readCashReceipt } from './cash-receipt'

const receipt = { actor: 'maker', movement_id: 'wire/# 12', accepted_at: '2026-11-18T12:00:00.123456Z',
  command_digest: 'a'.repeat(64), status: 'accepted' }

afterEach(() => vi.unstubAllGlobals())

it.each(['accepted', 'journal_recorded'])('reads the %s receipt using the two exact identifiers', async status => {
  const response = { ...receipt, status }
  const fetch = vi.fn().mockResolvedValue(Response.json(response))
  vi.stubGlobal('fetch', fetch)
  const signal = new AbortController().signal
  await expect(readCashReceipt('PF +A', receipt.movement_id, signal)).resolves.toEqual(response)
  expect(fetch).toHaveBeenCalledOnce()
  expect(fetch).toHaveBeenCalledWith('/api/v1/portfolios/PF%20%2BA/cash-movements/wire%2F%23%2012',
    expect.objectContaining({ method: 'GET', credentials: 'same-origin', signal, body: undefined }))
})

it.each([
  { movement_id: 'different' }, { actor: '' }, { actor: ' actor ' },
  { accepted_at: '2026-02-30T12:00:00Z' }, { accepted_at: 'yesterday' },
  { command_digest: 'b'.repeat(63) }, { command_digest: 'G'.repeat(64) },
  { status: 'settled' }, { status: null },
])('refuses malformed accounting evidence %j', async change => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(Response.json({ ...receipt, ...change })))
  await expect(readCashReceipt('PF', receipt.movement_id, new AbortController().signal)).rejects.toThrow('Invalid cash receipt')
})

it('refuses an HTTP acknowledgement that is not the status resource', async () => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(Response.json(receipt, { status: 202 })))
  await expect(readCashReceipt('PF', receipt.movement_id, new AbortController().signal)).rejects.toThrow()
})

it.each([['.', 'id'], ['PF', '..'], ['PF', ''], ['PF', 'é'.repeat(129)]])('rejects invalid IDs before fetch', async (pf, id) => {
  const fetch = vi.fn()
  vi.stubGlobal('fetch', fetch)
  await expect(readCashReceipt(pf, id, new AbortController().signal)).rejects.toThrow('Invalid cash receipt identifiers')
  expect(fetch).not.toHaveBeenCalled()
})
