// Offline UI checks. All API responses are synthetic; non-loopback is blocked.
import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { readFile, mkdtemp } from 'node:fs/promises'
import { extname, join, resolve, sep } from 'node:path'
import { tmpdir } from 'node:os'
import { fileURLToPath } from 'node:url'
const { chromium } = await import(process.env.SUB2API_PLAYWRIGHT_MODULE || 'playwright')
const dist = fileURLToPath(new URL('../../backend/internal/web/dist', import.meta.url))
const output = await mkdtemp(join(tmpdir(), 'sub2api-bud-update-ui-'))
const server = createServer(async (req, res) => {
  try {
    const path = new URL(req.url, 'http://localhost').pathname
    const file = resolve(dist, `.${extname(path) ? path : '/index.html'}`)
    assert(file.startsWith(dist + sep))
    res.setHeader('Content-Type', { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml' }[extname(file)] || 'application/octet-stream')
    res.end(await readFile(file))
  } catch { res.writeHead(404).end() }
})
await new Promise(done => server.listen(0, '127.0.0.1', done))
const origin = `http://127.0.0.1:${server.address().port}`
const browser = await chromium.launch({ headless: true })
const user = { id: 999, email: 'preview@example.invalid', role: 'admin', status: 'active', balance: 0 }
try {
  for (const [width, dark, locale] of [[1440, false, 'zh'], [1440, true, 'zh'], [390, false, 'zh'], [390, true, 'zh'], [320, false, 'zh'], [1440, true, 'en']]) {
    const context = await browser.newContext({ viewport: { width, height: 1000 } })
    const page = await context.newPage()
    page.setDefaultTimeout(10_000)
    await page.clock.install()
    let writes = 0, checks = 0, phase = 'idle', hasUpdate = true
    const errors = []
    page.on('pageerror', error => errors.push(error.message))
    await page.addInitScript(({ user, dark, locale }) => {
      localStorage.setItem('auth_token', 'synthetic-local-preview')
      localStorage.setItem('auth_user', JSON.stringify(user))
      localStorage.setItem('sub2api_locale', locale)
      localStorage.setItem('theme', dark ? 'dark' : 'light')
      localStorage.setItem('admin_guide_999_admin_v4_interactive', 'true')
    }, { user, dark, locale })
    await page.route('**/*', async route => {
      const req = route.request(), url = new URL(req.url()), path = url.pathname
      if (url.origin !== origin) return route.abort()
      if (!path.startsWith('/api/') && path !== '/setup/status') return route.continue()
      let data = { items: [], total: 0, pages: 1, page: 1, page_size: 20 }
      if (path === '/setup/status') data = { needs_setup: false }
      else if (path.endsWith('/auth/me')) data = user
      else if (path.endsWith('/settings/public')) data = { site_name: 'Sub2API Preview', version: '0.2.4-Bud.14', registration_enabled: false }
      else if (path.endsWith('/check-updates')) {
        checks++
        data = { current_version: '0.2.4-Bud.14', latest_version: hasUpdate ? '0.2.4-Bud.15' : '0.2.4-Bud.14', has_update: hasUpdate, build_type: 'release', can_update: true, cached: false,
          release_info: { name: 'Bud', html_url: 'https://github.com/Bud668/sub2api/releases/tag/v0.2.4-Bud.15' },
          official: { base_version: '0.2.4', latest_version: '0.2.5', has_update: true, checked_at: 1, html_url: 'https://github.com/Wei-Shaw/sub2api/releases/tag/v0.2.5' } }
      } else if (path.endsWith('/update-status')) data = { phase, version: phase === 'idle' ? undefined : '0.2.4-Bud.15' }
      else if (path.endsWith('/system/update')) {
        assert.equal(req.method(), 'POST'); assert.equal(req.postData(), null)
        writes++; phase = 'preparing'; data = { update_started: true, need_restart: false }
      } else if (req.method() !== 'GET') throw Error('Unexpected write ' + path)
      return route.fulfill({ json: { code: 0, data } })
    })
    await page.goto(origin + '/admin/subscriptions')
    if (width < 1024) {
      await page.locator('header button').first().click()
      await page.clock.runFor(300)
    }
    await page.getByTestId('version-toggle').click()
    const panel = page.getByTestId('version-panel'), official = page.getByTestId('official-update')
    await panel.waitFor()
    assert.equal(await official.getByRole('button').count(), 0, 'official is never installable')
    assert((await official.innerText()).includes('0.2.5'))
    const fits = async () => {
      const rect = await panel.boundingBox()
      assert(rect.x >= 0 && rect.y >= 0 && rect.x + rect.width <= width + 1 && rect.y + rect.height <= 1001, JSON.stringify(rect))
      assert.equal(await panel.evaluate(el => el.scrollWidth > el.clientWidth + 1), false)
    }
    await fits()
    await panel.screenshot({ path: join(output, `${width}-${locale}-${dark}-available.png`) })
    const color = await panel.evaluate(el => getComputedStyle(el).backgroundColor)
    await page.evaluate(() => document.documentElement.classList.toggle('dark'))
    assert.notEqual(await panel.evaluate(el => getComputedStyle(el).backgroundColor), color)
    await page.evaluate(() => document.documentElement.classList.toggle('dark'))
    await page.getByTestId('bud-update').click()
    await panel.getByRole('status').waitFor()
    assert.equal(writes, 1)
    assert.equal(await page.getByTestId('bud-update').isDisabled(), true)
    await fits()
    await panel.screenshot({ path: join(output, `${width}-${locale}-${dark}-preparing.png`) })
    phase = 'blocked'
    await page.clock.runFor(30_000)
    await panel.getByRole('alert').waitFor()
    await panel.screenshot({ path: join(output, `${width}-${locale}-${dark}-blocked.png`) })
    await fits()
    assert.equal(writes, 1)
    phase = 'completed'
    await page.clock.runFor(30_000)
    await panel.getByRole('button', { name: locale === 'zh' ? '更新已验收，刷新页面' : 'Update verified · Reload page' }).waitFor()
    const before = checks
    await page.clock.runFor(330_000)
    assert(checks > before, 'official polling must not stay in the old permanent UI cache')
    hasUpdate = false
    await panel.getByRole('button', { name: locale === 'zh' ? '刷新' : 'Refresh', exact: true }).click()
    await page.getByTestId('bud-update').waitFor({ state: 'detached' })
    assert.equal(await official.getByRole('button').count(), 0)
    await page.keyboard.press('Escape')
    assert.equal(await page.getByTestId('version-toggle').getAttribute('aria-expanded'), 'false')
    assert.deepEqual(errors, [])
    await context.close()
    console.log('PASS', width, dark ? 'dark' : 'light', locale)
  }
} finally { await browser.close(); await new Promise(done => server.close(done)) }
console.log('screenshots=' + output)
