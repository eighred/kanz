import { api, ApiError } from './client'

export interface Investigation {
  answer: string
  citations: string[]
  grounded: boolean
  ungrounded: string[]
  refused: boolean
  budget_exhausted: boolean
  injection_flagged: boolean
}

function text(value: unknown, max: number): value is string {
  return typeof value === 'string' && value.length <= max
}
function texts(value: unknown, count: number, length: number): value is string[] {
  return Array.isArray(value) && value.length <= count && value.every(item => text(item, length))
}

export function parseInvestigation(value: unknown): Investigation {
  if (!value || typeof value !== 'object') throw new Error('Invalid investigation response')
  const r = value as Record<string, unknown>
  if (!text(r.answer, 65536) || !texts(r.citations, 256, 2048) || !texts(r.ungrounded, 1024, 128)
    || typeof r.grounded !== 'boolean' || typeof r.refused !== 'boolean'
    || typeof r.budget_exhausted !== 'boolean' || typeof r.injection_flagged !== 'boolean'
    || (r.refused && r.budget_exhausted) || (r.grounded && r.ungrounded.length > 0)) {
    throw new Error('Invalid investigation response')
  }
  return {
    answer: r.answer, citations: [...r.citations], grounded: r.grounded,
    ungrounded: [...r.ungrounded], refused: r.refused,
    budget_exhausted: r.budget_exhausted, injection_flagged: r.injection_flagged,
  }
}

export const copilot = {
  async ask(question: string): Promise<Investigation> {
    if (!question.trim() || new TextEncoder().encode(question).length > 16384) throw new Error('Invalid question')
    return parseInvestigation(await api.post<unknown>('/api/v1/ask', { question }))
  },
}

export function describeInvestigation(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.status === 403) return 'Investigation access was denied.'
    if (error.status === 404) return 'Copilot is not enabled on this gateway.'
    if (error.status === 413) return 'The question exceeds the supported size.'
    if (error.status === 400 || error.status === 422) return 'The question could not be accepted.'
    if (error.status === 429) return 'Investigation capacity is unavailable. Try again later.'
    if (error.status >= 500) return 'Copilot is unavailable. No completed investigation was returned.'
  }
  return 'The investigation response could not be verified. No answer is displayed.'
}
