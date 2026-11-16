import { afterEach, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import { createPinia } from 'pinia'
import { useSession } from '../stores/session'
import { broker, type BrokerAccountDetail } from '../api/broker'
import { ApiError } from '../api/client'
import BrokerAccountView from './BrokerAccountView.vue'
const identity = { subject: 'reader', tenant: 'tenant-a', expires_at: '2099-01-01' }
function detail(id: string): BrokerAccountDetail { return { state: { id, currency: id, balance: '9007199254740993.0000000001', equity: '0', realizedPl: '0', unrealizedPl: '0', openPositions: 0 }, positions: [], orders: [], executions: [] } }
function pending<T>() { let resolve!: (value: T) => void; let reject!: (error: Error) => void; const promise = new Promise<T>((a,b) => { resolve=a;reject=b }); return {promise,resolve,reject} }
afterEach(() => vi.restoreAllMocks())
async function setup() {
 const pinia=createPinia(); const session=useSession(pinia); session.identity={...identity}
 const router=createRouter({history:createMemoryHistory(),routes:[{path:'/broker-accounts/:id',component:BrokerAccountView}]})
 await router.push('/broker-accounts/A')
 const wrapper=mount(BrokerAccountView,{global:{plugins:[pinia,router]}})
 return {wrapper,router,session}
}
it('clears old account and ignores late success after route reuse', async()=>{
 const a=pending<BrokerAccountDetail>(); const b=pending<BrokerAccountDetail>()
 const call=vi.spyOn(broker,'account').mockReturnValueOnce(a.promise).mockReturnValueOnce(b.promise)
 const {wrapper,router}=await setup(); await router.push('/broker-accounts/B'); await flushPromises()
 expect(call.mock.calls[0]?.[1]?.aborted).toBe(true)
 b.resolve(detail('B')); await flushPromises(); expect(wrapper.text()).toContain('9007199254740993.0000000001')
 a.resolve(detail('A')); await flushPromises(); expect(wrapper.find('dd').text()).toBe('B'); wrapper.unmount()
})
it('clears loaded data on route change and ignores obsolete error/finally',async()=>{
 const b=pending<BrokerAccountDetail>(); const c=pending<BrokerAccountDetail>()
 vi.spyOn(broker,'account').mockResolvedValueOnce(detail('A')).mockReturnValueOnce(b.promise).mockReturnValueOnce(c.promise)
 const {wrapper,router}=await setup();await flushPromises();await router.push('/broker-accounts/B');await flushPromises()
 expect(wrapper.find('dl').exists()).toBe(false)
 await router.push('/broker-accounts/C');b.reject(new ApiError(403,'private'));await flushPromises()
 expect(wrapper.text()).toContain('Loading');expect(wrapper.find('[role=alert]').exists()).toBe(false)
 c.resolve(detail('C'));await flushPromises();expect(wrapper.find('dd').text()).toBe('C');wrapper.unmount()
})
it('invalidates the same account on tenant/session replacement and logout',async()=>{
 const old=pending<BrokerAccountDetail>();const fresh=pending<BrokerAccountDetail>()
 const call=vi.spyOn(broker,'account').mockReturnValueOnce(old.promise).mockReturnValueOnce(fresh.promise)
 const {wrapper,session}=await setup();session.identity={...identity,tenant:'tenant-b'};await flushPromises()
 expect(call).toHaveBeenCalledTimes(2);expect(call.mock.calls[0]?.[1]?.aborted).toBe(true)
 old.resolve(detail('A'));await flushPromises();expect(wrapper.find('dl').exists()).toBe(false)
 session.forget();fresh.resolve(detail('A'));await flushPromises();expect(wrapper.find('dl').exists()).toBe(false);expect(wrapper.text()).toContain('Sign in')
 wrapper.unmount()
})
it('aborts on unmount and refuses mismatched account identity',async()=>{
 const task=pending<BrokerAccountDetail>();const call=vi.spyOn(broker,'account').mockReturnValue(task.promise)
 const {wrapper}=await setup();task.resolve(detail('other'));await flushPromises();expect(wrapper.find('[role=alert]').text()).toContain('could not be verified');expect(wrapper.find('dl').exists()).toBe(false)
 wrapper.unmount();expect(call.mock.calls[0]?.[1]?.aborted).toBe(true)
})
it.each([403,503])('refuses unavailable %s instead of empty data',async status=>{
 vi.spyOn(broker,'account').mockRejectedValue(new ApiError(status,'private'))
 const {wrapper}=await setup();await flushPromises();expect(wrapper.find('[role=alert]').exists()).toBe(true);expect(wrapper.text()).not.toContain('No orders');expect(wrapper.text()).not.toContain('private');wrapper.unmount()
})

it('also invalidates when a session identity field changes in place',async()=>{
 const old=pending<BrokerAccountDetail>(); const next=pending<BrokerAccountDetail>()
 const call=vi.spyOn(broker,'account').mockReturnValueOnce(old.promise).mockReturnValueOnce(next.promise)
 const {wrapper,session}=await setup()
 session.identity!.tenant='tenant-b'; await flushPromises()
 expect(call).toHaveBeenCalledTimes(2);expect(call.mock.calls[0]?.[1]?.aborted).toBe(true)
 old.resolve(detail('A'));await flushPromises();expect(wrapper.find('dl').exists()).toBe(false)
 next.resolve(detail('A'));await flushPromises();expect(wrapper.find('dl').exists()).toBe(true);wrapper.unmount()
})
