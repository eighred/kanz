import { expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { h, nextTick } from 'vue'
import WindowedTable from './WindowedTable.vue'
it('bounds rendered rows and retains complete indices and exact values',async()=>{
 const items=Array.from({length:10000},(_,id)=>({id:String(id),value:'9007199254740993.0000000001'}))
 const wrapper=mount(WindowedTable,{props:{items,rowKey:(row:unknown)=>(row as typeof items[number]).id,columns:1,label:'History',empty:'Empty'},slots:{head:()=>h('tr',[h('th','Quantity')]),row:({item}: {item:unknown})=>h('td',(item as typeof items[number]).value)}})
 const region=wrapper.get('[role=region]');expect(wrapper.findAll('.data-row').length).toBeLessThanOrEqual(14)
 expect(wrapper.get('table').attributes('aria-rowcount')).toBe('10001');expect(wrapper.text()).toContain(items[0]!.value)
 Object.defineProperty(region.element,'scrollTop',{value:64000,writable:true});await region.trigger('scroll');await nextTick()
 expect(Number(wrapper.find('.data-row').attributes('aria-rowindex'))).toBe(998)
 expect(wrapper.findAll('.data-row').length).toBeLessThanOrEqual(14)
 await wrapper.setProps({items:items.slice(0,2)});await nextTick();expect(region.element.scrollTop).toBe(0);expect(wrapper.findAll('.data-row')).toHaveLength(2);wrapper.unmount()
})

it('pages every served row with global indices and resets on replacement',async()=>{
 const items=Array.from({length:100001},(_,id)=>String(id))
 const wrapper=mount(WindowedTable,{props:{items,rowKey:(item:unknown)=>String(item),columns:1,label:'History',empty:'Empty'},slots:{row:({item}:{item:unknown})=>h('td',String(item))}})
 for(let page=0;page<10;page++) await wrapper.get('button:last-child').trigger('click')
 expect(wrapper.get('.data-row').attributes('aria-rowindex')).toBe('100002')
 expect(wrapper.get('.data-row').text()).toBe('100000')
 expect(wrapper.get('button:last-child').attributes('disabled')).toBeDefined()
 await wrapper.get('button:first-child').trigger('click')
 expect(wrapper.get('.data-row').attributes('aria-rowindex')).toBe('90002')
 await wrapper.setProps({items:['replacement']})
 expect(wrapper.get('.data-row').attributes('aria-rowindex')).toBe('2')
 expect(wrapper.get('.data-row').text()).toBe('replacement')
 expect(wrapper.find('nav').exists()).toBe(false)
 wrapper.unmount()
})
