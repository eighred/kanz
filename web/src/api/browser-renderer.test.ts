import { beforeEach, expect, it, vi } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { formatDecimal as oracleDecimal, formatRational as oracleRational } from '../../../kanz-rs/crates/browser-render/tests/oracle'

const bytes = readFileSync(resolve('../kanz-rs/target/wasm32-unknown-unknown/release/kanz_browser_render.wasm'))
const buffer = () => Uint8Array.from(bytes).buffer
beforeEach(() => vi.resetModules())

it('uses the actual bounded WASM for supported values and keeps legacy long Decimal rendering', async () => {
  const renderer = await import('./browser-renderer')
  const decimal = await import('./decimal')
  renderer.installBrowserRenderer(buffer())
  for (const coefficient of ['0', '-0', '00002', '-9223372036854775808', '9007199254740993', '9'.repeat(128)]) {
    for (const exponent of [-65, -64, -2, 0, 2, 64, 65]) {
      expect(decimal.formatDecimal({ coefficient, exponent })).toBe(oracleDecimal({ coefficient, exponent }))
    }
  }
  expect(renderer.renderBrowserValue(1, '12', -1)).toEqual({ handled: true, value: '1.2' })
  expect(renderer.renderBrowserValue(1, '9'.repeat(129))).toEqual({ handled: false })
  expect(decimal.formatDecimal({ coefficient: '9'.repeat(129), exponent: -2 }))
    .toBe(oracleDecimal({ coefficient: '9'.repeat(129), exponent: -2 }))
  for (const raw of ['6/4', '0/8', '3/6', '1/0', '-0/5', '9007199254740993/100']) {
    expect(decimal.formatRational(raw)).toBe(oracleRational(raw))
  }
  expect(decimal.formatDecimal({})).toBe('0')
  expect(decimal.formatDecimal(undefined)).toBeNull()
  expect(decimal.formatMoney({ amount: { coefficient: '150', exponent: -2 }, currency_code: 'USD' })).toBe('1.50 USD')
})

it('actual wrong-ABI bytes refuse installation without changing TS or null semantics', async () => {
  const renderer = await import('./browser-renderer')
  const changed = Buffer.from(bytes)
  const index = changed.indexOf(Buffer.from('render_value'))
  expect(index).toBeGreaterThan(0)
  changed.write('render_other', index, 'ascii')
  expect(() => renderer.installBrowserRenderer(Uint8Array.from(changed).buffer)).toThrow()
  renderer.disableBrowserRenderer()
  const decimal = await import('./decimal')
  expect(decimal.formatDecimal({ coefficient: '150', exponent: -2 })).toBe('1.50')
  expect(decimal.formatRational('1/3')).toBeNull()
  expect(decimal.formatDecimal(null)).toBeNull()
  expect(renderer.renderBrowserValue(1, '1')).toEqual({ handled: false })
})
