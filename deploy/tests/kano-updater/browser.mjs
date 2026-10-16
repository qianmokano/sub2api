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
  const anonymous = await browser.newContext({ ignoreHTTPSErrors: true })
  const ordinaryPage = await anonymous.newPage()
  await ordinaryPage.goto(`${origin}/login?from=maintenance`)
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
  const scheduleButton = panel.getByRole('button', { name: 'Reserve next 04:00 Beijing time', exact: true })
  await scheduleButton.click()
  assert.match(await page.getByRole('dialog').innerText(), /04:00/)
  await page.getByRole('dialog').getByRole('button', { name: 'Confirm', exact: true }).click()
  await panel.getByRole('button', { name: 'Cancel reservation', exact: true }).waitFor()
  expectedAudits++
  await page.getByTestId('maintenance-banner').waitFor({ timeout: 15000 })
  await ordinaryPage.getByTestId('maintenance-banner').waitFor({ timeout: 15000 })
  const reserved = await context.request.get(`${origin}/api/v1/admin/system/update-schedule`, { headers: authorization })
  assert.equal(reserved.status(), 200)
  const firstReservation = (await reserved.json()).data
  const publicNotice = await anonymous.request.get(`${origin}/kano-maintenance/status`)
  const publicJSON = await publicNotice.json()
  assert.equal(publicJSON.scheduled_at, firstReservation.job.scheduled_at)
  for (const privateValue of [firstReservation.operation_id, 'v0.2.16-2', 'image_digest', 'capability', 'manifest']) assert.equal(JSON.stringify(publicJSON).includes(privateValue), false)
  assert.equal((await anonymous.request.get(`${origin}${firstReservation.progress_url}`)).status(), 403)
  await panel.getByRole('button', { name: 'Cancel reservation', exact: true }).click()
  expectedAudits++
  await page.getByTestId('maintenance-banner').waitFor({ state: 'detached', timeout: 15000 })
  await ordinaryPage.getByTestId('maintenance-banner').waitFor({ state: 'detached', timeout: 15000 })
  // The static page and minimal status still work without any host daemon.
  assert.equal((await context.request.post(`${origin}/fixture/offline`)).status(), 200)
  const offlinePage = await anonymous.newPage()
  const offlineHTML = await offlinePage.goto(`${origin}/login?offline=1`)
  assert.equal(offlineHTML.status(), 503)
  await offlinePage.locator('#started').waitFor()
  await offlinePage.waitForFunction(() => document.querySelector('#started').textContent !== '正在获取')
  await offlinePage.waitForFunction(() => document.querySelector('#elapsed').textContent !== '—')
  assert.match(await offlinePage.title(), /kano API/)
  assert.equal((await anonymous.request.get(`${origin}/kano-maintenance/status`)).status(), 200)
  const offlineAPI = await anonymous.request.get(`${origin}/v1/models`, { headers: { Accept: 'text/html' } })
  assert.equal(offlineAPI.status(), 503)
  assert.match(offlineAPI.headers()['content-type'], /application\/json/)
  assert.equal((await offlineAPI.json()).error.type, 'maintenance')
  await offlinePage.screenshot({ path: path.join(artifacts, 'maintenance-daemon-offline.png'), fullPage: true })
  assert.equal((await context.request.post(`${origin}/fixture/online`)).status(), 200)
  await offlinePage.waitForFunction(() => !document.querySelector('#phase'), { timeout: 30000 })
  await offlinePage.close()
  await page.reload()
  await page.getByRole('button', { name: 'v0.2.16-1', exact: true }).first().click()
  assert.equal((await context.request.get(`${origin}/fixture/baseline?expected_audits=${expectedAudits}`, { timeout: 120_000 })).status(), 200)
  // Capture the real response before handing it to the page: the application
  // immediately navigates away and Chromium can discard its response body.
  let accepted
  await page.route('**/api/v1/admin/system/update-schedule', async route => {
    if (route.request().method() !== 'POST') return route.continue()
    const response = await route.fetch()
    accepted = { status: response.status(), body: await response.json() }
    await route.fulfill({ response })
  })
  await scheduleButton.click()
  await page.getByRole('dialog').getByRole('button', { name: 'Confirm', exact: true }).click()
  await panel.getByRole('button', { name: 'Cancel reservation', exact: true }).waitFor()
  assert.equal(accepted.status, 202)
  const { data: job } = accepted.body
  assert.match(job.progress_url, /^\/kano-updates\/up-[a-f0-9]{32}$/)
  assert.equal(JSON.stringify(job).includes('capability'), false)
  assert.ok(job.job.scheduled_at)
  const cookie = (await context.cookies()).find(item => item.path === job.progress_url)
  assert.ok(cookie && cookie.secure && cookie.httpOnly && cookie.sameSite === 'Strict')
  assert.equal(await page.evaluate(() => document.cookie.includes('kano_update_')), false)
  assert.equal((await anonymous.request.get(`${origin}${job.progress_url}`)).status(), 403)
  assert.equal((await anonymous.request.get(`${origin}${job.progress_url}/status`)).status(), 403)
  const preparedDeadline = Date.now() + 60000
  while (true) {
    const response = await context.request.get(`${origin}${job.progress_url}/status`)
    const status = await response.json()
    if (status.preparation === 'ready') break
    assert.ok(Date.now() < preparedDeadline, 'online preparation timed out')
    await new Promise(resolve => setTimeout(resolve, 200))
  }
  assert.equal((await context.request.get(`${origin}/health`)).status(), 200)
  assert.equal((await context.request.post(`${origin}/fixture/clock?at=${encodeURIComponent(job.job.scheduled_at)}`)).status(), 200)
  await page.goto(`${origin}${job.progress_url}`)
  const deadline = Date.now() + 8 * 60_000
  let checkedMaintenance = false
  while (true) {
    const statusResponse = await context.request.get(`${origin}${job.progress_url}/status`)
    assert.equal(statusResponse.status(), 200)
    const status = await statusResponse.json()
    assert.equal(JSON.stringify(status).includes('fixture-private-value'), false)
    if (!checkedMaintenance && ['backing_up', 'restoring', 'verifying', 'switching', 'checking'].includes(status.status)) {
      assert.equal((await context.request.get(`${origin}/health`)).status(), 503)
      const apiResponse = await anonymous.request.post(`${origin}/v1/chat/completions`, { headers: { Accept: 'text/event-stream' }, data: { stream: true } })
      assert.equal(apiResponse.status(), 503)
      assert.match(apiResponse.headers()['content-type'], /application\/json/)
      assert.equal(apiResponse.headers()['retry-after'], '60')
      assert.match(apiResponse.headers()['cache-control'], /no-store/)
      assert.equal((await apiResponse.json()).error.type, 'maintenance')
      const maintenancePage = await ordinaryPage.goto(`${origin}/login?from=maintenance`)
      assert.equal(maintenancePage.status(), 503)
      await ordinaryPage.locator('#phase').waitFor()
      assert.match(await ordinaryPage.locator('main').innerText(), /30–45/)
      await ordinaryPage.screenshot({ path: path.join(artifacts, 'public-maintenance.png'), fullPage: true })
      await ordinaryPage.setViewportSize({ width: 390, height: 844 })
      await ordinaryPage.screenshot({ path: path.join(artifacts, 'public-maintenance-mobile.png'), fullPage: true })
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
  await ordinaryPage.waitForFunction(() => !document.querySelector('#phase'), { timeout: 30000 })
  assert.equal(ordinaryPage.url(), `${origin}/login?from=maintenance`, 'recovery must refresh the original URL')
  await anonymous.close()
  await page.reload()
  await page.locator('#step.done').waitFor()
  await page.screenshot({ path: path.join(artifacts, 'upgrade-complete.png'), fullPage: true })
  await page.goto(`${origin}/admin/dashboard`)
  await page.getByRole('button', { name: 'v0.2.16-2', exact: true }).first().click()
  await page.getByTestId('container-upgrade').getByRole('link', { name: 'View upgrade progress' }).waitFor()
  const profile = await context.request.get(`${origin}/api/v1/user/profile`, { headers: { Authorization: `Bearer ${session.access_token}` } })
  assert.equal(profile.status(), 200, 'pre-upgrade administrator session was lost')
  passed = true
  console.log('HTTPS scheduled upgrade passed: date confirmation, immediate and cross-tab banners, cancellation, online preparation, static maintenance with daemon offline, JSON API/stream 503, private progress, automatic recovery to original URL and retained session.')
} finally {
  if (!passed) {
    await page.screenshot({ path: path.join(artifacts, 'failure.png'), fullPage: true }).catch(() => {})
    await writeFile(path.join(artifacts, 'diagnostics.json'), JSON.stringify({ url: page.url(), pending: [...pending], dom: await page.locator('body').innerHTML(), diagnostics }, null, 2))
  }
  await context.request.post(`${origin}/fixture/done?ok=${passed}`).catch(() => {})
  await browser.close()
}
