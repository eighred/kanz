// Use the real browser surface and a run-local Chromium user-verifying authenticator.
'use strict'
const crypto = require('node:crypto')
const fs = require('node:fs')
const http = require('node:http')
const https = require('node:https')
const net = require('node:net')
const path = require('node:path')
const { execFileSync } = require('node:child_process')

const base = 'https://localhost:18443'
let stage = 'input'
const routes = new Map([
  ['/redeem', 'invitePage'], ['/auth/redeem', 'redeem'], ['/auth/me', 'session'],
  ['/auth/mfa', 'factors'], ['/auth/mfa/register/begin', 'enrollBegin'], ['/auth/mfa/register/finish', 'enrollFinish'],
  ['/auth/mfa/stepup/begin', 'stepUpBegin'], ['/auth/mfa/stepup/finish', 'stepUpFinish'],
  ['/api/v1/control/provisions', 'listRuns'], ['/api/v1/control/nodes', 'addNode'],
])
const statuses = new Map()

function tool(name, args, input, env) {
  return execFileSync(name, args, { input, env, stdio: ['pipe', 'pipe', 'pipe'], timeout: 10000 })
}

// Give OpenSSL a real anonymous pipe. Node can implement its child's stdin as
// a socketpair, which OpenSSL cannot reopen via /dev/stdin on Linux.
function openssl(args, input, env) {
  return tool('sh', ['-c', 'cat | openssl "$@"', 'openssl', ...args], input, env)
}

function privateKey() {
  return crypto.generateKeyPairSync('ec', { namedCurve: 'P-256' }).privateKey.export({ type: 'pkcs8', format: 'pem' })
}

function tlsIdentity(dir, env) {
  const authority = privateKey()
  const key = privateKey()
  const ca = openssl(['req', '-x509', '-new', '-key', '/dev/stdin', '-days', '1',
    '-subj', '/CN=Kanz disposable browser CA', '-addext', 'basicConstraints=critical,CA:TRUE',
    '-addext', 'keyUsage=critical,keyCertSign'], authority, env)
  const signingRequest = openssl(['req', '-new', '-key', '/dev/stdin', '-subj', '/CN=localhost',
    '-addext', 'subjectAltName=DNS:localhost', '-addext', 'basicConstraints=critical,CA:FALSE',
    '-addext', 'extendedKeyUsage=serverAuth', '-addext', 'keyUsage=critical,digitalSignature'], key, env)
  const caPath = path.join(dir, 'authority.crt')
  const requestPath = path.join(dir, 'localhost.csr')
  fs.writeFileSync(caPath, ca)
  fs.writeFileSync(requestPath, signingRequest)
  const cert = openssl(['x509', '-req', '-in', requestPath, '-CA', caPath, '-CAkey', '/dev/stdin',
    '-copy_extensions', 'copy', '-set_serial', '0x' + crypto.randomBytes(16).toString('hex'), '-days', '1'], authority, env)
  // Chromium reads this NSS database when it exists. Only the public CA enters it.
  const store = path.join(dir, '.pki', 'nssdb')
  fs.mkdirSync(store, { recursive: true })
  tool('certutil', ['-N', '-d', 'sql:' + store, '--empty-password'], undefined, env)
  tool('certutil', ['-A', '-d', 'sql:' + store, '-n', 'Run-local browser CA', '-t', 'C,,', '-i', caPath], undefined, env)
  return { key, cert, minVersion: 'TLSv1.2' }
}

function serve(identity) {
  const server = https.createServer(identity, (incoming, outgoing) => {
    if (incoming.headers.host !== 'localhost:18443' || !/^\/(?!\/)/.test(incoming.url)) {
      outgoing.writeHead(400).end()
      return
    }
    const upstream = http.request({ host: '127.0.0.1', port: 18084, path: incoming.url,
      method: incoming.method, headers: incoming.headers, timeout: 15000 }, response => {
      outgoing.writeHead(response.statusCode, response.headers)
      response.pipe(outgoing)
    })
    upstream.on('timeout', () => upstream.destroy())
    upstream.on('error', () => { if (!outgoing.headersSent) outgoing.writeHead(502); outgoing.end() })
    incoming.on('error', () => upstream.destroy())
    incoming.pipe(upstream)
  })
  return server
}

