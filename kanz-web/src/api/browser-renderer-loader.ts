import { rendererAsset, url } from '../generated/browser-render'
import { disableBrowserRenderer, installBrowserRenderer } from './browser-renderer'

let started = false
// Fixed build-owned asset only; no URL/configuration or principal supplied by a
// caller. Initialization does not delay authentication, routing or mounting.
export async function initializeBrowserRenderer(): Promise<void> {
  if (started) return
  started = true
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), 3000)
  let reader: ReadableStreamDefaultReader<Uint8Array> | undefined
  try {
    const address = new URL(url, window.location.href)
    if (address.origin !== window.location.origin || !['http:', 'https:'].includes(address.protocol)
        || address.username || address.password || address.hash) throw new Error('renderer origin')
    if (rendererAsset.format !== 'kanz-browser-render-asset-v1' || rendererAsset.abi !== 1
        || rendererAsset.maxMemoryBytes !== 2097152 || !/^[0-9a-f]{64}$/.test(rendererAsset.moduleSHA256)
        || !Number.isInteger(rendererAsset.moduleBytes) || rendererAsset.moduleBytes < 1
        || rendererAsset.moduleBytes > 2097152) throw new Error('renderer descriptor')
    const response = await fetch(address.href, { signal: controller.signal, redirect: 'error', credentials: 'omit' })
    if (!response.ok || !response.body) throw new Error('renderer asset unavailable')
    reader = response.body.getReader()
    const bytes = new Uint8Array(rendererAsset.moduleBytes)
    let offset = 0
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      if (value.byteLength > bytes.length - offset) throw new Error('renderer asset oversized')
      bytes.set(value, offset); offset += value.byteLength
    }
    if (offset !== bytes.length || controller.signal.aborted) throw new Error('renderer asset incomplete')
    const digest = await crypto.subtle.digest('SHA-256', bytes)
    const hash = Array.from(new Uint8Array(digest), b => b.toString(16).padStart(2, '0')).join('')
    if (hash !== rendererAsset.moduleSHA256 || controller.signal.aborted) throw new Error('renderer asset identity')
    installBrowserRenderer(bytes.buffer)
  } catch { disableBrowserRenderer() }
  finally { controller.abort(); clearTimeout(timer); if (reader) { await reader.cancel().catch(() => undefined); reader.releaseLock() } }
}
