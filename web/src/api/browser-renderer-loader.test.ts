import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { createHash, webcrypto } from 'node:crypto'

const bytes = Uint8Array.from(readFileSync(resolve('../kanz-rs/target/wasm32-unknown-unknown/release/kanz_browser_render.wasm')))
const asset = { format: 'kanz-browser-render-asset-v1', abi: 1, maxMemoryBytes: 2097152,
  moduleSHA256: createHash('sha256').update(bytes).digest('hex'), moduleBytes: bytes.length }
beforeEach(() => { vi.resetModules(); vi.stubGlobal('crypto', webcrypto) })
afterEach(() => { vi.unstubAllGlobals(); vi.doUnmock('../generated/browser-render'); vi.useRealTimers() })

for (const kind of ['complete', 'oversized', 'truncated', 'cancelled'] as const) {
  it(`bounds and closes the actual ${kind} ReadableStream`, async () => {
    vi.doMock('../generated/browser-render', () => ({ rendererAsset: asset, url: '/fixed-renderer.wasm' }))
    if (kind === 'cancelled') vi.useFakeTimers()
    let signal: AbortSignal | undefined
    let cancel: ReturnType<typeof vi.spyOn> | undefined
    let release: ReturnType<typeof vi.spyOn> | undefined
    const fetcher = vi.fn(async (_url: string, options: RequestInit) => {
      signal = options.signal as AbortSignal
      const body = new ReadableStream<Uint8Array>({
        start(controller) {
          if (kind === 'cancelled') {
            signal!.addEventListener('abort', () => controller.error(new Error('synthetic cancellation')), { once: true })
          } else {
            controller.enqueue(kind === 'oversized' ? new Uint8Array(bytes.length + 1)
              : kind === 'truncated' ? bytes.slice(0, -1) : bytes)
            if (kind !== 'oversized') controller.close()
          }
        },
      })
      const reader = body.getReader()
      cancel = vi.spyOn(reader, 'cancel')
      release = vi.spyOn(reader, 'releaseLock')
      vi.spyOn(body, 'getReader').mockReturnValue(reader)
      return { ok: true, body }
    })
    vi.stubGlobal('fetch', fetcher)
    const loader = await import('./browser-renderer-loader')
    const loading = loader.initializeBrowserRenderer()
    if (kind === 'cancelled') await vi.advanceTimersByTimeAsync(3000)
    await loading
    expect(fetcher).toHaveBeenCalledOnce()
    expect(fetcher.mock.calls[0]![1]).toMatchObject({ credentials: 'omit', redirect: 'error' })
    expect(signal?.aborted).toBe(true)
    expect(cancel).toHaveBeenCalledOnce()
    expect(release).toHaveBeenCalledOnce()
    const renderer = await import('./browser-renderer')
    expect(renderer.renderBrowserValue(1, '150', -2)).toEqual(kind === 'complete'
      ? { handled: true, value: '1.50' } : { handled: false })
  })
}