async function onboard(page, context, input) {
  // Record only recognized local routes and numeric status. Bodies, query
  // strings, headers and arbitrary browser exceptions can carry credentials.
  const controlReads = []
  let nodePosts = 0
  page.on('request', request => {
    if (request.url() === base + '/api/v1/control/nodes' && request.method() === 'POST') nodePosts++
  })
  page.on('response', response => {
    const url = new URL(response.url())
    const label = url.origin === base && routes.get(url.pathname)
    if (label) statuses.set(label, response.status())
    if (label === 'listRuns' && response.request().method() === 'GET' && controlReads.at(-1) !== response.status()) {
      controlReads.push(response.status())
    }
  })
  const cdp = await context.newCDPSession(page)
  await cdp.send('WebAuthn.enable')
  await cdp.send('WebAuthn.addVirtualAuthenticator', { options: { protocol: 'ctap2', transport: 'usb',
    hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true } })
  const password = crypto.randomBytes(32).toString('base64url')
  stage = 'invitation redemption'
  await page.goto(base + '/redeem')
  await page.getByLabel('Invitation token').fill(input.invite)
  await page.getByLabel('Choose a password').fill(password)
  await page.getByLabel('Confirm password').fill(password)
  await page.getByRole('button', { name: 'Accept and sign in' }).click()
  await page.waitForURL(base + '/overview')
  const correct = await page.evaluate(async subject => {
    const response = await fetch('/auth/me')
    if (!response.ok) return false
    const identity = await response.json()
    return identity.subject === subject && identity.tenant === 'onboarding'
  }, input.subject)
  if (!correct) throw new Error()
  stage = 'WebAuthn enrollment'
  await page.goto(base + '/authentication')
  await page.getByLabel('Factor name').fill('Disposable operator key')
  await page.getByLabel('Current password').fill(password)
  await page.getByRole('button', { name: 'Add security key or passkey' }).click()
  await page.getByText('Factor enrolled. Other sessions were revoked.', { exact: true }).waitFor()
  stage = 'WebAuthn step-up'
  await page.getByRole('button', { name: 'Verify security key', exact: true }).click()
  await page.getByText('Security key verified. Return to your action and submit it explicitly.', { exact: true }).waitFor()
  stage = 'node submission'
  await page.goto(base + '/provisions')
  // Enrollment advances the identity session epoch. Allow two normal gateway
  // revocation refreshes and the view's 30-second GET poll before submitting once.
  await page.getByRole('button', { name: 'Add node', exact: true }).click({ timeout: 95000 })
  if (controlReads.at(-1) !== 200 || controlReads.some(status => ![200, 503].includes(status))) throw new Error()
  await page.getByLabel('Hostname', { exact: true }).fill(input.hostname)
  await page.getByLabel('IP address').fill(input.ip)
  await page.getByLabel('SSH user').fill(input.sshUser)
  await page.getByLabel('Verified SSH host public key').fill(input.sshHostKey)
  await page.getByLabel('One-use SSH private key (PEM)').setInputFiles({
    name: 'bootstrap.pem', mimeType: 'application/octet-stream', buffer: Buffer.from(input.sshPrivateKey),
  })
  await page.getByRole('button', { name: 'Provision node', exact: true }).click()
  const receipt = page.getByRole('status').filter({ hasText: 'Provisioning requested.' }).locator('code')
  const id = await receipt.innerText()
  if (!id.startsWith('provision-' + input.hostname.slice(0, 20) + '-') ||
      !/^provision-[a-z0-9-]{1,20}-[a-z0-9]{5}$/.test(id)) throw new Error()
  if (nodePosts !== 1) throw new Error()
  return { id, controlReads, nodePosts }
}

async function main() {
  const chunks = []
  let size = 0
  for await (const chunk of process.stdin) {
    size += chunk.length
    if (size > 32768) throw new Error()
    chunks.push(chunk)
  }
  const input = JSON.parse(Buffer.concat(chunks).toString('utf8'))
  if (input.base !== base || !/^kanz77-[a-z0-9]+-worker$/.test(input.hostname)
      || net.isIP(input.ip) !== 4 || !input.ip.startsWith('10.') || input.sshUser !== 'ubuntu'
      || !/^onboarding-[a-f0-9]{16}$/.test(input.subject) || !/^[\w-]{43}$/.test(input.invite)) throw new Error()
  stage = 'local HTTPS'
  const dir = fs.mkdtempSync('/dev/shm/kanz77-browser-')
  const env = { PATH: process.env.PATH, HOME: dir, TMPDIR: dir, XDG_CACHE_HOME: path.join(dir, 'cache'),
    XDG_CONFIG_HOME: path.join(dir, 'config'), LANG: 'C.UTF-8' }
  let browser
  let server
  try {
    server = serve(tlsIdentity(dir, env))
    await new Promise((resolve, reject) => {
      server.once('error', reject)
      server.listen(18443, '127.0.0.1', resolve)
    })
    stage = 'Chromium startup'
    process.env.TMPDIR = dir
    const { chromium } = require(process.env.TEST_MFA_PLAYWRIGHT || '../../web/node_modules/playwright')
    browser = await chromium.launch({ headless: true, env })
    const context = await browser.newContext()
    const page = await context.newPage()
    page.setDefaultTimeout(20000)
    const receipt = await onboard(page, context, input)
    await context.close()
    process.stdout.write(JSON.stringify(receipt) + '\n')
  } finally {
    try {
      if (browser) await browser.close()
    } finally {
      if (server) {
        server.close()
        server.closeAllConnections()
      }
      fs.rmSync(dir, { recursive: true, force: true })
    }
  }
}

main().catch(error => {
  const category = error.name === 'TimeoutError' ? 'timeout'
    : /net::ERR_CERT_/.test(error.message) ? 'tls' : 'failure'
  console.error(JSON.stringify({ stage, category, statuses: Object.fromEntries(statuses) }))
  process.exitCode = 1
})
