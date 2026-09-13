// Built UI with synthetic API fixtures only. No production login or writes.
import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { readFile, mkdtemp } from 'node:fs/promises'
import { extname, join, resolve, sep } from 'node:path'
import { tmpdir } from 'node:os'
import { fileURLToPath } from 'node:url'
const { chromium } = await import(process.env.SUB2API_PLAYWRIGHT_MODULE || 'playwright')
const dist = fileURLToPath(new URL('../../backend/internal/web/dist', import.meta.url))
const output = await mkdtemp(join(tmpdir(), 'sub2api-accounting-ui-'))
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
const items = ['automatic_unmetered', 'operator_decision', 'already_billed'].map((reason, i) => ({
  id: `synthetic-request-${i}`, user_id: 999, subscription_id: 1, group_id: 7,
  email: 'long.synthetic.customer.identity@example.invalid', group_name: 'Synthetic Pro 20x', account_id: 4, account_name: 'Synthetic source', cycle: 3,
  model: 'synthetic-model-with-long-name', reason, known_standard_usd: i === 2 ? 1.25 : null, reference_hold_usd: 999,
  started_at: '2026-09-12T12:00:00Z', absorbed_at: '2026-09-12T12:06:00Z', closed_at: null
}))
try {
  for (const [width, dark] of [[1440, false], [1440, true], [390, false], [390, true], [320, false], [320, true]]) {
    const context = await browser.newContext({ viewport: { width, height: 1000 }, timezoneId: 'Asia/Shanghai' })
    const page = await context.newPage()
    page.setDefaultTimeout(10_000)
    const errors = []
    let failDetails = false, failClear = false, clearWrites = 0, writes = 0
    const cleared = new Set()
    page.on('pageerror', error => errors.push(error.message))
    await page.addInitScript(({ user, dark }) => {
      localStorage.setItem('auth_token', 'synthetic-local-preview')
      localStorage.setItem('auth_user', JSON.stringify(user))
      localStorage.setItem('sub2api_locale', 'zh')
      localStorage.setItem('theme', dark ? 'dark' : 'light')
      localStorage.setItem('admin_guide_999_admin_v4_interactive', 'true')
    }, { user, dark })
    await page.route('**/*', async route => {
      const req = route.request(), url = new URL(req.url()), path = url.pathname
      if (url.origin !== origin) return route.abort()
      if (!path.startsWith('/api/') && path !== '/setup/status') return route.continue()
      if (path.endsWith('/absorbed-usage/clear') && req.method() === 'POST') {
        clearWrites++
        if (failClear) return route.fulfill({ status: 503, json: { code: 503, message: 'synthetic clear failure' } })
        const body = req.postDataJSON()
        assert.deepEqual(Object.keys(body), ['ids'])
        assert(body.ids.every(id => items.some(row => row.id === id)))
        let n = 0
        for (const id of body.ids) if (!cleared.has(id)) { cleared.add(id); n++ }
        return route.fulfill({ json: { code: 0, data: { cleared: n } } })
      }
      if (req.method() !== 'GET') { writes++; return route.abort() }
      let data = { items: [], total: 0, pages: 1, page: 1, page_size: 20 }
      if (path === '/setup/status') data = { needs_setup: false }
      else if (path.endsWith('/auth/me')) data = user
      else if (path.endsWith('/settings/public')) data = { site_name: 'Accounting Preview', registration_enabled: false }
      else if (path.endsWith('/absorbed-usage')) {
        assert.equal(url.searchParams.has('category'), false, 'no manual review query')
        const summaryOnly = url.searchParams.get('summary_only') === 'true'
        if (!summaryOnly && failDetails) return route.fulfill({ status: 503, json: { code: 503, message: 'synthetic failure' } })
        const visibility = url.searchParams.get('visibility') || 'uncleared'
        const visible = items.filter(row => visibility === 'all' || (visibility === 'cleared' ? cleared.has(row.id) : !cleared.has(row.id)))
        const known = visible.filter(row => row.known_standard_usd !== null)
        const totals = { requests: visible.length, known_requests: known.length, known_standard_usd: known.reduce((sum, row) => sum + row.known_standard_usd, 0), unknown_requests: visible.length - known.length }
        data = { ...data, summary: totals, items: summaryOnly ? [] : visible.map(row => ({ ...row, display_cleared_at: cleared.has(row.id) ? '2026-09-13T00:00:00Z' : null, closed_at: url.searchParams.get('scope') === 'history' ? '2026-09-13T00:00:00Z' : null })) }
      }
      return route.fulfill({ json: { code: 0, data } })
    })
    await page.goto(origin + '/admin/subscriptions')
    const button = page.getByTestId('absorption-summary')
    await button.getByText('未核实金额 2 条', { exact: true }).waitFor()
    assert.equal(await page.getByText('待复核', { exact: true }).count(), 0)
    await button.screenshot({ path: join(output, `${width}-${dark}-summary.png`), animations: 'disabled' })
    await button.click()
    const modal = page.getByRole('dialog'), panel = modal.locator('.modal-content')
    await page.getByTestId('absorption-status').first().waitFor()
    assert.deepEqual(await page.getByTestId('absorption-status').allTextContents(), ['已结案 · 未向用户计费', '已处理 · 站点承担', '已处理 · 原账已计费'])
    assert.equal(await modal.getByRole('button', { name: /补扣|承担|结案|charge|cover/i }).count(), 0)
    const fits = async () => {
      const rect = await panel.boundingBox()
      assert(rect.x >= 0 && rect.x + rect.width <= width + 1, JSON.stringify(rect))
      assert.equal(await panel.evaluate(el => el.scrollWidth > el.clientWidth + 1), false)
      for (const article of await panel.locator('article').all()) assert.equal(await article.evaluate(el => el.scrollWidth > el.clientWidth + 1), false)
    }
    await fits()
    await panel.screenshot({ path: join(output, `${width}-${dark}-details.png`), animations: 'disabled' })
    const badge = page.getByTestId('absorption-status').first()
    const before = await badge.evaluate(el => getComputedStyle(el).color)
    await page.evaluate(() => document.documentElement.classList.toggle('dark'))
    assert.notEqual(await badge.evaluate(el => getComputedStyle(el).color), before)
    await page.evaluate(() => document.documentElement.classList.toggle('dark'))
    await panel.locator('article details summary').first().click()
    await fits()
    assert((await panel.innerText()).includes('999.00'))
    await page.getByTestId('absorption-scope').selectOption('history')
    await panel.getByText('已归档', { exact: true }).first().waitFor({ state: 'attached' })
    failDetails = true
    await page.getByTestId('absorption-scope').selectOption('current')
    await panel.getByRole('alert').waitFor()
    await panel.screenshot({ path: join(output, `${width}-${dark}-error.png`), animations: 'disabled' })
    await fits()
    failDetails = false
    await panel.getByRole('button', { name: '刷新', exact: true }).click()
    await page.getByTestId('absorption-status').first().waitFor()
    failClear = true
    await page.getByTestId('absorption-select-row').first().check()
    await page.getByTestId('absorption-clear').click()
    await page.getByTestId('absorption-clear-feedback').filter({ hasText: /清理结果未确认/ }).waitFor()
    assert.equal(await page.getByTestId('absorption-select-row').first().isChecked(), true)
    await fits()
    await panel.screenshot({ path: join(output, `${width}-${dark}-clear-error.png`), animations: 'disabled' })
    failClear = false
    await page.getByTestId('absorption-select-all').check()
    await page.getByTestId('absorption-clear').click()
    await page.getByTestId('absorption-clear-feedback').filter({ hasText: /已清理 3 条/ }).waitFor()
    await button.filter({ hasText: /0 条/ }).waitFor({ state: 'attached' })
    await panel.screenshot({ path: join(output, `${width}-${dark}-clear-success.png`), animations: 'disabled' })
    await page.getByTestId('absorption-visibility').selectOption('cleared')
    await page.getByTestId('absorption-status').first().waitFor()
    assert.equal(await page.getByTestId('absorption-select-row').count(), 0)
    assert.equal(await page.getByTestId('absorption-clear').isDisabled(), true)
    await fits()
    await panel.screenshot({ path: join(output, `${width}-${dark}-cleared.png`), animations: 'disabled' })
    await page.keyboard.press('Escape')
    await modal.waitFor({ state: 'detached' })
    assert.equal(await button.getAttribute('aria-expanded'), 'false')
    assert.equal(writes, 0)
    assert.equal(clearWrites, 2)
    assert.deepEqual(errors, [])
    await context.close()
    console.log('PASS', width, dark ? 'dark' : 'light')
  }
} finally { await browser.close(); await new Promise(done => server.close(done)) }
console.log('screenshots=' + output)
