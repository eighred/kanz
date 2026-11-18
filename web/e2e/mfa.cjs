const { chromium } = require(process.env.TEST_MFA_PLAYWRIGHT || 'playwright')
let stage = 'start'
async function integerContracts(page, base) {
  stage = 'integer directory and epoch wire representation'
  const directory = await page.evaluate(async () => (await fetch('/api/identity/users')).json())
  for (const [subject, revision, epoch] of [['wire-a','9007199254740992','9007199254740992'], ['wire-b','9007199254740993','0'], ['wire-max','9223372036854775807','0']]) {
    const row = directory.users.find(r => r.subject === subject)
    if (!row || row.revision !== revision || row.session_epoch !== epoch || row.tenant !== 'acme') throw new Error('integer wire contract')
  }
  if (directory.users.some(r => r.tenant !== 'acme' || r.subject === 'wire-foreign')) throw new Error('foreign directory')
  await page.goto(base + '/users')
  for (const [subject, revision, status] of [['wire-a','9007199254740992',200], ['wire-b','9007199254740993',200], ['wire-max','9223372036854775807',409]]) {
    stage = 'reviewed integer account mutation'
    await page.getByRole('button', { name: 'Edit access for ' + subject, exact: true }).click()
    await page.getByLabel('Portfolio IDs, separated by commas').fill('wire-edited')
    const request = page.waitForRequest(r => r.url().endsWith('/users/' + subject + '/access') && r.method() === 'PUT')
    const response = page.waitForResponse(r => r.url().endsWith('/users/' + subject + '/access') && r.request().method() === 'PUT')
    await page.getByRole('button', { name: 'Save access and revoke sessions', exact: true }).click()
    if ((await request).postDataJSON().revision !== revision || (await response).status() !== status) throw new Error('reviewed account revision changed')
    if (status === 200) {
      await page.getByText('Access updated and previous sessions revoked. The account holder must sign in again.', { exact: true }).waitFor()
      const body = await (await response).json()
      if (body.revision !== (BigInt(revision)+1n).toString()) throw new Error('integer mutation response')
    } else await page.getByRole('alert').filter({ hasText: 'reload' }).waitFor()
  }
  stage = 'numeric malformed and stale integer requests'
  const failures = await page.evaluate(async () => {
    const send = async body => (await fetch('/api/identity/users/wire-b/access', {method:'PUT',headers:{'Content-Type':'application/json'},body})).status
    const statuses=[]
    for (const revision of [0, -1, null, '', '-1', '+1', '01', '1.0', '1e3', '9223372036854775808']) statuses.push(await send(JSON.stringify({revision,roles:['kanz-user'],portfolios:[]})))
    statuses.push(await send('{"roles":["kanz-user"],"portfolios":[]}'))
    statuses.push(await send('{"revision":"1","roles":["kanz-user"],"portfolios":[]} {}'))
    statuses.push(await send('{"revision":"9007199254740993","roles":["kanz-user"],"portfolios":[]}'))
    statuses.push((await fetch('/api/identity/users/wire-foreign/access', {method:'PUT',headers:{'Content-Type':'application/json'},body:'{"revision":"0","roles":["kanz-user"],"portfolios":[]}'})).status)
    return statuses
  })
  if (failures.slice(0,-2).some(s => s!==400) || failures.at(-2)!==409 || failures.at(-1)!==404) throw new Error('integer refusal')
  await page.goto(base + '/invitations')
  for (const [id, action, revision, status] of [['wire-revoke-a','revoke','9007199254740992',200], ['wire-revoke-b','revoke','9007199254740993',200], ['wire-reissue','reissue','9007199254740993',201], ['wire-max-invite','revoke','9223372036854775807',409]]) {
    stage = 'reviewed integer invitation mutation'
    const label = (action === 'revoke' ? 'Revoke' : 'Reissue') + ' invitation ' + id
    await page.getByRole('button', {name:label,exact:true}).click()
    const request = page.waitForRequest(r => r.url().endsWith('/invites/'+id+'/'+action) && r.method()==='POST')
    const response = page.waitForResponse(r => r.url().endsWith('/invites/'+id+'/'+action) && r.request().method()==='POST')
    await page.getByRole('button', {name:'Confirm '+action,exact:true}).click()
    if ((await request).postDataJSON().revision!==revision || (await response).status()!==status) throw new Error('reviewed invitation revision changed')
    if (status===200 && (await (await response).json()).revision!==(BigInt(revision)+1n).toString()) throw new Error('integer invitation response')
    if (status===201) {
      if ((await (await response).json()).revision!=='0') throw new Error('integer replacement response')
      await page.getByRole('button',{name:'I have transferred it',exact:true}).click()
    }
    if (status===409) await page.getByRole('alert').waitFor()
  }
}
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
    const unassured = await page.evaluate(async () => {
      const headers = {'Content-Type':'application/json'}
      return [
        (await fetch('/api/identity/users/wire-a/access', {method:'PUT',headers,body:'{"revision":"9007199254740992","roles":["kanz-user"],"portfolios":[]}'})).status,
        (await fetch('/api/identity/invites/wire-revoke-a/revoke', {method:'POST',headers,body:'{"revision":"9007199254740992"}'})).status,
      ]
    })
    if (unassured.some(status => status!==403)) throw new Error('unassured integer mutation')
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
    await page.getByText('Security key verified. Return to your action and submit it explicitly.', { exact: true }).waitFor()
    await page.getByText('Recent verification is active.', { exact: false }).waitFor()
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
    await integerContracts(page, input.base)
    await context.close()
  } finally { await browser.close() }
}
let input = ''
process.stdin.setEncoding('utf8')
process.stdin.on('data', d => input += d)
process.stdin.on('end', () => main(JSON.parse(input)).catch(() => { console.error('MFA browser verification failed at: ' + stage); process.exitCode = 1 }))
