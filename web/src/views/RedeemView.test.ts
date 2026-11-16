import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createRouter, createWebHistory } from 'vue-router'
import RedeemView from './RedeemView.vue'
import * as client from '../api/client'
import { ApiError } from '../api/client'

// THE TWO CLAIMS THIS SCREEN MAKES ARE CORRECTNESS CLAIMS, NOT COSMETIC ONES,
// so they are tested rather than asserted in a comment:
//
//   1. the invitation token is removed from the URL on arrival;
//   2. the form cannot be submitted with mismatched passwords.
//
// Both matter because the invitation is SINGLE USE. A token left in the address
// bar persists in history and in the Referer of anything the page loads; a
// mistyped password would create the account with a value the invitee does not
// know and spend the invitation doing it.

const identity = { subject: 'user:dana', tenant: 'acme', expires_at: '2099-01-01T00:00:00Z' }

function router() {
  return createRouter({
    history: createWebHistory(),
    routes: [
      { path: '/redeem', name: 'redeem', component: RedeemView },
      { path: '/overview', name: 'overview', component: { template: '<div />' } },
      { path: '/login', name: 'login', component: { template: '<div />' } },
    ],
  })
}

async function open(query = '') {
  const r = router()
  await r.push('/redeem' + query)
  await r.isReady()
  const wrapper = mount(RedeemView, { global: { plugins: [r] } })
  // onMounted starts a router.replace to strip the token; a navigation is
  // asynchronous, so the assertions below must wait for it to land rather than
  // race it.
  await flushPromises()
  return { wrapper, r }
}

/**
 * passwords returns the two password fields, failing loudly if the form no
 * longer renders both. Indexing straight into findAll() would give `undefined`
 * and a confusing failure three lines later; a named error says the FORM
 * changed, not the assertion.
 */
function passwords(wrapper: ReturnType<typeof mount>) {
  const fields = wrapper.findAll('input[type="password"]')
  const [chosen, confirmed] = fields
  if (!chosen || !confirmed) {
    throw new Error(`expected two password fields, found ${fields.length}`)
  }
  return { chosen, confirmed }
}

beforeEach(() => {
  setActivePinia(createPinia())
})

describe('the invitation token', () => {
  it('is taken from the link and then removed from the URL', async () => {
    const { wrapper, r } = await open('?token=a-single-use-token')

    // It reached the form...
    const token = wrapper.find('input[placeholder*="token"]')
    expect((token.element as HTMLInputElement).value).toBe('a-single-use-token')

    // ...and it is no longer in the address bar. Left there it persists in
    // history and in the Referer of every request this page makes. Both the
    // router's view and the real URL are checked: the router could agree with
    // itself while the browser bar still showed the token.
    expect(r.currentRoute.value.query.token).toBeUndefined()
    expect(window.location.search).not.toContain('a-single-use-token')
  })

  it('can be pasted when there is no link, so an invitation still works without one', async () => {
    const { wrapper } = await open()
    const token = wrapper.find('input[placeholder*="token"]')
    expect((token.element as HTMLInputElement).value).toBe('')
    expect(token.exists()).toBe(true)
  })
})

describe('the confirmation field', () => {
  it('blocks submission while the two passwords differ', async () => {
    const redeem = vi.spyOn(client.auth, 'redeem').mockResolvedValue(identity)
    const { wrapper } = await open('?token=t')

    const { chosen, confirmed } = passwords(wrapper)
    await chosen.setValue('a-properly-long-passphrase')
    await confirmed.setValue('a-properly-long-passphraze') // one letter out
    await wrapper.vm.$nextTick()

    expect(wrapper.find('button[type="submit"]').attributes('disabled')).toBeDefined()
    await wrapper.find('form').trigger('submit.prevent')
    expect(redeem).not.toHaveBeenCalled()
  })

  it('blocks submission while the password is under the server minimum', async () => {
    const redeem = vi.spyOn(client.auth, 'redeem').mockResolvedValue(identity)
    const { wrapper } = await open('?token=t')

    const { chosen, confirmed } = passwords(wrapper)
    await chosen.setValue('short')
    await confirmed.setValue('short')
    await wrapper.vm.$nextTick()

    expect(wrapper.find('button[type="submit"]').attributes('disabled')).toBeDefined()
    await wrapper.find('form').trigger('submit.prevent')
    expect(redeem).not.toHaveBeenCalled()
  })

  it('submits once both match and are long enough', async () => {
    const redeem = vi.spyOn(client.auth, 'redeem').mockResolvedValue(identity)
    const { wrapper, r } = await open('?token=a-single-use-token')

    const { chosen, confirmed } = passwords(wrapper)
    await chosen.setValue('a-properly-long-passphrase')
    await confirmed.setValue('a-properly-long-passphrase')
    await wrapper.vm.$nextTick()

    await wrapper.find('form').trigger('submit.prevent')
    await flushPromises()
    expect(redeem).toHaveBeenCalledWith('a-single-use-token', 'a-properly-long-passphrase')
    expect(r.currentRoute.value.path).toBe('/overview')
  })
})

describe('refusals the invitee has to understand', () => {
  it('reports a too-short credential as a password problem, NOT as a dead invitation', async () => {
    // This is the defect that made the fix necessary: a 400 collapsed into the
    // invitation message sends someone to ask for a new invitation over a typo.
    vi.spyOn(client.auth, 'redeem').mockRejectedValue(
      new ApiError(400, 'a credential must be at least 12 characters'),
    )
    const { wrapper } = await open('?token=t')

    const { chosen, confirmed } = passwords(wrapper)
    await chosen.setValue('a-properly-long-passphrase')
    await confirmed.setValue('a-properly-long-passphrase')
    await wrapper.find('form').trigger('submit.prevent')
    await new Promise((r) => setTimeout(r, 0))
    await wrapper.vm.$nextTick()

    const alert = wrapper.find('[role="alert"]').text()
    expect(alert).toContain('at least 12 characters')
    expect(alert).not.toMatch(/invitation is not valid/i)
  })

  it('reports a genuinely invalid invitation as one', async () => {
    vi.spyOn(client.auth, 'redeem').mockRejectedValue(new ApiError(401, 'that invitation is not valid'))
    const { wrapper } = await open('?token=t')

    const { chosen, confirmed } = passwords(wrapper)
    await chosen.setValue('a-properly-long-passphrase')
    await confirmed.setValue('a-properly-long-passphrase')
    await wrapper.find('form').trigger('submit.prevent')
    await new Promise((r) => setTimeout(r, 0))
    await wrapper.vm.$nextTick()

    expect(wrapper.find('[role="alert"]').text()).toMatch(/expired or already been used/i)
  })
})
