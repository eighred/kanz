import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { describe, expect, it } from 'vitest'
import App from './App.vue'
import { useSession } from './stores/session'
import { workspaces } from './navigation'
import { router as appRouter } from './router'

describe('workspace navigation', () => {
  it('links only to real screens and closes menus after selection or Escape', async () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    useSession().$patch({ resolved: true, identity: { subject: 'test-only', tenant: 'test-only', expires_at: '2099-01-01T00:00:00Z' } })
    const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/:pathMatch(.*)*', component: { template: '<p>Test screen</p>' } }] })
    await router.push('/overview')
    const wrapper = mount(App, { global: { plugins: [pinia, router] } })
    for (const group of workspaces) {
      for (const page of group.pages) {
        expect(appRouter.resolve(page.to).name).not.toBe('not-found')
        expect(wrapper.find(`a[href="${page.to}"]`).exists()).toBe(true)
      }
    }
    const menu = wrapper.find('details')
    const element = menu.element as HTMLDetailsElement
    element.open = true
    await menu.find('a').trigger('click')
    expect(element.open).toBe(false)
    element.open = true
    await menu.find('summary').trigger('keydown', { key: 'Escape' })
    expect(element.open).toBe(false)
  })
})
