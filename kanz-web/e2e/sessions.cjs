const { chromium } = require(process.env.TEST_MFA_PLAYWRIGHT)
async function main() {
  let input = ''
  for await (const chunk of process.stdin) input += chunk
  const { base, replica, cookie, otherCookie } = JSON.parse(input)
  const browser = await chromium.launch({ headless: true })
  try {
    const context = await browser.newContext()
    await context.addCookies([{ name: 'kanz_session', value: cookie, url: base, httpOnly: true, sameSite: 'Lax' }])
    const page = await context.newPage()
    await page.addInitScript(() => { window.violations = []; document.addEventListener('securitypolicyviolation', e => window.violations.push(e.violatedDirective)) })
    await page.goto(base + '/sessions')
    await page.getByRole('heading', { name: 'Browser sessions', exact: true }).waitFor()
    await page.getByRole('button', { name: 'Sign out session', exact: true }).click()
    await page.getByRole('status').filter({ hasText: 'Session signed out.' }).waitFor()
    const dead = await context.request.get(replica + '/auth/me', { headers: { Cookie: 'kanz_session=' + otherCookie } })
    if (dead.status() !== 401) throw new Error('revoked browser remained active on other replica')
    await page.locator('tbody tr').nth(1).waitFor({ state: 'detached' })
    if (await page.locator('tbody tr').count() !== 1) throw new Error('inventory did not update')
    if ((await page.evaluate(() => document.cookie)).includes('kanz_session')) throw new Error('session cookie exposed to script')
    if ((await page.evaluate(() => window.violations)).length) throw new Error('CSP violation')
    await page.getByRole('button', { name: 'Sign out this browser', exact: true }).click()
    await page.waitForURL(base + '/login')
    const self = await context.request.get(replica + '/auth/me', { headers: { Cookie: 'kanz_session=' + cookie } })
    if (self.status() !== 401) throw new Error('current browser remained active on other replica')
  } finally { await browser.close() }
}
main().catch(() => { console.error('Shared-session browser verification failed'); process.exitCode = 1 })
