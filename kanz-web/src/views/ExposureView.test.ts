import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createRouter, createWebHistory } from 'vue-router'
import ExposureView from './ExposureView.vue'
import * as api from '../api/risk'
import { ApiError } from '../api/client'
import type { ExposureResponse } from '../api/risk'

// THE TWO CLAIMS THIS SCREEN MAKES ARE ABOUT MONEY, so they are tested:
//
//   1. no figure passes through a JS number, so a coefficient beyond 2^53
//      renders exactly rather than losing its last digits;
//   2. CURRENCY_EXCLUDED is presented as "these totals are missing positions",
//      not as a staleness hint — the proto says a gate that must not
//      under-report has to refuse such a response outright.
//
// Both are ways for a reader to act on a number that is wrong in a direction
// they cannot see.

function router() {
  return createRouter({
    history: createWebHistory(),
    routes: [
      { path: '/portfolios/:id/exposure', name: 'exposure', component: ExposureView },
      { path: '/portfolios', name: 'portfolios', component: { template: '<div />' } },
    ],
  })
}

async function open(resp: ExposureResponse | Error) {
  if (resp instanceof Error) vi.spyOn(api.risk, 'exposure').mockRejectedValue(resp)
  else vi.spyOn(api.risk, 'exposure').mockResolvedValue(resp)
  const r = router()
  await r.push('/portfolios/PF1/exposure')
  await r.isReady()
  const wrapper = mount(ExposureView, { global: { plugins: [r] } })
  await flushPromises()
  return wrapper
}

const base: ExposureResponse = {
  portfolio_id: 'PF1',
  as_of: '2026-08-11T00:00:00Z',
  set: {
    exposures: [
      {
        dimension: 'EXPOSURE_DIMENSION_ASSET_CLASS',
        bucket: 'EQUITY',
        // 2^53 + 1 with two decimal places: the first value a double cannot hold.
        gross: { amount: { coefficient: '900719925474099301', exponent: -2 }, currency_code: 'USD' },
        net: { amount: { coefficient: '-12345', exponent: -2 }, currency_code: 'USD' },
      },
    ],
  },
  quality_flags: [],
}

beforeEach(() => vi.restoreAllMocks())

describe('figures never pass through a JS number', () => {
  it('renders a coefficient beyond 2^53 exactly', async () => {
    const wrapper = await open(base)
    const text = wrapper.text()

    expect(text).toContain('9007199254740993.01 USD')
    // The failure this avoids, demonstrated: the same value through a double.
    expect(text).not.toContain('9007199254740993.02')
    expect(text).not.toContain('9007199254740992')
  })

  it('marks a negative net without parsing it as a number', async () => {
    const wrapper = await open(base)
    expect(wrapper.find('.state-bad').text()).toContain('-123.45')
  })

  // "—" NOT "0". An unrenderable amount must look unrenderable: a zero reads as
  // "flat" and is dropped from whatever is checked downstream, which is the rule
  // internal/dec states at the same boundary.
  it('renders an out-of-domain figure as unrenderable, never as zero', async () => {
    const wrapper = await open({
      ...base,
      set: {
        exposures: [
          {
            dimension: 'EXPOSURE_DIMENSION_CURRENCY',
            bucket: 'USD',
            gross: { amount: { coefficient: '1', exponent: 2_000_000_000 }, currency_code: 'USD' },
            net: { amount: { coefficient: '1', exponent: 0 }, currency_code: 'USD' },
          },
        ],
      },
    })
    const cells = wrapper.findAll('td').map((c) => c.text())
    expect(cells).toContain('—')
    expect(cells).not.toContain('0 USD')
  })
})

describe('a flag that changes what the totals mean is not a hint', () => {
  // The proto: positions in an unconvertible currency are OMITTED, so "a
  // concentration or exposure limit checked against them can pass when the full
  // book would breach". The number is smaller than the truth.
  it('presents CURRENCY_EXCLUDED as missing positions, with alert prominence', async () => {
    const wrapper = await open({ ...base, quality_flags: ['QUALITY_FLAG_CURRENCY_EXCLUDED'] })

    const alert = wrapper.find('[role="alert"]')
    expect(alert.exists()).toBe(true)
    expect(alert.text()).toMatch(/MISSING from these totals/i)
    expect(alert.text()).toMatch(/larger than what is shown/i)
  })

  // Staleness is a different claim: the numbers are complete but old. Rendering
  // it with the same prominence would flatten the difference between "old" and
  // "wrong in a direction you cannot see".
  it('does not give staleness the same prominence', async () => {
    const wrapper = await open({ ...base, quality_flags: ['QUALITY_FLAG_STALE'] })
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    expect(wrapper.text()).toMatch(/older than the request asked for/i)
  })

  it('says nothing when the response carries no flags', async () => {
    const wrapper = await open(base)
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    expect(wrapper.text()).not.toMatch(/MISSING from these totals/i)
  })
})

describe('a refusal is not an empty portfolio', () => {
  it('explains a 404 rather than drawing an empty decomposition', async () => {
    const wrapper = await open(new ApiError(404, 'portfolio not found'))
    expect(wrapper.find('[role="alert"]').text()).toMatch(/not the same as the fund having none/i)
    expect(wrapper.find('table').exists()).toBe(false)
  })
})
