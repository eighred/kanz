import { ref, shallowRef, watch } from 'vue'
import type { Ref } from 'vue'
import { describeRisk } from './api/risk'

export function usePortfolioRead<Q, T>(query: Ref<Q>, fetchPortfolio: (query: Q, signal: AbortSignal) => Promise<T>, describeError = describeRisk) {
  const data = shallowRef<T | null>(null)
  const error = ref('')
  const loading = ref(true)

  watch(query, async (selection, _, onCleanup) => {
    const request = new AbortController()
    onCleanup(() => request.abort())
    data.value = null
    error.value = ''
    loading.value = true
    try {
      const result = await fetchPortfolio(selection, request.signal)
      if (!request.signal.aborted) data.value = result
    } catch (e) {
      if (!request.signal.aborted) error.value = describeError(e)
    } finally {
      if (!request.signal.aborted) loading.value = false
    }
  }, { immediate: true, flush: 'sync' })

  return { data, error, loading }
}
