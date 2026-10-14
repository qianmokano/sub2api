import assert from 'node:assert/strict'
import { createRequire } from 'node:module'
import { mkdir, writeFile } from 'node:fs/promises'
import path from 'node:path'

const require = createRequire(path.join(process.env.KANO_BROWSER_MODULE_ROOT || process.cwd(), 'package.json'))
const { chromium } = require('playwright')
const origin = process.env.KANO_BROWSER_ORIGIN || 'https://127.0.0.1:19443'
const artifacts = process.env.KANO_BROWSER_ARTIFACTS || '/tmp/kano-updater-browser'
await mkdir(artifacts, { recursive: true })
const browser = await chromium.launch({ headless: true })
const context = await browser.newContext({ ignoreHTTPSErrors: true, locale: 'en-US' })
const page = await context.newPage()
page.setDefaultTimeout(30_000)
const diagnostics = []
const pending = new Set()
page.on('request', request => pending.add(new URL(request.url()).pathname))
page.on('requestfinished', request => pending.delete(new URL(request.url()).pathname))
page.on('pageerror', error => diagnostics.push({ type: 'pageerror', message: error.message }))
page.on('console', message => {
  if (message.type() === 'error') diagnostics.push({ type: 'console', message: message.text().slice(0, 1000) })
})
page.on('requestfailed', request => diagnostics.push({
  type: 'requestfailed', path: new URL(request.url()).pathname, message: request.failure()?.errorText
}))
page.on('response', response => {
  if (diagnostics.length < 100) diagnostics.push({ type: 'loaded', path: new URL(response.url()).pathname, status: response.status(), mime: response.headers()['content-type'] })
  if (response.status() >= 400) diagnostics.push({ type: 'response', path: new URL(response.url()).pathname, status: response.status() })
})
let passed = false
try {
  const readyDeadline = Date.now() + 180_000
  while (true) {
    try {
      const response = await context.request.get(`${origin}/fixture/ready`)
      if (response.ok()) break
    } catch { /* The fixture starts PostgreSQL before exposing nginx. */ }
    assert.ok(Date.now() < readyDeadline, 'HTTPS fixture did not become ready')
    await new Promise(resolve => setTimeout(resolve, 1000))
  }
  const login = await context.request.post(`${origin}/api/v1/auth/login`, {
    data: { email: 'updater-fixture@example.test', password: 'Fixture-Only-Password-2026' }
  })
  assert.equal(login.status(), 200)
  const { data: session } = await login.json()
  assert.ok(session.access_token && session.user.role === 'admin')
  const authorization = { Authorization: `Bearer ${session.access_token}` }
  const compliance = await context.request.get(`${origin}/api/v1/admin/compliance`, { headers: authorization })
  assert.equal(compliance.status(), 200)
  const { data: declaration } = await compliance.json()
  let expectedAudits = 2 // Initial fixture login and this browser login.
  if (declaration.required) {
    const accepted = await context.request.post(`${origin}/api/v1/admin/compliance/accept`, {
      headers: authorization,
      data: { phrase: declaration.ack_phrase_en, language: 'en' }
    })
    assert.equal(accepted.status(), 200)
    assert.equal((await accepted.json()).data.required, false)
    expectedAudits++
  }
  await context.addInitScript(session => {
    localStorage.setItem('auth_token', session.access_token)
    localStorage.setItem('auth_user', JSON.stringify(session.user))
    localStorage.setItem('sub2api_locale', 'en')
    localStorage.setItem('kano-update-language', 'en')
    localStorage.setItem(`admin_guide_${session.user.id}_admin_v4_interactive`, 'true')
  }, session)
  await page.goto(`${origin}/admin/dashboard`)
  await page.getByRole('button', { name: 'v0.2.16-1', exact: true }).first().click()
  const panel = page.getByTestId('container-upgrade')
  await panel.waitFor()
  const button = panel.getByRole('button', { name: 'Upgrade', exact: true })
  await button.waitFor()
  await page.waitForFunction(() => {
    const button = document.querySelector('[data-testid="container-upgrade"] button')
    return button && !button.disabled
  })
  assert.equal(await button.isEnabled(), true)
  await page.screenshot({ path: path.join(artifacts, 'upgrade-button.png'), fullPage: true })
  assert.equal((await context.request.get(`${origin}/fixture/baseline?expected_audits=${expectedAudits}`, { timeout: 120_000 })).status(), 200)
  // Capture the real response before handing it to the page: the application
  // immediately navigates away and Chromium can discard its response body.
  let accepted
  await page.route('**/api/v1/admin/system/update', async route => {
    const response = await route.fetch()
    accepted = { status: response.status(), body: await response.json() }
    await route.fulfill({ response })
  })
  await button.click()
  await page.waitForURL(new RegExp(`${origin}/kano-updates/up-[a-f0-9]{32}$`))
  assert.equal(accepted.status, 202)
  const { data: job } = accepted.body
  assert.match(job.progress_url, /^\/kano-updates\/up-[a-f0-9]{32}$/)
  assert.equal(JSON.stringify(job).includes('capability'), false)
  assert.equal(page.url(), `${origin}${job.progress_url}`)
  const cookie = (await context.cookies()).find(item => item.path === job.progress_url)
  assert.ok(cookie && cookie.secure && cookie.httpOnly && cookie.sameSite === 'Strict')
  assert.equal(await page.evaluate(() => document.cookie.includes('kano_update_')), false)
  const anonymous = await browser.newContext({ ignoreHTTPSErrors: true })
  assert.equal((await anonymous.request.get(`${origin}${job.progress_url}`)).status(), 403)
  assert.equal((await anonymous.request.get(`${origin}${job.progress_url}/status`)).status(), 403)
  await anonymous.close()
  const deadline = Date.now() + 8 * 60_000
  let checkedMaintenance = false
  while (true) {
    const statusResponse = await context.request.get(`${origin}${job.progress_url}/status`)
    assert.equal(statusResponse.status(), 200)
    const status = await statusResponse.json()
    assert.equal(JSON.stringify(status).includes('fixture-private-value'), false)
    if (!checkedMaintenance && ['backing_up', 'restoring', 'verifying', 'switching', 'checking'].includes(status.status)) {
      assert.equal((await context.request.get(`${origin}/health`)).status(), 503)
      const refreshed = await page.reload()
      assert.equal(refreshed.status(), 200)
      await page.locator('#step').waitFor()
      await page.screenshot({ path: path.join(artifacts, 'maintenance-progress.png'), fullPage: true })
      checkedMaintenance = true
    }
    if (['succeeded', 'failed', 'failed_rolled_back', 'needs_attention'].includes(status.status)) {
      assert.equal(status.status, 'succeeded', 'real browser upgrade did not succeed')
      break
    }
    assert.ok(Date.now() < deadline, 'browser upgrade timed out')
    await new Promise(resolve => setTimeout(resolve, 1000))
  }
  assert.equal(checkedMaintenance, true)
  assert.equal((await context.request.get(`${origin}/health`)).status(), 200)
  await page.reload()
  await page.locator('#step.done').waitFor()
  await page.screenshot({ path: path.join(artifacts, 'upgrade-complete.png'), fullPage: true })
  await page.goto(`${origin}/admin/dashboard`)
  await page.getByRole('button', { name: 'v0.2.16-2', exact: true }).first().click()
  await page.getByTestId('container-upgrade').getByRole('link', { name: 'View upgrade progress' }).waitFor()
  const profile = await context.request.get(`${origin}/api/v1/user/profile`, { headers: { Authorization: `Bearer ${session.access_token}` } })
  assert.equal(profile.status(), 200, 'pre-upgrade administrator session was lost')
  passed = true
  console.log('HTTPS browser upgrade passed: real 202, scoped cookie, anonymous 403, refresh during maintenance, retained administrator session and recent task.')
} finally {
  if (!passed) {
    await page.screenshot({ path: path.join(artifacts, 'failure.png'), fullPage: true }).catch(() => {})
    await writeFile(path.join(artifacts, 'diagnostics.json'), JSON.stringify({ url: page.url(), pending: [...pending], dom: await page.locator('body').innerHTML(), diagnostics }, null, 2))
  }
  await context.request.post(`${origin}/fixture/done?ok=${passed}`).catch(() => {})
  await browser.close()
}
