import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { spawnSync } from 'node:child_process'
import { syncBuiltinESMExports } from 'node:module'
import { validateModule, publishAsset, checkCompilerConfiguration, root } from './build-renderer.mjs'

// Independent tiny binary fixture proves the build validator's module boundary,
// not renderer arithmetic. Exported functions have the fixed scalar signatures.
function moduleBytes({ maximum = 32, omitMaximum = false, extraExport = false, imported = false } = {}) {
  const uint = n => { const bytes = []; do { const b = n & 127; n >>>= 7; bytes.push(b | (n ? 128 : 0)) } while (n); return bytes }
  const string = s => [...uint(s.length), ...Buffer.from(s)]
  const section = (id, b) => [id, ...uint(b.length), ...b]
  const functions = ['render_input_byte', 'render_output_byte', 'render_output_len', 'render_reset', 'render_value']
  const entries = [ [...string('memory'), 2, 0], [...string('__data_end'), 3, 0], [...string('__heap_base'), 3, 1],
    ...functions.map((name, i) => [...string(name), 0, i + (imported ? 1 : 0)]) ]
  if (extraExport) entries.push([...string('unreviewed'), 0, 0])
  return Buffer.from([0, 97, 115, 109, 1, 0, 0, 0,
    ...section(1, [4, 96, 0, 0, 96, 2, 127, 127, 1, 127, 96, 0, 1, 127, 96, 1, 127, 1, 127]),
    ...(imported ? section(2, [1, ...string('host'), ...string('call'), 0, 0]) : []),
    ...section(3, [5, 1, 3, 2, 0, 1]),
    ...section(5, omitMaximum ? [1, 0, 1] : [1, 1, 1, ...uint(maximum)]),
    ...section(6, [2, 127, 0, 65, 0, 11, 127, 0, 65, 0, 11]),
    ...section(7, [...uint(entries.length), ...entries.flat()]),
    ...section(10, [5, ...[0, 1, 2, 3, 4].flatMap(i => i === 3 ? [2, 0, 11] : [4, 0, 65, 1, 11])])])
}

test('actual module validator enforces zero imports, closed exports and hard memory bound', () => {
  assert.match(validateModule(moduleBytes()), /^[0-9a-f]{64}$/)
  for (const options of [{ maximum: 33 }, { maximum: 31 }, { omitMaximum: true },
    { extraExport: true }, { imported: true }]) {
    assert.throws(() => validateModule(moduleBytes(options)))
  }
  assert.throws(() => validateModule(Buffer.alloc(0)))
  assert.throws(() => validateModule(Buffer.alloc(2097153)))
  assert.throws(() => validateModule(Buffer.from('not wasm')))
})

test('publication commits one matching entrypoint; interrupted commit preserves prior generation', t => {
  const parent = fs.mkdtempSync(join(tmpdir(), 'kanz-render-build-'))
  t.after(() => fs.rmSync(parent, { recursive: true }))
  const directory = join(parent, 'generated'), bytes = moduleBytes()
  const descriptor = { moduleSHA256: validateModule(bytes), source: 'a'.repeat(40) }
  publishAsset(directory, bytes, descriptor)
  const entry = join(directory, 'browser-render.ts'), previous = fs.readFileSync(entry)
  publishAsset(directory, bytes, descriptor)
  assert.deepEqual(fs.readFileSync(entry), previous)
  assert.throws(() => publishAsset(directory, bytes, { ...descriptor, moduleSHA256: '0'.repeat(64) }))
  assert.deepEqual(fs.readFileSync(entry), previous)
  const original = fs.renameSync
  fs.renameSync = () => { throw new Error('synthetic interrupted pointer commit') }
  syncBuiltinESMExports()
  try { assert.throws(() => publishAsset(directory, bytes, { ...descriptor, source: 'b'.repeat(40) })) }
  finally { fs.renameSync = original; syncBuiltinESMExports() }
  assert.deepEqual(fs.readFileSync(entry), previous)
  assert.equal(fs.readdirSync(directory).some(name => name.endsWith('.tmp')), false)
  const module = join(directory, `browser-render.${descriptor.moduleSHA256}.wasm`)
  fs.writeFileSync(module, Buffer.from('tampered'))
  assert.throws(() => publishAsset(directory, bytes, descriptor))
  assert.deepEqual(fs.readFileSync(entry), previous)
})

test('untracked Cargo config and ambient compiler overrides fail closed', t => {
  const parent = fs.mkdtempSync(join(tmpdir(), 'kanz-render-config-'))
  t.after(() => fs.rmSync(parent, { recursive: true }))
  const cargoHome = join(parent, 'cargo-home'), cwd = join(parent, 'workspace')
  fs.mkdirSync(cargoHome); fs.mkdirSync(cwd)
  checkCompilerConfiguration(cwd, cargoHome, {})
  for (const name of ['RUSTFLAGS', 'RUSTC_WRAPPER', 'CARGO_TARGET_WASM32_UNKNOWN_UNKNOWN_RUSTFLAGS', 'RUSTC_BOOTSTRAP', 'CARGO_PROFILE_RELEASE_OPT_LEVEL', 'CARGO_PROFILE_RELEASE_OVERFLOW_CHECKS', 'CARGO_PROFILE_RELEASE_PANIC']) {
    assert.throws(() => checkCompilerConfiguration(cwd, cargoHome, { [name]: 'unreviewed' }))
  }
  fs.writeFileSync(join(cargoHome, 'config.toml'), '[build]\nrustflags=[]\n')
  assert.throws(() => checkCompilerConfiguration(cwd, cargoHome, {}))
  fs.unlinkSync(join(cargoHome, 'config.toml'))
  fs.mkdirSync(join(cwd, '.cargo'))
  fs.writeFileSync(join(cwd, '.cargo/config'), '[build]\nrustflags=[]\n')
  assert.throws(() => checkCompilerConfiguration(cwd, cargoHome, {}))
})

test('failed prerequisite stops prebuild without accepting an old entrypoint', () => {
  const entry = join(root, 'kanz-web/src/generated/browser-render.ts')
  const previous = fs.existsSync(entry) ? fs.readFileSync(entry) : null
  const child = spawnSync(process.execPath, [join(root, 'kanz-web/scripts/build-renderer.mjs')],
    { env: { ...process.env, PATH: '', GITHUB_ACTIONS: 'false' }, encoding: 'utf8', timeout: 10000 })
  assert.equal(child.status, 1)
  assert.equal(child.stdout, '')
  assert.equal(child.stderr.trim(), 'Pinned browser renderer build failed; no stale asset fallback. Verify Rust 1.90.0, wasm32 target and source/configuration prerequisites.')
  assert.deepEqual(fs.existsSync(entry) ? fs.readFileSync(entry) : null, previous)
})
