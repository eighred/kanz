const { chromium } = require(process.env.TEST_MFA_PLAYWRIGHT)
const assert = require('node:assert/strict')
let modeObserved = 'initial', phaseObserved = 'startup'
// Presentation proof on real BFF with synthetic session/gateway data. No claim
// that these test authorities are production authentication or financial state.
async function main() {
  let input = ''
  for await (const chunk of process.stdin) { input += chunk; assert(input.length <= 4096) }
  const { base, old, cookie } = JSON.parse(input)
  const browser = await chromium.launch({ headless: true, timeout: 10000 })
  try {
    for (const mode of ['ready', 'missing', 'tampered', 'held', 'old-csp']) {
      modeObserved = mode
      const origin = mode === 'old-csp' ? old : base
      const context = await browser.newContext()
      try {
        await context.addCookies([{ name: 'kanz_session', value: cookie, url: origin, httpOnly: true, sameSite: 'Lax' }])
        await context.addInitScript(() => {
          window.renderProof = { instances: 0, calls: 0, assetDone: false, violations: [] }
          document.addEventListener('securitypolicyviolation', event => window.renderProof.violations.push(event.violatedDirective))
          const original = WebAssembly.Instance
          WebAssembly.Instance = new Proxy(original, { construct(target, args) {
            const actual = Reflect.construct(target, args)
            window.renderProof.instances++
            const exports = Object.fromEntries(Object.entries(actual.exports).map(([name, value]) => [name,
              name === 'render_value' ? new Proxy(value, { apply(fn, receiver, args) {
                window.renderProof.calls++; return Reflect.apply(fn, receiver, args)
              } }) : value]))
            // Only this test facade observes calls; every invocation delegates to
            // the actual compiled WASM export and preserves function arity.
            return new Proxy(actual, { get(target, name) { return name === 'exports' ? exports : Reflect.get(target, name, target) } })
          } })
          const fetchOriginal = window.fetch.bind(window)
          window.fetch = async (...args) => {
            try { return await fetchOriginal(...args) }
            finally { if (String(args[0]).includes('.wasm')) window.renderProof.assetDone = true }
          }
        })
        const page = await context.newPage()
        page.setDefaultTimeout(10000)
        await page.route('**/*.wasm', async route => {
          if (mode === 'missing') return route.fulfill({ status: 404, body: '' })
          if (mode === 'tampered') {
            const response = await route.fetch(); const bytes = await response.body(); bytes[bytes.length - 1] ^= 1
            return route.fulfill({ response, body: bytes })
          }
          if (mode === 'held') return // AbortSignal must release the pending fetch.
          return route.continue()
        })
        await page.route('**/api/v1/portfolios/fixture/orders', async route => {
          if (mode === 'ready') await page.waitForFunction(() => window.renderProof.instances === 2)
          return route.continue()
        })
        phaseObserved = 'navigation'
        const navigation = await page.goto(origin + '/portfolios/fixture/orders')
        assert(navigation.ok())
        phaseObserved = 'header'
        const csp = navigation.headers()['content-security-policy']
        assert(csp.includes("script-src 'self'")); assert(!csp.includes("'unsafe-eval'")); assert(!csp.includes("'unsafe-inline'")); assert(!csp.includes('blob:'))
        assert.equal(csp.includes("'wasm-unsafe-eval'"), mode !== 'old-csp')
        phaseObserved = 'cells'
        await page.getByRole('cell', { name: '90071992547409.93', exact: true }).waitFor()
        await page.getByRole('cell', { name: '0.00', exact: true }).waitFor()
        await page.getByRole('cell', { name: '1.50 USD', exact: true }).waitFor()
        await page.getByRole('cell', { name: '9'.repeat(127) + '.99', exact: true }).waitFor()
        assert(!(await page.evaluate(() => document.cookie)).includes('kanz_session'))
        phaseObserved = 'calls'
        if (mode === 'ready') assert((await page.evaluate(() => window.renderProof.calls)) >= 3)
        else {
          await page.waitForFunction(() => window.renderProof.assetDone)
          assert.equal(await page.evaluate(() => window.renderProof.calls), 0)
        }
        phaseObserved = 'csp-control'
        await page.route('**/renderer-csp-control.js', route => route.fulfill({ contentType: 'text/javascript', body:
          `window.rendererCspControl = true; try { eval('1+1'); window.rendererEvalBlocked=false } catch { window.rendererEvalBlocked=true } const script=document.createElement('script'); script.textContent='window.inlineRendererProbe=true'; document.body.appendChild(script);` }))
        await page.evaluate(url => new Promise((resolve, reject) => { const node = document.createElement('script'); node.src = url; node.onload = resolve; node.onerror = reject; document.head.appendChild(node) }), origin + '/renderer-csp-control.js')
        assert.equal(await page.evaluate(() => window.rendererCspControl), true)
        assert.equal(await page.evaluate(() => window.rendererEvalBlocked), true)
        assert.equal(await page.evaluate(() => window.inlineRendererProbe), undefined)
      } finally { await context.close() }
    }
    console.log('Browser renderer: actual WASM, legacy fallback, missing/tampered/aborted asset and strict CSP controls PASS')
  } finally { await browser.close() }
}
main().catch(error => { console.error(JSON.stringify({ mode: modeObserved, phase: phaseObserved, category: error?.code === 'ERR_ASSERTION' ? 'assertion' : error?.name === 'TimeoutError' ? 'timeout' : 'other' })); process.exitCode = 1 })
