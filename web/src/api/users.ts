import { api, ApiError } from './client'
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

const nonempty = (value: unknown): value is string => typeof value === 'string' && value.trim().length > 0
const strings = (value: unknown): value is string[] => Array.isArray(value) && value.every(nonempty)

function hasPrincipal(value: unknown): value is Record<string, unknown> & { subject: string; tenant: string } {
  return !!value && typeof value === 'object' && 'subject' in value && nonempty(value.subject) &&
    'tenant' in value && nonempty(value.tenant)
}

export class UnconfirmedAccessUpdate extends Error {
  constructor() { super('Identity did not confirm the reviewed account change. Reload users to check its outcome.') }
}

function sameGrants(actual: string[], requested: string[]): boolean {
  const ordered = [...requested].sort()
  return actual.length === ordered.length && [...actual].sort().every((grant, index) => grant === ordered[index])
}

export const users = {
  async permissions(): Promise<IdentityPermissions> {
    const body = await api.get<unknown>('/auth/permissions')
    if (!hasPrincipal(body) || typeof body.identity_admin !== 'boolean') throw new Error('Identity permissions are invalid.')
    return { subject: body.subject, tenant: body.tenant, identity_admin: body.identity_admin }
  },
  async gatewayPermissions(authority: IdentityPermissions): Promise<GatewayPermissions> {
    const body = await api.get<unknown>('/api/v1/permissions')
    if (!hasPrincipal(body) || body.subject !== authority.subject || body.tenant !== authority.tenant ||
        !strings(body.capabilities) || !strings(body.portfolios) || !Array.isArray(body.routes) ||
        !body.routes.every(route => route && nonempty(route.Pattern) && nonempty(route.Capability))) {
      throw new Error('The gateway returned invalid permissions for this account.')
    }
    return { subject: body.subject, tenant: body.tenant, capabilities: body.capabilities, portfolios: body.portfolios, routes: body.routes }
  },
  list: async (tenant: string, after = '') => {
    const body = await api.get<unknown>(`/api/identity/users?after=${encodeURIComponent(after)}`)
    if (!body || typeof body !== 'object') throw new Error('Identity returned an invalid directory page.')
    const page = body as Record<string, unknown>
    if (!Array.isArray(page.users) || typeof page.next_cursor !== 'string') throw new Error('Identity returned an invalid directory page.')
    const accounts = page.users.map(access)
    if (accounts.some(account => !nonempty(account.subject) || account.tenant !== tenant)) {
      throw new Error('Identity returned a directory page outside the verified account scope.')
    }
    return { users: accounts, next_cursor: page.next_cursor }
  },
  async update(user: UserAccess, roles: string[], portfolios: string[]) {
    const revision = requireRevision(user.revision)
    const epoch = requireRevision(user.session_epoch)
    let result: UserAccess
    try {
      result = access(await api.put<unknown>(`${path(user.subject)}/access`, { revision, roles, portfolios }, 200))
    } catch (e) {
      if (e instanceof ApiError && e.status < 500) throw e
      throw new UnconfirmedAccessUpdate()
    }
    if (result.subject !== user.subject || result.tenant !== user.tenant || result.status !== user.status ||
        BigInt(result.revision) !== BigInt(revision) + 1n || BigInt(result.session_epoch) !== BigInt(epoch) + 1n ||
        !sameGrants(result.roles, roles) || !sameGrants(result.portfolios, portfolios)) {
      throw new UnconfirmedAccessUpdate()
    }
    return result
  },
  status: (subject: string, status: 'active' | 'disabled') => api.post(`${path(subject)}/${status === 'active' ? 'enable' : 'disable'}`),
}
