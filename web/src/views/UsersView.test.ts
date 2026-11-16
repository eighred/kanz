import { beforeEach, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import UsersView from './UsersView.vue'
import { users, type UserAccess } from '../api/users'

const alice: UserAccess = { subject: 'alice', tenant: 'acme', roles: ['kanz-user'], portfolios: ['pf-1'], status: 'active', revision: 7, created_by: 'admin', created_at: '2026-09-01T00:00:00Z', updated_at: '2026-09-28T00:00:00Z' }
beforeEach(() => {
  vi.restoreAllMocks()
  vi.spyOn(users, 'permissions').mockResolvedValue({ subject: 'admin', tenant: 'acme', identity_admin: true })
  vi.spyOn(users, 'gatewayPermissions').mockResolvedValue({ subject: 'admin', tenant: 'acme', capabilities: ['read'], portfolios: [], routes: [] })
  vi.spyOn(users, 'list').mockResolvedValue({ users: [alice], next_cursor: '' })
})
it('renders server permissions and submits the revision that was reviewed', async () => {
  const update = vi.spyOn(users, 'update').mockResolvedValue({ ...alice, revision: 8 })
  const wrapper = mount(UsersView); await flushPromises()
  expect(wrapper.text()).toContain('Gateway capabilities: read')
  expect(wrapper.text()).toContain('Identity administration: permitted')
  await wrapper.get('[aria-label="Edit access for alice"]').trigger('click')
  await wrapper.get('#access-roles').setValue('kanz-user, kanz-trader')
  await wrapper.get('#access-portfolios').setValue('pf-2')
  await wrapper.get('form').trigger('submit'); await flushPromises()
  expect(update).toHaveBeenCalledWith(alice, ['kanz-user', 'kanz-trader'], ['pf-2'])
  expect(wrapper.text()).toContain('previous sessions revoked')
})
it('keeps a conflicting edit visible without reporting success or retrying it', async () => {
  const update = vi.spyOn(users, 'update').mockRejectedValue(new Error('account access changed; reload before editing'))
  const wrapper = mount(UsersView); await flushPromises()
  await wrapper.get('[aria-label="Edit access for alice"]').trigger('click')
  await wrapper.get('form').trigger('submit'); await flushPromises()
  expect(wrapper.get('[role="alert"]').text()).toContain('reload before editing')
  expect(wrapper.find('form').exists()).toBe(true)
  expect(update).toHaveBeenCalledTimes(1)
  expect(wrapper.text()).not.toContain('Access updated')
})
it('does not fetch user records or display edit controls for a nonadministrator', async () => {
  vi.mocked(users.permissions).mockResolvedValue({ subject: 'alice', tenant: 'acme', identity_admin: false })
  const wrapper = mount(UsersView); await flushPromises()
  expect(users.list).not.toHaveBeenCalled()
  expect(wrapper.find('table').exists()).toBe(false)
  expect(wrapper.text()).toContain('Identity administration: not permitted')
})
it('does not invent permissions when the gateway is unavailable', async () => {
  vi.mocked(users.gatewayPermissions).mockRejectedValue(new Error('service unavailable'))
  const wrapper = mount(UsersView); await flushPromises()
  expect(wrapper.text()).toContain('Gateway permissions unavailable')
  expect(wrapper.text()).not.toContain('Gateway capabilities:')
})
