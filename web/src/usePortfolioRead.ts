import { ref, shallowRef, watch } from 'vue'
import type { Ref } from 'vue'
import { describeRisk } from './api/risk'

export function usePortfolioRead<T>(id: Ref<string>, fetchPortfolio: (id: string) => Promise<T>) {
  const data = shallowRef<T | null>(null)
  const error = ref('')
  const loading = ref(true)

  watch(id, async (portfolio, _, onCleanup) => {
    let current = true
    onCleanup(() => { current = false })
    data.value = null
    error.value = ''
    loading.value = true
    try {
      const result = await fetchPortfolio(portfolio)
      if (current) data.value = result
    } catch (e) {
      if (current) error.value = describeRisk(e)
    } finally {
      if (current) loading.value = false
    }
  }, { immediate: true })

  return { data, error, loading }
}
