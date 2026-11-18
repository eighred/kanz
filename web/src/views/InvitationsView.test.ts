import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import InvitationsView from './InvitationsView.vue'
import { createPinia, setActivePinia } from 'pinia'
import { useSession } from '../stores/session'
import { api } from '../api/client'
import * as invitationApi from '../api/invitations'

const existing = {
  invite_id: 'invite-1',
  subject: 'user:existing',
  tenant: 'acme',
  roles: ['kanz-trader'],
  portfolios: ['PF1'],
  created_by: 'user:operator',
  expires_at: '2099-01-01T00:00:00Z',
  redeemable: true,
  state: 'pending' as const,
  revision: '0',
}

const issuer = { subject: 'user:operator', tenant: 'acme' }
beforeEach(() => {
  setActivePinia(createPinia())
  useSession().identity = { ...issuer, expires_at: '2099-01-01T00:00:00Z' }
  vi.spyOn(invitationApi.invitations, 'list').mockResolvedValue([existing])
})

describe('operator invitation creation', () => {
  it('reviews a reissue before submitting its exact revision and displays the new token once', async () => {
    const reissue = vi.spyOn(invitationApi.invitations, 'reissue').mockResolvedValue({ ...existing, invite_id: 'replacement', invite_token: 'replacement-once' })
    const wrapper = mount(InvitationsView); await flushPromises()
    await wrapper.get('[aria-label="Reissue invitation invite-1"]').trigger('click')
    expect(reissue).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('The old token will stop working')
    const confirm = wrapper.findAll('button').find(b => b.text() === 'Confirm reissue')!
    await confirm.trigger('click'); await flushPromises()
    expect(reissue).toHaveBeenCalledExactlyOnceWith(existing, issuer)
    expect(wrapper.get('textarea').element.value).toContain('replacement-once')
    await wrapper.findAll('button').find(b => b.text() === 'I have transferred it')!.trigger('click')
    expect(wrapper.find('textarea').exists()).toBe(false)
  })

  it.each([
    { subject: 'other' }, { tenant: 'other' }, { created_by: 'someone-else' }, { roles: ['other'] },
    { portfolios: [] }, { state: 'expired' }, { redeemable: false }, { invite_id: existing.invite_id },
  ])('withholds the reissued link and confirmation on a mismatched receipt', async changed => {
    const post = vi.spyOn(api, 'post').mockResolvedValue({ ...existing, invite_id: 'replacement', invite_token: 'fixture-only', ...changed })
    const wrapper = mount(InvitationsView)
    await flushPromises()
    await wrapper.get('[aria-label="Reissue invitation invite-1"]').trigger('click')
    await wrapper.findAll('button').find(b => b.text() === 'Confirm reissue')!.trigger('click')
    await flushPromises()
    expect(post).toHaveBeenCalledExactlyOnceWith('/api/identity/invites/invite-1/reissue', { revision: '0' })
    expect(wrapper.get('[role=alert]').text()).toContain('does not match')
    expect(wrapper.find('.secret-panel').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('Invitation reissued.')
  })

  it('binds create to the typed subject and grants without sending issuer claims', async () => {
    const post = vi.spyOn(api, 'post').mockResolvedValueOnce({ ...existing, invite_token: 'fixture-only' })
      .mockResolvedValueOnce({ ...existing, roles: ['another-role'], invite_token: 'fixture-only' })
    const request = { subject: existing.subject, roles: existing.roles, portfolios: existing.portfolios }
    await expect(invitationApi.invitations.create(request, issuer)).resolves.toMatchObject({ invite_id: existing.invite_id })
    await expect(invitationApi.invitations.create(request, issuer)).rejects.toThrow('does not match')
    expect(post.mock.calls).toEqual([['/api/identity/invites', request], ['/api/identity/invites', request]])
  })

  it('accepts a new ID for the same reviewed grant and refuses a foreign target before reissue', async () => {
    const reviewed = { ...existing, roles: ['kanz-user', 'kanz-trader'], created_by: 'another-operator' }
    const post = vi.spyOn(api, 'post').mockResolvedValue({ ...reviewed, invite_id: 'new-id', created_by: issuer.subject,
      roles: ['kanz-trader', 'kanz-user'], invite_token: 'fixture-only' })
    await expect(invitationApi.invitations.reissue(reviewed, issuer)).resolves.toMatchObject({ invite_id: 'new-id' })
    await expect(invitationApi.invitations.reissue({ ...reviewed, tenant: 'foreign' }, issuer)).rejects.toThrow('another tenant')
    expect(post).toHaveBeenCalledExactlyOnceWith('/api/identity/invites/invite-1/reissue', { revision: '0' })
  })

  it('withholds a valid issuance that finishes after the issuing session changes', async () => {
    let finish!: (value: unknown) => void
    vi.spyOn(api, 'post').mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    const wrapper = mount(InvitationsView)
    await flushPromises()
    await wrapper.get('[aria-label="Reissue invitation invite-1"]').trigger('click')
    await wrapper.findAll('button').find(b => b.text() === 'Confirm reissue')!.trigger('click')
    useSession().forget()
    finish({ ...existing, invite_id: 'replacement', invite_token: 'fixture-only' })
    await flushPromises()
    expect(wrapper.find('.secret-panel').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('Invitation reissued.')
  })

  it('shows exact lifecycle states and prevents actions on accepted or replaced offers', async () => {
    vi.mocked(invitationApi.invitations.list).mockResolvedValue([
      existing,
      { ...existing, invite_id: 'accepted', state: 'accepted', redeemable: false },
      { ...existing, invite_id: 'expired', state: 'expired', redeemable: false },
      { ...existing, invite_id: 'revoked', state: 'revoked', redeemable: false },
      { ...existing, invite_id: 'replaced', state: 'revoked', reissued_as: 'new-id', redeemable: false },
    ])
    const wrapper = mount(InvitationsView); await flushPromises()
    for (const state of ['pending', 'accepted', 'expired', 'revoked']) expect(wrapper.text()).toContain(state)
    expect(wrapper.find('[aria-label="Reissue invitation accepted"]').exists()).toBe(false)
    expect(wrapper.find('[aria-label="Reissue invitation replaced"]').exists()).toBe(false)
    expect(wrapper.find('[aria-label="Revoke invitation revoked"]').exists()).toBe(false)
    expect(wrapper.find('[aria-label="Reissue invitation revoked"]').exists()).toBe(true)
  })

  it('does not retry a conflict or claim a failed revoke succeeded', async () => {
    const revoke = vi.spyOn(invitationApi.invitations, 'revoke').mockRejectedValue(new Error('invitation changed'))
    const wrapper = mount(InvitationsView); await flushPromises()
    await wrapper.get('[aria-label="Revoke invitation invite-1"]').trigger('click')
    await wrapper.findAll('button').find(b => b.text() === 'Confirm revoke')!.trigger('click'); await flushPromises()
    expect(revoke).toHaveBeenCalledTimes(1)
    expect(wrapper.get('[role="alert"]').text()).toContain('Reload invitations')
    expect(wrapper.text()).not.toContain('Invitation revoked.')
  })
  it('sends only the authority the operator entered and shows the one-time activation link', async () => {
    const create = vi.spyOn(invitationApi.invitations, 'create').mockResolvedValue({
      ...existing,
      invite_id: 'invite-2',
      subject: 'user:second-person',
      roles: ['kanz-order-approver', 'kanz-mandate-signatory'],
      invite_token: 'single-use-token',
    })
    const wrapper = mount(InvitationsView)
    await flushPromises()

    await wrapper.find('input[placeholder="user:person"]').setValue('user:second-person')
    await wrapper.find('input[placeholder*="role names"]').setValue(
      'kanz-order-approver, kanz-mandate-signatory',
    )
    await wrapper.find('input[placeholder*="portfolio IDs"]').setValue('PF1')
    await wrapper.find('form').trigger('submit.prevent')
    await flushPromises()

    expect(create).toHaveBeenCalledWith({
      subject: 'user:second-person',
      roles: ['kanz-order-approver', 'kanz-mandate-signatory'],
      portfolios: ['PF1'],
    }, issuer)
    // Tenant and created_by come from the verified operator token. If either
    // appears in this request, the browser has become a forgeable audit source.
    expect(create.mock.calls[0]?.[0]).not.toHaveProperty('tenant')
    expect(create.mock.calls[0]?.[0]).not.toHaveProperty('created_by')

    const secret = wrapper.find('.secret-panel')
    expect(secret.text()).toContain('cannot be recovered')
    expect((secret.find('textarea').element as HTMLTextAreaElement).value).toContain(
      '/redeem?token=single-use-token',
    )
  })

  it('does not create an account offer with no roles', async () => {
    const create = vi.spyOn(invitationApi.invitations, 'create')
    const wrapper = mount(InvitationsView)
    await flushPromises()

    await wrapper.find('input[placeholder="user:person"]').setValue('user:second-person')
    expect(wrapper.find('button[type="submit"]').attributes('disabled')).toBeDefined()
    await wrapper.find('form').trigger('submit.prevent')
    expect(create).not.toHaveBeenCalled()
  })

  it('keeps an unread invitation list distinct from an empty one', async () => {
    vi.mocked(invitationApi.invitations.list).mockRejectedValue(new Error('identity unavailable'))
    const wrapper = mount(InvitationsView)
    await flushPromises()

    expect(wrapper.find('[role="alert"]').text()).toContain('identity unavailable')
    expect(wrapper.text()).not.toContain('No invitation records were returned')
  })
})
