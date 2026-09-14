import { afterEach, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import CopilotView from './CopilotView.vue'
import { copilot, type Investigation } from '../api/copilot'
import { ApiError } from '../api/client'

const answer: Investigation = { answer: '<script>unsafe()</script> 9007199254740993', citations: ['javascript:unsafe()'], grounded: false, ungrounded: ['9007199254740993'], refused: false, budget_exhausted: false, injection_flagged: true }
afterEach(() => vi.restoreAllMocks())
it('shows warnings and citations as escaped text without executable markup', async () => {
  vi.spyOn(copilot, 'ask').mockResolvedValue(answer)
  const w = mount(CopilotView)
  await w.get('textarea').setValue('test')
  await w.get('form').trigger('submit')
  await flushPromises()
  expect(w.text()).toContain('9007199254740993')
  expect(w.text()).toContain('Potential prompt injection')
  expect(w.text()).toContain('Unmatched numeric claims')
  expect(w.find('script').exists()).toBe(false)
  expect(w.find('a').exists()).toBe(false)
})
it('clears the previous answer while waiting and after access refusal', async () => {
  const ask = vi.spyOn(copilot, 'ask').mockResolvedValue(answer)
  const w = mount(CopilotView)
  await w.get('textarea').setValue('test')
  await w.get('form').trigger('submit')
  await flushPromises()
  let reject!: (error: Error) => void
  ask.mockImplementation(() => new Promise((_, no) => { reject = no }))
  await w.get('form').trigger('submit')
  expect(w.text()).not.toContain(answer.answer)
  expect(w.get('button').attributes('disabled')).toBeDefined()
  reject(new ApiError(403, 'PRIVATE'))
  await flushPromises()
  expect(w.text()).toContain('access was denied')
  expect(w.text()).not.toContain('PRIVATE')
  expect(w.text()).not.toContain(answer.answer)
})
it.each([
  [{ ...answer, refused: true }, 'Model refused'],
  [{ ...answer, budget_exhausted: true }, 'Investigation incomplete'],
  [{ ...answer, answer: '', citations: [], ungrounded: [], grounded: true }, 'No source citations'],
])('keeps distinct response states', async (value, expected) => {
  vi.spyOn(copilot, 'ask').mockResolvedValue(value)
  const w = mount(CopilotView)
  await w.get('textarea').setValue('test')
  await w.get('form').trigger('submit')
  await flushPromises()
  expect(w.text()).toContain(expected)
})
