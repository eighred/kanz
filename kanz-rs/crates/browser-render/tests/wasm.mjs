// Test-only actual WASM differential runner. Node must support TS type stripping.
import assert from 'node:assert/strict'
import { readFileSync, statSync } from 'node:fs'
import { execFileSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { resolve } from 'node:path'
import { createHash } from 'node:crypto'
import { formatDecimal, formatRational } from './oracle.ts'

const localDiagnostic = process.argv.length === 4 && process.argv[3] === '--local-diagnostic'
assert(process.argv.length === 3 || localDiagnostic)
const root = fileURLToPath(new URL('../../../../', import.meta.url))
const git = (...args) => execFileSync('git', args, {cwd: root, encoding: 'utf8'}).trim()
const sha = git('rev-parse', 'HEAD')
const tree = git('rev-parse', 'HEAD^{tree}')
assert.match(sha, /^[0-9a-f]{40}$/)
assert.match(tree, /^[0-9a-f]{40}$/)
if (localDiagnostic) {
  assert.notEqual(process.env.GITHUB_ACTIONS, 'true')
} else {
  assert.equal(process.env.GITHUB_ACTIONS, 'true')
  assert.equal(process.env.GITHUB_SHA, sha)
  assert.match(process.env.GITHUB_RUN_ID ?? '', /^[1-9][0-9]*$/)
  assert.match(process.env.GITHUB_RUN_ATTEMPT ?? '', /^[1-9][0-9]*$/)
  assert.equal(process.versions.node.split('.')[0], readFileSync(resolve(root, 'kanz-web/.nvmrc'), 'utf8').trim())
}
const sourceNames = ['kanz-rs/Cargo.toml', 'kanz-rs/Cargo.lock', 'kanz-rs/crates/browser-render/Cargo.toml', 'kanz-rs/crates/browser-render/src/lib.rs', 'kanz-rs/crates/browser-render/tests/kernel.rs', 'kanz-rs/crates/browser-render/tests/oracle.ts', 'kanz-rs/crates/browser-render/tests/wasm.mjs', 'kanz-rs/crates/browser-render/tests/build-wasm.sh', 'kanz-web/.nvmrc']
const trackedDirty = git('status', '--porcelain', '--untracked-files=no', '--', ...sourceNames) !== ''
if (!localDiagnostic) {
  assert.equal(trackedDirty, false)
  for (const name of sourceNames) {
    assert.equal(git('ls-files', '--error-unmatch', '--', name), name)
    const committed = execFileSync('git', ['show', `HEAD:${name}`], {cwd: root, maxBuffer: 4 * 1024 * 1024})
    assert.deepEqual(readFileSync(resolve(root, name)), committed)
  }
}
const hash = raw => createHash('sha256').update(raw).digest('hex')
const sources = Object.fromEntries(sourceNames.map(name => [name, hash(readFileSync(resolve(root, name)))]))
const size = statSync(process.argv[2])
assert(size.isFile() && size.size > 0 && size.size <= 2 * 1024 * 1024)
const bytes = readFileSync(process.argv[2])
assert.equal(bytes.length, size.size)
const cases = []
assert(bytes.length > 0 && bytes.length <= 2 * 1024 * 1024)
const module = new WebAssembly.Module(bytes)
assert.deepEqual(WebAssembly.Module.imports(module), [])
cases.push('zero_imports')
const instance = new WebAssembly.Instance(module)
const w = instance.exports
assert.deepEqual(WebAssembly.Module.exports(module).map(x => `${x.kind}:${x.name}`).sort(), ['memory:memory', 'global:__data_end', 'global:__heap_base', 'function:render_input_byte', 'function:render_output_byte', 'function:render_output_len', 'function:render_reset', 'function:render_value'].sort())
const functions = WebAssembly.Module.exports(module).filter(x => x.kind === 'function').map(x => x.name).sort()
assert.deepEqual(functions, ['render_input_byte', 'render_output_byte', 'render_output_len', 'render_reset', 'render_value'])
cases.push('closed_exports')
assert(w.memory instanceof WebAssembly.Memory)
assert(w.memory.buffer.byteLength <= 2 * 1024 * 1024)
// The linker recipe must declare a maximum, not merely a small initial memory.
const initialPages = w.memory.buffer.byteLength / 65536
assert.equal(w.memory.grow(32 - initialPages), initialPages)
assert.throws(() => w.memory.grow(1), RangeError)
cases.push('memory_ceiling')
function render(kind, raw, exponent = 0) {
  w.render_reset()
  const input = new TextEncoder().encode(raw)
  for (let i = 0; i < input.length; i++) {
    if (w.render_input_byte(i, input[i]) !== 1) {
      assert.equal(w.render_output_len(), 0)
      assert.equal(w.render_value(kind, exponent), 2)
      return null
    }
  }
  const status = w.render_value(kind, exponent)
  if (status === 2) { assert.equal(w.render_output_len(), 0); return null }
  assert.equal(status, 1)
  const length = w.render_output_len()
  assert(length > 0 && length <= 256)
  const output = Uint8Array.from({length}, (_, i) => { const byte = w.render_output_byte(i); assert(byte >= 0 && byte <= 127); return byte })
  assert.equal(w.render_output_byte(length), 256)
  return new TextDecoder('utf-8', {fatal: true}).decode(output)
}
let comparisons = 0
for (const raw of ['0', '-0', '00000', '-00002', '5', '100', '9007199254740993', '-9223372036854775808', '9223372036854775807', '9'.repeat(128), '', '-', '1.5', '1e3', ' 1']) {
  for (let exponent = -65; exponent <= 65; exponent++) {
    assert.equal(render(1, raw, exponent), formatDecimal({coefficient: raw, exponent}))
    comparisons++
  }
}
assert.equal(comparisons, 1965)
cases.push('decimal_differential')
for (const num of ['0', '-0', '1', '-3', '00012', '9007199254740993', '9'.repeat(128)]) {
  for (const den of ['1', '2', '4', '5', '8', '20', '0005', '3', '6', '0', '-2', '18446744073709551616', '36893488147419103232']) {
    const raw = `${num}/${den}`
    assert.equal(render(2, raw), formatRational(raw)); comparisons++
  }
}
assert.equal(comparisons, 2056)
cases.push('rational_differential')
for (const [raw, want] of [['6/4', '1.50'], ['0/8', '0.000'], ['3/6', null], ['1/8', '0.125']]) assert.equal(render(2, raw), want)
cases.push('independent_goldens')
assert.equal(render(1, '9'.repeat(129)), null) // Explicit bounded noncanonical-domain difference.
assert.equal(render(2, '1/'.padEnd(130, '0') + '1'), null)
assert.equal(render(2, '1'.padStart(128, '0') + '/' + '1'.padStart(128, '0')), '1')
assert.equal(render(2, '1'.repeat(258)), null)
assert.equal(render(1, '1', -2147483648), null)
assert.equal(render(1, '1', 2147483647), null)
cases.push('bounded_domain')
assert.equal(render(1, '1'), '1')
assert.equal(w.render_value(99, 0), 2)
assert.equal(w.render_output_len(), 0)
assert.equal(w.render_output_byte(0), 256)
cases.push('invalid_kind_clears')
assert.equal(render(1, '1'), '1')
assert.equal(w.render_input_byte(9, 50), 2)
assert.equal(w.render_output_len(), 0)
assert.equal(w.render_value(1, 0), 2)
assert.equal(w.render_input_byte(1, 50), 2)
cases.push('noncontiguous_poison')
w.render_reset()
assert.equal(w.render_input_byte(0, 256), 2)
assert.equal(w.render_value(1, 0), 2)
cases.push('invalid_byte_poison')
assert(w.memory.buffer.byteLength <= 2 * 1024 * 1024)
assert.deepEqual(cases, ['zero_imports', 'closed_exports', 'memory_ceiling', 'decimal_differential', 'rational_differential', 'independent_goldens', 'bounded_domain', 'invalid_kind_clears', 'noncontiguous_poison', 'invalid_byte_poison'])
console.log(JSON.stringify({kind: localDiagnostic ? 'local-node-wasm-diagnostic' : 'actual-node-wasm', source_sha: sha, source_tree: tree, tracked_dirty: trackedDirty, run: localDiagnostic ? null : process.env.GITHUB_RUN_ID, attempt: localDiagnostic ? null : process.env.GITHUB_RUN_ATTEMPT, node: process.version, sources, cases, comparisons, memory_max_bytes: 2097152, wasm_sha256: hash(bytes)}))
