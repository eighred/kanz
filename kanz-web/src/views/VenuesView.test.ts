import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import VenuesView from './VenuesView.vue'
import * as api from '../api/control'
import { ApiError } from '../api/client'
import type { VenueKeyStatus } from '../api/control'

// THE MOST DANGEROUS BODY ON THIS SURFACE, and the gateway says so beside its own
// handler: an exchange API key and secret. What is tested here is what a mistake
// costs rather than what the form looks like:
//
//   1. a key is never rendered back — a surface that can display one can leak one;
//   2. replacing the keys of a venue that is ALREADY trading needs the venue
//      named, because a wrong key there is an outage on the capital path;
//   3. the credential leaves the component when the interaction does;
//   4. a venue's refusal (412) is reported as the VENUE's, not as a form error.

const notSet: VenueKeyStatus = { venue: 'binance' }
const configured: VenueKeyStatus = { venue: 'okx', configured: true }

async function open(rows: VenueKeyStatus[]) {
  vi.spyOn(api.control, 'venues').mockResolvedValue(rows)
  const wrapper = mount(VenuesView)
  await flushPromises()
  return wrapper
}

function button(wrapper: Awaited<ReturnType<typeof open>>, label: string) {
  return wrapper.findAll('button').find((b) => b.text() === label)
}

beforeEach(() => vi.restoreAllMocks())

describe('a key is never rendered back', () => {
  it('shows presence only, never material', async () => {
    const wrapper = await open([configured])
    expect(wrapper.text()).toContain('configured')
    // There is no read path at all — the API returns no key — so the strongest
    // available assertion is that the secret inputs are masked and empty.
    const secret = wrapper.find('input[name="api-secret"]')
    expect(secret.exists()).toBe(false) // no form open yet
  })

  it('masks every credential field when the form opens', async () => {
    const wrapper = await open([notSet])
    await button(wrapper, 'Set keys')!.trigger('click')

    for (const name of ['api-key', 'api-secret', 'passphrase']) {
      const input = wrapper.find(`input[name="${name}"]`)
      expect(input.attributes('type')).toBe('password')
      // A browser offering to save an exchange secret is the credential leaving
      // the operator's control by a route nobody chose.
      expect(input.attributes('autocomplete')).toBe('off')
      expect((input.element as HTMLInputElement).value).toBe('')
    }
  })
})

describe('replacing live credentials asks for the venue name; setting them does not', () => {
  it('writes a first-time credential without extra friction', async () => {
    const set = vi.spyOn(api.control, 'setVenueKeys').mockResolvedValue({ exchange_account_id: 'acct-1' })
    const wrapper = await open([notSet])

    await button(wrapper, 'Set keys')!.trigger('click')
    await wrapper.find('input[name="api-key"]').setValue('k')
    await wrapper.find('input[name="api-secret"]').setValue('s')
    // No confirm-name field for a venue that has nothing to overwrite.
    expect(wrapper.find('input[name="confirm-venue"]').exists()).toBe(false)

    await button(wrapper, 'Set credentials')!.trigger('click')
    await flushPromises()
    expect(set).toHaveBeenCalledExactlyOnceWith('binance', 'k', 's', '')
  })

  it('refuses to replace until the venue is named', async () => {
    const set = vi.spyOn(api.control, 'setVenueKeys').mockResolvedValue({})
    const wrapper = await open([configured])

    await button(wrapper, 'Replace keys')!.trigger('click')
    await wrapper.find('input[name="api-key"]').setValue('k')
    await wrapper.find('input[name="api-secret"]').setValue('s')
    expect(button(wrapper, 'Replace credentials')!.attributes('disabled')).toBeDefined()

    await wrapper.find('input[name="confirm-venue"]').setValue('binance') // wrong venue
    expect(button(wrapper, 'Replace credentials')!.attributes('disabled')).toBeDefined()

    await button(wrapper, 'Replace credentials')!.trigger('click')
    await flushPromises()
    expect(set).not.toHaveBeenCalled()

    await wrapper.find('input[name="confirm-venue"]').setValue('okx')
    await button(wrapper, 'Replace credentials')!.trigger('click')
    await flushPromises()
    expect(set).toHaveBeenCalledExactlyOnceWith('okx', 'k', 's', '')
  })

  it('will not submit an empty key or secret', async () => {
    const set = vi.spyOn(api.control, 'setVenueKeys').mockResolvedValue({})
    const wrapper = await open([notSet])

    await button(wrapper, 'Set keys')!.trigger('click')
    await wrapper.find('input[name="api-key"]').setValue('k')
    // secret still empty
    await button(wrapper, 'Set credentials')!.trigger('click')
    await flushPromises()
    expect(set).not.toHaveBeenCalled()
  })
})

describe('the credential does not outlive the interaction', () => {
  it('clears the fields on cancel', async () => {
    const wrapper = await open([notSet])
    await button(wrapper, 'Set keys')!.trigger('click')
    await wrapper.find('input[name="api-secret"]').setValue('super-secret')
    await button(wrapper, 'Cancel')!.trigger('click')

    // Re-opening must not present the previous secret.
    await button(wrapper, 'Set keys')!.trigger('click')
    expect((wrapper.find('input[name="api-secret"]').element as HTMLInputElement).value).toBe('')
  })

  it('clears the fields after a successful write, and never echoes the secret', async () => {
    vi.spyOn(api.control, 'setVenueKeys').mockResolvedValue({ exchange_account_id: 'acct-9' })
    const wrapper = await open([notSet])

    await button(wrapper, 'Set keys')!.trigger('click')
    await wrapper.find('input[name="api-key"]').setValue('k')
    await wrapper.find('input[name="api-secret"]').setValue('super-secret')
    await button(wrapper, 'Set credentials')!.trigger('click')
    await flushPromises()

    expect(wrapper.text()).not.toContain('super-secret')
    // The exchange's own account id IS shown: it is a public fact and the only
    // evidence the caller gets that the credential actually works.
    expect(wrapper.find('[role="status"]').text()).toContain('acct-9')
  })
})

describe("a venue's refusal is reported as the venue's", () => {
  it('explains a 412 without blaming the form, and keeps the secret typed', async () => {
    vi.spyOn(api.control, 'setVenueKeys').mockRejectedValue(new ApiError(412, 'precondition failed'))
    const wrapper = await open([notSet])

    await button(wrapper, 'Set keys')!.trigger('click')
    await wrapper.find('input[name="api-key"]').setValue('k')
    await wrapper.find('input[name="api-secret"]').setValue('super-secret')
    await button(wrapper, 'Set credentials')!.trigger('click')
    await flushPromises()

    expect(wrapper.find('[role="alert"]').text()).toMatch(/venue refused the credential/i)
    // Retyping a 64-character key because the venue was briefly unreachable is
    // how people start pasting credentials into a text editor first.
    expect((wrapper.find('input[name="api-secret"]').element as HTMLInputElement).value)
      .toBe('super-secret')
  })
})
