import { beforeEach, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import UsersView from './UsersView.vue'
import { api } from '../api/client'
import { users, type UserAccess } from '../api/users'

const alice: UserAccess = { subject: 'alice', tenant: 'acme', roles: ['kanz-user'], portfolios: ['pf-1'], status: 'active', revision: '7', session_epoch: '0', created_by: 'admin', created_at: '2026-09-01T00:00:00Z', updated_at: '2026-09-28T00:00:00Z' }
beforeEach(() => {
  vi.restoreAllMocks()
  vi.spyOn(users, 'permissions').mockResolvedValue({ subject: 'admin', tenant: 'acme', identity_admin: true })
  vi.spyOn(users, 'gatewayPermissions').mockResolvedValue({ subject: 'admin', tenant: 'acme', capabilities: ['read'], portfolios: [], routes: [] })
  vi.spyOn(users, 'list').mockResolvedValue({ users: [alice], next_cursor: '' })
})
it('renders server permissions and submits the revision that was reviewed', async () => {
  const update = vi.spyOn(users, 'update').mockResolvedValue({ ...alice, revision: '8' })
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
const authority = { subject: 'alice', tenant: 'acme', identity_admin: false }
const gateway = { subject: 'alice', tenant: 'acme', capabilities: [], portfolios: [], routes: [] }

it.each([
  { subject: 'someone-else', capabilities: ['unrelated'] },
  { tenant: 'other-tenant', portfolios: ['unrelated'] },
  { capabilities: [42] },
  { portfolios: null },
  { routes: [{ pattern: '/unrelated', capability: 'read' }] },
])('withholds mismatched or malformed gateway grants', async difference => {
  vi.restoreAllMocks()
  vi.spyOn(api, 'get').mockResolvedValueOnce(authority).mockResolvedValueOnce({ ...gateway, ...difference })
  const wrapper = mount(UsersView)
  await flushPromises()
  expect(wrapper.text()).toContain('alice · Tenant acme')
  expect(wrapper.get('[role="alert"]').text()).toContain('Gateway permissions unavailable')
  expect(wrapper.text()).not.toContain('Gateway capabilities:')
  expect(wrapper.text()).not.toContain('unrelated')
})

it.each([{}, { ...authority, identity_admin: 'true' }, { ...authority, tenant: '' }])('requires a verified identity permission response before loading permissions or users', async body => {
  vi.restoreAllMocks()
  const get = vi.spyOn(api, 'get').mockResolvedValue(body)
  const wrapper = mount(UsersView)
  await flushPromises()
  expect(get).toHaveBeenCalledExactlyOnceWith('/auth/permissions')
  expect(wrapper.get('[role="alert"]').text()).toContain('Identity permissions are invalid')
  expect(wrapper.find('table').exists()).toBe(false)
})

it.each([{ routes: [] }, { routes: [{ Pattern: 'GET /api/v1/permissions', Capability: 'read' }] }])('renders the gateway wire format and empty grant lists', async ({ routes }) => {
  vi.restoreAllMocks()
  const capabilities = routes.length ? ['read'] : []
  vi.spyOn(api, 'get').mockResolvedValueOnce(authority).mockResolvedValueOnce({ ...gateway, capabilities, routes })
  const wrapper = mount(UsersView)
  await flushPromises()
  expect(wrapper.find('[role="alert"]').exists()).toBe(false)
  expect(wrapper.text()).toContain('Gateway capabilities: ' + (routes.length ? 'read' : 'none'))
  expect(wrapper.text()).toContain('Portfolio claims: none asserted')
  expect(wrapper.findAll('details li').map(row => row.text())).toEqual(routes.map(route => route.Pattern + ' · ' + route.Capability))
})
it('rejects a mixed-tenant first page as a whole and permits a fresh empty read', async () => {
  vi.mocked(users.list).mockRestore()
  const foreign = { ...alice, tenant: 'other-tenant', subject: 'foreign-user' }
  vi.spyOn(api, 'get').mockResolvedValueOnce({ users: [alice, foreign], next_cursor: '' })
    .mockResolvedValueOnce({ users: [], next_cursor: '' })
  const wrapper = mount(UsersView)
  await flushPromises()
  expect(wrapper.get('[role="alert"]').text()).toContain('verified account scope')
  expect(wrapper.find('table').exists()).toBe(false)
  expect(wrapper.text()).not.toContain('foreign-user')
  expect(wrapper.text()).not.toContain('No users found.')
  await wrapper.get('button').trigger('click'); await flushPromises()
  expect(wrapper.text()).toContain('No users found.')
  wrapper.unmount()
})

it('keeps an authenticated directory page on a rejected continuation and retries the same cursor', async () => {
  vi.mocked(users.list).mockRestore()
  // UsersFor returns 25 ordered accounts on a full page. The last subject is the cursor.
  const first = Array.from({ length: 25 }, (_, i) => ({ ...alice, subject: 'user-' + String(i).padStart(2, '0') }))
  const next = { ...alice, subject: 'user-25' }
  const foreign = { ...alice, subject: 'user-26', tenant: 'unrelated' }
  const get = vi.spyOn(api, 'get').mockResolvedValueOnce({ users: first, next_cursor: 'user-24' })
    .mockResolvedValueOnce({ users: [next, foreign], next_cursor: '' })
    .mockResolvedValueOnce({ users: [next], next_cursor: '' })
  const wrapper = mount(UsersView)
  await flushPromises()
  const more = wrapper.findAll('button').find(b => b.text() === 'Load more users')!
  await more.trigger('click'); await flushPromises()
  expect(wrapper.get('[role="alert"]').text()).toContain('verified account scope')
  expect(wrapper.findAll('tbody tr')).toHaveLength(25)
  expect(wrapper.find('[aria-label="Edit access for user-24"]').exists()).toBe(true)
  expect(wrapper.text()).not.toContain('user-25')
  expect(wrapper.text()).not.toContain('user-26')
  await more.trigger('click'); await flushPromises()
  expect(get.mock.calls.map(([path]) => path)).toEqual(['/api/identity/users?after=', '/api/identity/users?after=user-24', '/api/identity/users?after=user-24'])
  expect(wrapper.findAll('tbody tr')).toHaveLength(26)
  expect(wrapper.find('[aria-label="Edit access for user-25"]').exists()).toBe(true)
  expect(wrapper.text()).not.toContain('Load more users')
  wrapper.unmount()
})

it('does not invent permissions when the gateway is unavailable', async () => {
  vi.mocked(users.gatewayPermissions).mockRejectedValue(new Error('service unavailable'))
  const wrapper = mount(UsersView); await flushPromises()
  expect(wrapper.text()).toContain('Gateway permissions unavailable')
  expect(wrapper.text()).not.toContain('Gateway capabilities:')
})
