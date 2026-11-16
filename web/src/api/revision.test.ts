import { beforeEach, expect, it, vi } from 'vitest'
import { api } from './client'
import { users, type UserAccess } from './users'
import { invitations, type InvitationSummary } from './invitations'
import { isRevision } from './revision'

const account: UserAccess = { subject: 'alice', tenant: 'acme', roles: ['kanz-user'], portfolios: [], status: 'active', revision: '9007199254740993', session_epoch: '9007199254740992', created_at: '2026-09-01T00:00:00Z', updated_at: '2026-09-01T00:00:00Z', created_by: 'admin' }
const invitation: InvitationSummary = { invite_id: 'invite', subject: 'alice', tenant: 'acme', roles: ['kanz-user'], portfolios: [], created_by: 'admin', expires_at: '2099-01-01T00:00:00Z', redeemable: true, state: 'pending', revision: '9007199254740993' }
beforeEach(() => vi.restoreAllMocks())

it('retains adjacent large revisions and the signed upper boundary as decimal text', async () => {
  for (const revision of ['0', '9007199254740992', '9007199254740993', '9223372036854775807']) {
    const row = { ...account, revision }
    vi.spyOn(api, 'get').mockResolvedValueOnce({ users: [row], next_cursor: '' })
    const page = await users.list()
    const put = vi.spyOn(api, 'put').mockResolvedValueOnce(row)
    expect((await users.update(page.users[0]!, ['kanz-user'], [])).revision).toBe(revision)
    expect(put).toHaveBeenLastCalledWith('/api/identity/users/alice/access', { revision, roles: ['kanz-user'], portfolios: [] })
    vi.spyOn(api, 'get').mockResolvedValueOnce([{ ...invitation, revision }])
    const list = await invitations.list()
    const post = vi.spyOn(api, 'post').mockResolvedValueOnce({ ...invitation, revision })
    expect((await invitations.revoke(list[0]!)).revision).toBe(revision)
    expect(post).toHaveBeenLastCalledWith('/api/identity/invites/invite/revoke', { revision })
  }
})

it('refuses unsafe wire values and commands before any mutation request', async () => {
  for (const revision of [0, 7, 9007199254740992, -1, null, undefined, '', '-1', '+1', '01', ' 1', '1.0', '1e3', '9223372036854775808']) {
    expect(isRevision(revision)).toBe(false)
    vi.spyOn(api, 'get').mockResolvedValueOnce({ users: [{ ...account, revision }], next_cursor: '' })
    await expect(users.list()).rejects.toThrow()
    vi.spyOn(api, 'get').mockResolvedValueOnce([{ ...invitation, revision }])
    await expect(invitations.list()).rejects.toThrow()
    const put = vi.spyOn(api, 'put')
    const post = vi.spyOn(api, 'post')
    await expect(users.update({ ...account, revision } as unknown as UserAccess, ['kanz-user'], [])).rejects.toThrow()
    await expect(invitations.reissue({ ...invitation, revision } as unknown as InvitationSummary)).rejects.toThrow()
    expect(put).not.toHaveBeenCalled()
    expect(post).not.toHaveBeenCalled()
  }
})

it('also refuses a rounded or missing epoch and unsafe mutation responses', async () => {
  vi.spyOn(api, 'get').mockResolvedValueOnce({ users: [{ ...account, session_epoch: 7 }], next_cursor: '' })
  await expect(users.list()).rejects.toThrow()
  vi.spyOn(api, 'put').mockResolvedValueOnce({ ...account, revision: 7 })
  await expect(users.update(account, account.roles, account.portfolios)).rejects.toThrow()
  vi.spyOn(api, 'post').mockResolvedValueOnce({ ...invitation, revision: 7 })
  await expect(invitations.revoke(invitation)).rejects.toThrow()
})
