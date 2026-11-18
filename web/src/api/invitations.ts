import { api } from './client'
import { isRevision, requireRevision } from './revision'

export interface InvitationSummary {
  invite_id: string
  subject: string
  tenant: string
  roles: string[]
  portfolios: string[]
  created_by: string
  expires_at: string
  redeemable: boolean
  state: 'pending' | 'accepted' | 'expired' | 'revoked'
  revision: string
  revoked_by?: string
  revoked_at?: string
  redeemed_at?: string
  reissued_as?: string
}

export interface CreatedInvitation extends InvitationSummary {
  invite_token: string
  note?: string
}

export interface CreateInvitation {
  subject: string
  roles: string[]
  portfolios: string[]
}

function summary(value: unknown): InvitationSummary {
  if (!value || typeof value !== 'object') throw new Error('Identity returned an invalid invitation.')
  const row = value as Record<string, unknown>
  if (typeof row.invite_id !== 'string' || typeof row.subject !== 'string' || typeof row.tenant !== 'string' ||
      !Array.isArray(row.roles) || !row.roles.every(v => typeof v === 'string') ||
      !Array.isArray(row.portfolios) || !row.portfolios.every(v => typeof v === 'string') ||
      typeof row.created_by !== 'string' || typeof row.expires_at !== 'string' || typeof row.redeemable !== 'boolean' ||
      !['pending', 'accepted', 'expired', 'revoked'].includes(row.state as string) || !isRevision(row.revision) ||
      ['revoked_by', 'revoked_at', 'redeemed_at', 'reissued_as'].some(k => row[k] !== undefined && typeof row[k] !== 'string')) {
    throw new Error('Identity returned an invalid invitation or revision; reload before editing.')
  }
  return row as unknown as InvitationSummary
}

type Issuer = { subject: string; tenant: string }

function sameGrants(received: string[], reviewed: string[]): boolean {
  const expected = [...reviewed].sort()
  return received.length === expected.length && [...received].sort().every((grant, i) => grant === expected[i])
}

function issued(value: unknown, request: CreateInvitation, issuer: Issuer, previousId?: string): CreatedInvitation {
  const row = summary(value) as CreatedInvitation
  if (typeof row.invite_token !== 'string' || !row.invite_token || row.note !== undefined && typeof row.note !== 'string') {
    throw new Error('Identity returned an invalid invitation response.')
  }
  if (!row.invite_id.trim() || row.invite_id === previousId || row.subject !== request.subject ||
      row.tenant !== issuer.tenant || row.created_by !== issuer.subject || row.state !== 'pending' || !row.redeemable ||
      row.redeemed_at || row.revoked_at || row.reissued_as || !Number.isFinite(Date.parse(row.expires_at)) ||
      !sameGrants(row.roles, request.roles) || !sameGrants(row.portfolios, request.portfolios)) {
    throw new Error('The invitation receipt does not match the reviewed offer.')
  }
  return row
}

export const invitations = {
  list: async (): Promise<InvitationSummary[]> => {
    const body = await api.get<unknown>('/api/identity/invites')
    if (!Array.isArray(body)) {
      throw new Error('identity returned something other than an invitation list')
    }
    return body.map(summary)
  },
  create: async (request: CreateInvitation, issuer: Issuer) =>
    issued(await api.post<unknown>('/api/identity/invites', request), request, issuer),
  revoke: async (invite: InvitationSummary) => summary(await api.post<unknown>(`/api/identity/invites/${encodeURIComponent(invite.invite_id)}/revoke`, { revision: requireRevision(invite.revision) })),
  reissue: async (invite: InvitationSummary, issuer: Issuer) => {
    if (invite.tenant !== issuer.tenant) throw new Error('The reviewed invitation belongs to another tenant.')
    const body = await api.post<unknown>(`/api/identity/invites/${encodeURIComponent(invite.invite_id)}/reissue`, { revision: requireRevision(invite.revision) })
    return issued(body, invite, issuer, invite.invite_id)
  },
}
