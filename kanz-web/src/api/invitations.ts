import { api } from './client'

export interface InvitationSummary {
  invite_id: string
  subject: string
  tenant: string
  roles: string[]
  portfolios: string[]
  created_by: string
  expires_at: string
  redeemable: boolean
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

export const invitations = {
  list: async (): Promise<InvitationSummary[]> => {
    const body = await api.get<unknown>('/api/identity/invites')
    if (!Array.isArray(body)) {
      throw new Error('identity returned something other than an invitation list')
    }
    return body as InvitationSummary[]
  },
  create: (request: CreateInvitation) =>
    api.post<CreatedInvitation>('/api/identity/invites', request),
}
