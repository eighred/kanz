import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createRouter, createWebHistory } from 'vue-router'
import NodesView from './NodesView.vue'
import * as api from '../api/control'
import { ApiError } from '../api/client'
import type { Node } from '../api/control'

// THIS SCREEN IS THE APPLICATION'S FIRST MUTATING SURFACE, and the claims it
// makes are operational, not cosmetic:
//
//   1. no action reaches the gateway without a confirmation;
//   2. drain — the only one that moves running work off a machine — cannot be
//      confirmed without typing the node's name;
//   3. the node acted on is the node that was on screen;
//   4. a drain is NOT reported as finished, because the gateway returns once the
//      node is cordoned and eviction continues afterwards;
//   5. a refused action leaves the estate view honest rather than optimistic.
//
// Each is a way to take down someone else's workload by accident, so each is
// tested rather than asserted in a comment.

const worker: Node = {
  name: 'worker-1',
  status: 'NODE_STATUS_READY',
  schedulable: true,
  region: 'eu-west',
  evictablePods: 4,
}

function router() {
  return createRouter({
    history: createWebHistory(),
    routes: [
      { path: '/nodes', name: 'nodes', component: NodesView },
      { path: '/estate', name: 'estate', component: { template: '<div />' } },
    ],
  })
}

async function open(nodes: Node[] = [worker]) {
  vi.spyOn(api.control, 'nodes').mockResolvedValue(nodes)
  const r = router()
  await r.push('/nodes')
  await r.isReady()
  const wrapper = mount(NodesView, { global: { plugins: [r] } })
  await flushPromises()
  return wrapper
}

/** button finds a button by its visible label. */
function button(wrapper: Awaited<ReturnType<typeof open>>, label: string) {
  return wrapper.findAll('button').find((b) => b.text() === label)
}

describe('every action is confirmed before it reaches the gateway', () => {
  it('does not call the gateway when the action is only clicked', async () => {
    const cordon = vi.spyOn(api.control, 'cordon').mockResolvedValue({})
    const wrapper = await open()

    await button(wrapper, 'Cordon')!.trigger('click')
    await flushPromises()

    expect(cordon).not.toHaveBeenCalled()
    expect(wrapper.find('[role="dialog"]').exists()).toBe(true)
  })

  it('calls it once the confirmation is accepted', async () => {
    const cordon = vi.spyOn(api.control, 'cordon').mockResolvedValue({})
    const wrapper = await open()

    await button(wrapper, 'Cordon')!.trigger('click')
    await button(wrapper, 'Cordon node')!.trigger('click')
    await flushPromises()

    expect(cordon).toHaveBeenCalledExactlyOnceWith('worker-1')
  })

  it('does nothing at all when the confirmation is cancelled', async () => {
    const cordon = vi.spyOn(api.control, 'cordon').mockResolvedValue({})
    const wrapper = await open()

    await button(wrapper, 'Cordon')!.trigger('click')
    await button(wrapper, 'Cancel')!.trigger('click')
    await flushPromises()

    expect(cordon).not.toHaveBeenCalled()
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
  })
})

// DRAIN IS THE ONE THAT EVICTS. Cordon and uncordon are each other's inverse and
// a region change is a relabel; drain moves running work off the machine, so the
// question "which row was I on" has to be answered rather than assumed.
describe('drain requires the node name to be typed', () => {
  it('refuses to confirm until the typed name matches', async () => {
    const drain = vi.spyOn(api.control, 'drain').mockResolvedValue({})
    const wrapper = await open()

    await button(wrapper, 'Drain')!.trigger('click')
    const confirm = button(wrapper, 'Drain node')!
    expect(confirm.attributes('disabled')).toBeDefined()

    await wrapper.find('input[name="confirm-name"]').setValue('worker-2')
    expect(button(wrapper, 'Drain node')!.attributes('disabled')).toBeDefined()

    await confirm.trigger('click')
    await flushPromises()
    expect(drain).not.toHaveBeenCalled()
  })

  it('confirms once the name matches exactly', async () => {
    const drain = vi.spyOn(api.control, 'drain').mockResolvedValue({})
    const wrapper = await open()

    await button(wrapper, 'Drain')!.trigger('click')
    await wrapper.find('input[name="confirm-name"]').setValue('worker-1')
    await button(wrapper, 'Drain node')!.trigger('click')
    await flushPromises()

    expect(drain).toHaveBeenCalledExactlyOnceWith('worker-1')
  })

  // The gateway returns once the node is CORDONED; eviction proceeds in the
  // background under PodDisruptionBudgets and can stall on one. Reporting
  // "drained" would state something the platform never claimed.
  it('does not report the drain as finished', async () => {
    vi.spyOn(api.control, 'drain').mockResolvedValue({})
    const wrapper = await open()

    await button(wrapper, 'Drain')!.trigger('click')
    await wrapper.find('input[name="confirm-name"]').setValue('worker-1')
    await button(wrapper, 'Drain node')!.trigger('click')
    await flushPromises()

    const notice = wrapper.find('[role="status"]').text()
    expect(notice).toMatch(/background/i)
    expect(notice).not.toMatch(/\bdrained\b/i)
  })
})

