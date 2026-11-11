import { api } from './client'

export type PreflightState = 'PASS' | 'FAIL' | 'UNKNOWN'

export interface PreflightCheck {
  name: string
  status: PreflightState
  observed?: unknown
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

export const preflight = {
  status: () => api.get<PreflightStatus>('/api/preflight'),
}
