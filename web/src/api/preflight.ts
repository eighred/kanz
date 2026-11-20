import { api } from './client'
import { preflightChecks, preflightWorkloads } from './preflight_contract'
import { isTimestamp } from './timestamp'

export type PreflightState = 'PASS' | 'FAIL' | 'UNKNOWN'

export interface PreflightCheck {
  name: string
  status: 'PASS' | 'FAIL'
  observed: unknown
}

export interface PreflightStatus {
  status: PreflightState
  reason?: string
  observed_at?: string
  age_seconds?: number
  max_age_seconds: number
  verifier_commit?: string
  deployed_commit?: string
  command_id?: string
  checks: PreflightCheck[]
  workload_images?: Record<string, string>
}

const knownChecks = new Set<string>(preflightChecks)
const commit = /^[a-f0-9]{40}$/
const zeroInstant = Date.parse('0001-01-01T00:00:00Z')
const invalid = (reason: string): PreflightStatus => ({
  status: 'UNKNOWN', reason, max_age_seconds: 0, checks: [],
})

function object(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}

function seconds(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0
}

function observedInstant(value: unknown): value is string {
  if (!isTimestamp(value)) return false
  // Date.parse normalizes offsets but truncates the fractional second to milliseconds.
  return Date.parse(value) !== zeroInstant || /\.\d*[1-9]/.test(value)
}

function workloadImages(value: unknown): value is Record<string, string> {
  return object(value) && Object.values(value).every(image => typeof image === 'string') &&
    preflightWorkloads.every(name => typeof value[name] === 'string' && value[name] !== '')
}

function readStatus(value: unknown): PreflightStatus {
  if (!object(value) || !seconds(value.max_age_seconds) || !Array.isArray(value.checks)) {
    return invalid('preflight response is incomplete')
  }
  const { status, reason } = value
  if (status !== 'PASS' && status !== 'FAIL' && status !== 'UNKNOWN') {
    return invalid('preflight verdict is not recognized')
  }
  if (status === 'UNKNOWN' && (typeof reason !== 'string' || !reason)) {
    return invalid('preflight evidence is unavailable')
  }
  if (status !== 'UNKNOWN' && reason !== undefined) return invalid('preflight verdict has an unexpected reason')
  if (!value.checks.length) {
    if (status === 'UNKNOWN' && typeof reason === 'string') {
      return { status, reason, max_age_seconds: value.max_age_seconds, checks: [] }
    }
    return invalid('preflight evidence contains no controls')
  }

  const checks: PreflightCheck[] = []
  const seen = new Set<string>()
  for (const check of value.checks) {
    if (!object(check) || typeof check.name !== 'string' || !knownChecks.has(check.name) ||
        seen.has(check.name) || (check.status !== 'PASS' && check.status !== 'FAIL') ||
        !Object.hasOwn(check, 'observed')) {
      return invalid('preflight evidence control set is invalid')
    }
    seen.add(check.name)
    checks.push({ name: check.name, status: check.status, observed: check.observed })
  }
  if (seen.size !== knownChecks.size) return invalid('preflight evidence control set is incomplete')
  const derived = checks.every(check => check.status === 'PASS') ? 'PASS' : 'FAIL'
  if (status !== 'UNKNOWN' && status !== derived) return invalid('preflight verdict disagrees with its controls')

  const { verifier_commit, deployed_commit, command_id, observed_at, age_seconds, workload_images } = value
  if (typeof verifier_commit !== 'string' || !commit.test(verifier_commit) ||
      typeof deployed_commit !== 'string' || !commit.test(deployed_commit) ||
      typeof command_id !== 'string' || command_id === '' || !observedInstant(observed_at) || !workloadImages(workload_images)) {
    return invalid('preflight evidence identity is incomplete')
  }
  if (age_seconds !== undefined && !seconds(age_seconds)) return invalid('preflight evidence age is invalid')
  if (status !== 'UNKNOWN' && (age_seconds ?? 0) > value.max_age_seconds) return invalid('preflight evidence is stale')
  return {
    status, ...(typeof reason === 'string' && { reason }), observed_at, age_seconds,
    max_age_seconds: value.max_age_seconds, verifier_commit, deployed_commit, command_id, checks, workload_images,
  }
}

export const preflight = {
  async status(): Promise<PreflightStatus> {
    return readStatus(await api.get<unknown>('/api/preflight'))
  },
}