describe('the action lands on the node that was on screen', () => {
  it('acts on the row whose button was pressed, not the first row', async () => {
    const other: Node = { name: 'worker-2', status: 'NODE_STATUS_READY', schedulable: true }
    const cordon = vi.spyOn(api.control, 'cordon').mockResolvedValue({})
    const wrapper = await open([worker, other])

    // The second row's Cordon button.
    await wrapper.findAll('button').filter((b) => b.text() === 'Cordon')[1]!.trigger('click')
    await button(wrapper, 'Cordon node')!.trigger('click')
    await flushPromises()

    expect(cordon).toHaveBeenCalledExactlyOnceWith('worker-2')
  })

  // Offering Uncordon on a schedulable node is a no-op that still reads as an
  // action having been taken.
  it('offers uncordon only for a node that is actually cordoned', async () => {
    const cordoned: Node = { name: 'worker-3', status: 'NODE_STATUS_READY', schedulable: false }
    const wrapper = await open([worker, cordoned])

    const labels = wrapper.findAll('button').map((b) => b.text())
    expect(labels.filter((l) => l === 'Cordon')).toHaveLength(1)
    expect(labels.filter((l) => l === 'Uncordon')).toHaveLength(1)
  })
})

describe('a move sends a region and never the node name', () => {
  it('refuses an unchanged or empty region', async () => {
    const setRegion = vi.spyOn(api.control, 'setRegion').mockResolvedValue({})
    const wrapper = await open()

    await button(wrapper, 'Move')!.trigger('click')
    // Pre-filled with the current region, so confirming would be a no-op write.
    expect(button(wrapper, 'Move node to another region')!.attributes('disabled')).toBeDefined()

    await wrapper.find('input[name="region"]').setValue('   ')
    expect(button(wrapper, 'Move node to another region')!.attributes('disabled')).toBeDefined()
    expect(setRegion).not.toHaveBeenCalled()
  })

  it('sends the trimmed region for the named node', async () => {
    const setRegion = vi.spyOn(api.control, 'setRegion').mockResolvedValue({})
    const wrapper = await open()

    await button(wrapper, 'Move')!.trigger('click')
    await wrapper.find('input[name="region"]').setValue('  us-east  ')
    await button(wrapper, 'Move node to another region')!.trigger('click')
    await flushPromises()

    expect(setRegion).toHaveBeenCalledExactlyOnceWith('worker-1', 'us-east')
  })
})

describe('a refused action is reported rather than assumed', () => {
  // 403 is the gateway doing its job: this page renders for anyone signed in,
  // because hiding a button is a courtesy and not a control.
  it('explains a missing operator authority and leaves the dialog open', async () => {
    vi.spyOn(api.control, 'cordon').mockRejectedValue(new ApiError(403, 'insufficient capability'))
    const wrapper = await open()

    await button(wrapper, 'Cordon')!.trigger('click')
    await button(wrapper, 'Cordon node')!.trigger('click')
    await flushPromises()

    expect(wrapper.find('[role="alert"]').text()).toMatch(/operator authority/i)
    expect(wrapper.find('[role="dialog"]').exists()).toBe(true)
    expect(wrapper.find('[role="status"]').exists()).toBe(false)
  })

  // A 404 means the gateway fronts no control plane. Drawn as an empty table it
  // reads as a healthy estate with no nodes, which is the most dangerous screen
  // this application could draw.
  it('does not draw an unconfigured control plane as an empty estate', async () => {
    vi.spyOn(api.control, 'nodes').mockRejectedValue(new ApiError(404, 'not found'))
    const r = router()
    await r.push('/nodes')
    const wrapper = mount(NodesView, { global: { plugins: [r] } })
    await flushPromises()

    expect(wrapper.find('[role="alert"]').text()).toMatch(/no control plane/i)
    expect(wrapper.find('table').exists()).toBe(false)
  })
})

beforeEach(() => {
  vi.restoreAllMocks()
})
