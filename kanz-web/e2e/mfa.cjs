const { chromium } = require(process.env.TEST_MFA_PLAYWRIGHT || 'playwright')
let stage = 'start'
async function main(input) {
  const browser = await chromium.launch({ headless: true })
  try {
    const context = await browser.newContext()
    const page = await context.newPage()
    const cdp = await context.newCDPSession(page)
    await cdp.send('WebAuthn.enable')
    const addKey = () => cdp.send('WebAuthn.addVirtualAuthenticator', { options: { protocol: 'ctap2', transport: 'usb', hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true } })
    const primary = await addKey()
    const login = async () => {
      await page.goto(input.base + '/login')
      await page.getByLabel('Account', { exact: true }).fill(input.subject)
      await page.getByLabel('Password', { exact: true }).fill(input.credential)
      await page.getByRole('button', { name: 'Sign in', exact: true }).click()
      await page.waitForURL('**/overview')
    }
    const apiStatus = (path) => page.evaluate(async p => (await fetch(p)).status, path)
    stage = 'initial login and privileged refusal'
    await login()
    if (await apiStatus('/api/identity/users') !== 403) throw new Error('initial policy')
    await page.goto(input.base + '/authentication')
    stage = 'primary enrollment'
    await page.getByLabel('Factor name').fill('Primary key')
    await page.getByLabel('Current password').fill(input.credential)
    await page.getByRole('button', { name: 'Add security key or passkey' }).click()
    await page.getByText('Factor enrolled. Other sessions were revoked.', { exact: true }).waitFor()
    if (await apiStatus('/api/identity/users') !== 200) throw new Error('privileged assurance')
    stage = 'password alone cannot create session'
    const guest = await browser.newContext()
    const initial = await guest.request.post(input.base + '/auth/login', { data: { subject: input.subject, credential: input.credential } })
    if (initial.status() !== 202 || (await guest.request.get(input.base + '/auth/me')).status() !== 401) throw new Error('password bypass')
    await guest.close()
    stage = 'MFA login'
    await page.evaluate(async () => { await fetch('/auth/logout', { method: 'POST' }) })
    await login()
    stage = 'explicit step-up and replay'
    await page.goto(input.base + '/authentication')
    const finishRequest = page.waitForRequest(r => r.url().endsWith('/auth/mfa/stepup/finish') && r.method() === 'POST')
    await page.getByRole('button', { name: 'Verify security key', exact: true }).click()
    await page.getByText('Verified. Privileged access is available for five minutes.', { exact: false }).waitFor()
    const captured = (await finishRequest).postDataJSON()
    const replay = await context.request.post(input.base + '/auth/mfa/stepup/finish', { data: captured })
    if (replay.status() !== 401) throw new Error('proof replay')
    stage = 'spare factor enrollment'
    await cdp.send('WebAuthn.removeVirtualAuthenticator', { authenticatorId: primary.authenticatorId })
    await addKey()
    await page.getByLabel('Factor name').fill('Spare key')
    await page.getByLabel('Current password').fill(input.credential)
    await page.getByRole('button', { name: 'Add security key or passkey' }).click()
    await page.getByText('Factor enrolled. Other sessions were revoked.', { exact: true }).waitFor()
    stage = 'factor removal'
    await page.getByRole('button', { name: 'Remove Primary key', exact: true }).click()
    await page.getByText('Factor removed. Other sessions were revoked.', { exact: true }).waitFor()
    if (!(await page.getByRole('button', { name: 'Remove Spare key', exact: true }).isDisabled())) throw new Error('last factor control')
    stage = 'remaining factor login'
    await page.evaluate(async () => { await fetch('/auth/logout', { method: 'POST' }) })
    await login()
    if (await apiStatus('/api/identity/users') !== 200) throw new Error('remaining factor unusable')
    await context.close()
  } finally { await browser.close() }
}
let input = ''
process.stdin.setEncoding('utf8')
process.stdin.on('data', d => input += d)
process.stdin.on('end', () => main(JSON.parse(input)).catch(() => { console.error('MFA browser verification failed at: ' + stage); process.exitCode = 1 }))
