// Optional exact rendering only. Unavailable WASM leaves the existing TS
// implementation in use; it never changes wire values or command authority.
type Kernel = {
  memory: WebAssembly.Memory
  render_reset(): void
  render_input_byte(index: number, value: number): number
  render_value(kind: number, exponent: number): number
  render_output_len(): number
  render_output_byte(index: number): number
}
type RenderResult = { handled: false } | { handled: true; value: string | null }
let kernel: Kernel | undefined
let unavailable = false

export function installBrowserRenderer(bytes: ArrayBuffer): void {
  if (unavailable || kernel) throw new Error('renderer state already resolved')
  if (bytes.byteLength === 0 || bytes.byteLength > 2097152) throw new Error('renderer size')
  const module = new WebAssembly.Module(bytes)
  if (WebAssembly.Module.imports(module).length !== 0) throw new Error('renderer imports')
  const expected = ['memory:memory', 'global:__data_end', 'global:__heap_base',
    'function:render_input_byte', 'function:render_output_byte', 'function:render_output_len',
    'function:render_reset', 'function:render_value'].sort()
  const actual = WebAssembly.Module.exports(module).map(x => `${x.kind}:${x.name}`).sort()
  if (JSON.stringify(actual) !== JSON.stringify(expected)) throw new Error('renderer exports')
  const probe = new WebAssembly.Instance(module).exports as unknown as Kernel
  for (const [name, arity] of Object.entries({ render_reset: 0, render_input_byte: 2,
    render_value: 2, render_output_len: 0, render_output_byte: 1 })) {
    const fn = probe[name as keyof Kernel]
    if (typeof fn !== 'function' || fn.length !== arity) throw new Error('renderer ABI')
  }
  if (!(probe.memory instanceof WebAssembly.Memory)) throw new Error('renderer memory')
  const pages = probe.memory.buffer.byteLength / 65536
  if (!Number.isInteger(pages) || pages < 1 || pages > 32) throw new Error('renderer memory')
  if (probe.memory.grow(32 - pages) !== pages) throw new Error('renderer memory')
  let bounded = false
  try { probe.memory.grow(1) } catch (error) { bounded = error instanceof RangeError }
  if (!bounded) throw new Error('renderer memory ceiling')
  // Never install the grown validation instance.
  kernel = new WebAssembly.Instance(module).exports as unknown as Kernel
}

export function disableBrowserRenderer(): void { kernel = undefined; unavailable = true }

export function renderBrowserValue(kind: 1 | 2, raw: string, exponent = 0): RenderResult {
  if (!kernel || unavailable || raw.length > (kind === 1 ? 128 : 257)) return { handled: false }
  try {
    kernel.render_reset()
    for (let i = 0; i < raw.length; i++) {
      const value = raw.charCodeAt(i)
      if (value > 127 || kernel.render_input_byte(i, value) !== 1) throw new Error('renderer input')
    }
    const status = kernel.render_value(kind, exponent)
    const length = kernel.render_output_len()
    if (status === 2 && length === 0) return { handled: true, value: null }
    if (status !== 1 || !Number.isInteger(length) || length < 1 || length > 256) throw new Error('renderer result')
    let result = ''
    for (let i = 0; i < length; i++) {
      const value = kernel.render_output_byte(i)
      if (!Number.isInteger(value) || value < 0 || value > 127) throw new Error('renderer byte')
      result += String.fromCharCode(value)
    }
    if (kernel.render_output_byte(length) !== 256 || !/^-?\d+(\.\d+)?$/.test(result)) throw new Error('renderer output')
    return { handled: true, value: result }
  } catch {
    disableBrowserRenderer()
    return { handled: false }
  }
}
