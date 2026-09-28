import { api } from './client'

export interface UserAccess {
  subject: string
  tenant: string
  roles: string[]
  portfolios: string[]
  status: 'active' | 'disabled'
  revision: number
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
export const users = {
  permissions: () => api.get<IdentityPermissions>('/auth/permissions'),
  gatewayPermissions: () => api.get<GatewayPermissions>('/api/v1/permissions'),
  list: (after = '') => api.get<{ users: UserAccess[]; next_cursor: string }>(`/api/identity/users?after=${encodeURIComponent(after)}`),
  update: (user: UserAccess, roles: string[], portfolios: string[]) =>
    api.put<UserAccess>(`${path(user.subject)}/access`, { revision: user.revision, roles, portfolios }),
  status: (subject: string, status: 'active' | 'disabled') => api.post(`${path(subject)}/${status === 'active' ? 'enable' : 'disable'}`),
}
