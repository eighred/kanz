import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import InvitationsView from './InvitationsView.vue'
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
  revision: 0,
}

beforeEach(() => {
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
    expect(reissue).toHaveBeenCalledExactlyOnceWith(existing)
    expect(wrapper.get('textarea').element.value).toContain('replacement-once')
    await wrapper.findAll('button').find(b => b.text() === 'I have transferred it')!.trigger('click')
    expect(wrapper.find('textarea').exists()).toBe(false)
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
    })
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
