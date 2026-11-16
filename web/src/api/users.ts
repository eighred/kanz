import { api } from './client'
import { isRevision, requireRevision } from './revision'

export interface UserAccess {
  subject: string
  tenant: string
  roles: string[]
  portfolios: string[]
  status: 'active' | 'disabled'
  revision: string
  session_epoch: string
  created_at: string
  updated_at: string
  created_by: string
}
export interface IdentityPermissions { subject: string; tenant: string; identity_admin: boolean }
export interface GatewayPermissions {
  subject: string
  tenant: string
  capabilities: string[]
  portfolios: string[]
  routes: { Pattern: string; Capability: string }[]
}
const path = (subject: string) => `/api/identity/users/${encodeURIComponent(subject)}`

function access(value: unknown): UserAccess {
  if (!value || typeof value !== 'object') throw new Error('Identity returned an invalid account.')
  const row = value as Record<string, unknown>
  if (typeof row.subject !== 'string' || typeof row.tenant !== 'string' ||
      !Array.isArray(row.roles) || !row.roles.every(v => typeof v === 'string') ||
      !Array.isArray(row.portfolios) || !row.portfolios.every(v => typeof v === 'string') ||
      !['active', 'disabled'].includes(row.status as string) ||
      !isRevision(row.revision) || !isRevision(row.session_epoch) ||
      typeof row.created_at !== 'string' || typeof row.updated_at !== 'string' || typeof row.created_by !== 'string') {
    throw new Error('Identity returned an invalid account or integer representation; reload before editing.')
  }
  return row as unknown as UserAccess
}

export const users = {
  permissions: () => api.get<IdentityPermissions>('/auth/permissions'),
  gatewayPermissions: () => api.get<GatewayPermissions>('/api/v1/permissions'),
  list: async (after = '') => {
    const body = await api.get<unknown>(`/api/identity/users?after=${encodeURIComponent(after)}`)
    if (!body || typeof body !== 'object') throw new Error('Identity returned an invalid directory page.')
    const page = body as Record<string, unknown>
    if (!Array.isArray(page.users) || typeof page.next_cursor !== 'string') throw new Error('Identity returned an invalid directory page.')
    return { users: page.users.map(access), next_cursor: page.next_cursor }
  },
  update: async (user: UserAccess, roles: string[], portfolios: string[]) =>
    access(await api.put<unknown>(`${path(user.subject)}/access`, { revision: requireRevision(user.revision), roles, portfolios })),
  status: (subject: string, status: 'active' | 'disabled') => api.post(`${path(subject)}/${status === 'active' ? 'enable' : 'disable'}`),
}
