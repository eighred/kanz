import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, ApiError } from './client'
import { copilot, describeInvestigation, parseInvestigation } from './copilot'

const answer = { answer: 'Source value 9007199254740993', citations: ['test:source'], grounded: false, ungrounded: ['9007199254740993'], refused: false, budget_exhausted: false, injection_flagged: true }
afterEach(() => vi.restoreAllMocks())
describe('governed investigation contract', () => {
  it('preserves numeric text and projects only the supported response', async () => {
    const post = vi.spyOn(api, 'post').mockResolvedValue({ ...answer, raw: 'private' })
    expect(await copilot.ask('test question')).toEqual(answer)
    expect(post).toHaveBeenCalledWith('/api/v1/ask', { question: 'test question' })
  })
  it.each([{}, { ...answer, grounded: true }, { ...answer, refused: true, budget_exhausted: true }, { ...answer, ungrounded: [1] }, { ...answer, citations: null }, { ...answer, answer: 'x'.repeat(65537) }])('refuses an ambiguous response', value => {
    expect(() => parseInvestigation(value)).toThrow()
  })
  it('refuses oversized UTF-8 and empty questions before fetching', async () => {
    const post = vi.spyOn(api, 'post')
    await expect(copilot.ask('あ'.repeat(6000))).rejects.toThrow()
    await expect(copilot.ask('   ')).rejects.toThrow()
    expect(post).not.toHaveBeenCalled()
  })
  it.each([403, 404, 413, 429, 503])('never exposes backend error details (%i)', status => {
    expect(describeInvestigation(new ApiError(status, 'PRIVATE diagnostic'))).not.toContain('PRIVATE')
  })
})
