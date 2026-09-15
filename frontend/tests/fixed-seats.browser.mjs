// Synthetic APIs only. Serves the locally built UI on a random loopback port;
// every non-local browser request is rejected. No live credentials or writes.
import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { readFile, mkdtemp } from 'node:fs/promises'
import { extname, join, resolve, sep } from 'node:path'
import { tmpdir } from 'node:os'
import { fileURLToPath } from 'node:url'

const { chromium } = await import(process.env.SUB2API_PLAYWRIGHT_MODULE || 'playwright')
const dist = fileURLToPath(new URL('../../backend/internal/web/dist', import.meta.url))
const output = await mkdtemp(join(tmpdir(), 'sub2api-fixed-seats-ui-'))
const mime = { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png', '.woff2': 'font/woff2' }
const server = createServer(async (req, res) => {
  try {
    const path = new URL(req.url, 'http://localhost').pathname
    const file = resolve(dist, `.${extname(path) ? path : '/index.html'}`)
    assert(file.startsWith(dist + sep))
    res.setHeader('Content-Type', mime[extname(file)] || 'application/octet-stream')
    res.end(await readFile(file))
  } catch { res.writeHead(404).end() }
})
await new Promise(done => server.listen(0, '127.0.0.1', done))
const origin = `http://127.0.0.1:${server.address().port}`
const browser = await chromium.launch({ headless: true })
const stamp = '2026-09-12T05:00:00Z'
const user = { id: 999, email: 'preview@example.invalid', username: 'Preview', role: 'admin', status: 'active', balance: 0 }
const groups = [7, 8, 9].map(id => ({ id, name: `Preview Pro ${id}`, platform: 'openai', subscription_type: 'subscription', rate_multiplier: 1, status: 'active', account_count: 1, active_account_count: 1 }))
const base = { enabled: true, requested_enabled: true, activation_pending: false, revision: 1, account_id: 4, weight: 1, max_limit_usd: 600, fixed_slots: 4, source_fixed_slots: 8, floor_limit_usd: null, cycle: 1, limit_usd: 320, used_usd: 50, remaining_usd: 267.31, reserved_usd: 2.69, status: 'active', started_at: stamp, updated_at: stamp, synced_at: stamp, expected_reset_at: '2026-09-19T05:00:00Z', next_adjustment_percent: 15, last_change_usd: 20, last_allocation_at: stamp, last_change: { previous_usd: 300, current_usd: 320, node: 10, reason: 'upstream_node', at: stamp } }
const fits = async locator => assert.equal(await locator.evaluate(el => el.scrollWidth > el.clientWidth + 1), false, 'no horizontal overflow')
async function checkCard(page, card, dark, locale) {
  await fits(card)
  const metrics = await card.locator('.quota-amounts').evaluate(el => ({
    order: [...el.children].map(node => node.dataset.testid),
    columns: getComputedStyle(el).gridTemplateColumns.trim().split(/\s+/).length,
    titles: [...el.querySelectorAll('dt')].map(dt => { const label = dt.firstElementChild || dt; const s = getComputedStyle(label); return [s.fontWeight, s.color] }),
    amounts: [...el.querySelectorAll('.quota-amount')].map(dd => [getComputedStyle(dd).fontWeight, getComputedStyle(dd).textAlign])
  }))
  assert.deepEqual(metrics.order, ['dynamic-allocated', 'dynamic-used', 'dynamic-remaining'])
  assert.deepEqual(metrics.titles, Array.from({ length: 3 }, () => ['600', dark ? 'rgb(229, 231, 235)' : 'rgb(55, 65, 81)']))
  assert.deepEqual(metrics.amounts, [['700', 'left'], ['700', metrics.columns === 2 ? 'left' : 'center'], ['700', 'right']])
  assert(!(await card.innerText()).includes('US$'))
  assert.equal(await card.getByTestId('dynamic-quota-delta').innerText(), '+$20.00')
  assert.equal(await card.getByTestId('dynamic-reserved').innerText(), locale === 'zh' ? '在途预占 · $2.69' : 'In flight · $2.69')
  assert.equal(await card.getByTestId('dynamic-used').locator('dt').getByTestId('dynamic-reserved').count(), 1)
  assert((await card.getByTestId('dynamic-cap').innerText()).includes('$600.00'))
  assert.notEqual(await card.getByTestId('dynamic-cap').evaluate(el => getComputedStyle(el).backgroundColor), 'rgba(0, 0, 0, 0)')
  assert.equal(await card.getByTestId('dynamic-cap').evaluate(el => getComputedStyle(el).backgroundColor), await card.getByTestId('dynamic-reserved').evaluate(el => getComputedStyle(el).backgroundColor))
  assert.equal(await card.getByTestId('dynamic-quota-meta').count(), 1)
  assert.equal(await card.getByTestId('dynamic-start').locator('dt').innerText(), locale === 'zh' ? '本周期起点' : 'Cycle started')
  assert(!(await card.getByTestId('dynamic-quota-meta').innerText()).includes('#1'))
  const metaLayout = await card.getByTestId('dynamic-quota-meta').evaluate(el => ({ columns: getComputedStyle(el).gridTemplateColumns.trim().split(/\s+/).length, padding: getComputedStyle(el).paddingLeft, align: [...el.children].map(node => getComputedStyle(node).textAlign) }))
  assert.equal(metaLayout.padding, '12px')
  assert.deepEqual(metaLayout.align, metaLayout.columns === 4 ? ['left', 'center', 'center', 'right'] : ['left', 'right', 'left', 'right'])
  assert.equal(await card.locator('details').count(), 0)
  assert.equal(await card.locator('[data-testid=dynamic-bounds]').getByText(/下调保护|Downward protection/).count(), 0)
  const badge = card.getByTestId('fixed-seat-badge')
  assert.equal(await badge.count(), 1)
  assert((await badge.innerText()).includes('· 6'))
  assert.deepEqual(await badge.evaluate(el => [getComputedStyle(el).color, getComputedStyle(el).fontWeight]), [dark ? 'rgb(147, 197, 253)' : 'rgb(29, 78, 216)', '600'])
  await badge.click()
  const tooltip = page.locator('[role=tooltip]:visible')
  await tooltip.waitFor()
  const rect = await tooltip.boundingBox(), viewport = page.viewportSize()
  assert(rect.x >= 0 && rect.y >= 0 && rect.x + rect.width <= viewport.width + 1 && rect.y + rect.height <= viewport.height + 1)
  assert((await tooltip.innerText()).includes('10'), 'distinguish group seats from shared-source seats')
  await page.keyboard.press('Escape')
  assert.equal(await badge.getAttribute('aria-expanded'), 'false')
}
async function checkDebug(page, card, locale, dark, limit) {
  await fits(card)
  const usage = card.getByTestId('admin-debug-usage')
  assert.deepEqual(await usage.locator('.debug-summary dt').allTextContents(), locale === 'zh' ? ['周额度', '本周已用', '剩余可用'] : ['Weekly quota', 'Used this week', 'Available'])
  const amounts = usage.locator('.debug-amount')
  for (const [i, value] of [limit, 50, limit - 52].entries()) assert((await amounts.nth(i).innerText()).includes(value.toFixed(2)))
  const boxes = await Promise.all([0, 1, 2].map(i => amounts.nth(i).boundingBox()))
  if ((await usage.boundingBox()).width > 512) {
    assert(boxes[0].x < boxes[1].x && boxes[1].x < boxes[2].x, 'used in the middle, available on the right')
    assert(Math.abs(boxes[0].y - boxes[1].y) < 1 && Math.abs(boxes[1].y - boxes[2].y) < 1, 'desktop amounts align')
  } else {
    assert(boxes[0].y < boxes[1].y && Math.abs(boxes[1].y - boxes[2].y) < 1, 'mobile used and available align below the limit')
  }
  const badge = card.getByTestId('admin-debug-badge')
  assert.equal(await badge.count(), 1)
  assert.equal(await card.getByTestId('fixed-seat-badge').count(), 0, 'debug does not claim a fixed seat')
  assert((await badge.locator('..').locator('..').innerText()).includes('Preview Pro 9'), 'debug badge belongs beside group name, not user identity')
  await fits(badge)
  assert.deepEqual(await badge.evaluate(el => [getComputedStyle(el).color, getComputedStyle(el).fontWeight, getComputedStyle(el).borderRadius]), [dark ? 'rgb(196, 181, 253)' : 'rgb(109, 40, 217)', '600', '6px'])
  await badge.click()
  await page.locator('[role=tooltip]:visible').waitFor()
  await page.keyboard.press('Escape')
  assert.equal(await badge.getAttribute('aria-expanded'), 'false')
}
try {
  for (const [width, locale, dark] of [[1440, 'zh', false], [1440, 'zh', true], [390, 'zh', false], [390, 'zh', true], [320, 'zh', false], [1440, 'en', true], [390, 'en', false]]) {
    const context = await browser.newContext({ viewport: { width, height: 1000 }, timezoneId: 'Asia/Shanghai' })
    const page = await context.newPage()
    await page.clock.install({ time: new Date(stamp) })
    page.setDefaultTimeout(8000)
    const errors = []
    page.on('pageerror', e => errors.push(e.message))
    let policy = { group_id: 7, account_id: 4, revision: 1, enabled: true, weight: 1, max_limit_usd: 600, fixed_slots: 4, floor_limit_usd: null }
    const rows = groups.map((group, i) => ({ id: i + 11, user_id: 999, group_id: group.id, group, user, status: 'active', starts_at: stamp, expires_at: new Date(Date.parse(stamp) + [23, 7, 3][i] * 86400_000).toISOString(), weekly_usage_usd: 50, admin_debug: i === 2, dynamic_quota: i === 2 ? null : { ...base, ...(i === 1 ? { next_adjustment_percent: 40, pending_adjustment_percent: 35, pending_adjustment_reason: 'sync_recovery', growth_frozen: true, growth_frozen_reason: 'sync_recovery', last_change_usd: -20 } : {}) }, admin_debug_quota: i === 2 ? { weekly_limit_usd: 120, remaining_usd: 68, reserved_usd: 2, revision: 1, follow_reset: true, reset_pending: false, expected_reset_at: '2026-09-19T05:00:00Z' } : null }))
    await page.addInitScript(({ user, locale, dark }) => {
      localStorage.setItem('auth_token', 'synthetic-local-preview')
      localStorage.setItem('auth_user', JSON.stringify(user))
      localStorage.setItem('sub2api_locale', locale)
      localStorage.setItem('admin_guide_999_admin_v4_interactive', 'true')
      localStorage.setItem('admin_guide_guest_user_v4_interactive', 'true')
      localStorage.setItem('theme', dark ? 'dark' : 'light')
    }, { user, locale, dark })
    await page.route('**/*', async route => {
      const request = route.request(), url = new URL(request.url()), path = url.pathname
      if (url.origin !== origin) return route.abort()
      if (!path.startsWith('/api/') && path !== '/setup/status') return route.continue()
      let data = { items: [], total: 0, pages: 1, page: 1, page_size: 20 }
      if (path === '/setup/status') data = { needs_setup: false }
      else if (path.endsWith('/auth/me')) data = user
      else if (path.endsWith('/settings/public')) data = { site_name: 'Sub2API Preview', registration_enabled: false, payment_enabled: false }
      else if (path.endsWith('/groups/7/dynamic-quota')) {
        if (request.method() === 'PUT') {
          if (request.postDataJSON().fixed_slots === 5) return route.fulfill({ status: 409, json: { code: 409, reason: 'DYNAMIC_QUOTA_SEATS_SPENT', message: 'Synthetic conflict' } })
          policy = { ...policy, ...request.postDataJSON(), revision: policy.revision + 1 }
          rows[0].dynamic_quota.fixed_slots = policy.fixed_slots
          rows.forEach(row => { if (row.dynamic_quota) row.dynamic_quota.source_fixed_slots = policy.fixed_slots + 4 })
        }
        data = { policy, sources: [{ id: 4, name: 'Preview upstream' }], members: 1, debug_members: 0, legacy_members: 0, effective_slots: policy.fixed_slots, occupied_slots: 1, source_slots: policy.fixed_slots + 4 }
      } else if (path.endsWith('/admin/subscriptions/13/admin-debug/quota')) {
        if (request.postDataJSON().weekly_limit_usd === 444) return route.fulfill({ status: 409, json: { code: 409, reason: 'DYNAMIC_QUOTA_CHANGED', message: 'Synthetic conflict' } })
        rows[2].admin_debug_quota = { ...rows[2].admin_debug_quota, ...request.postDataJSON(), revision: rows[2].admin_debug_quota.revision + 1 }
        rows[2].admin_debug_quota.remaining_usd = rows[2].admin_debug_quota.weekly_limit_usd - 52
        data = rows[2]
      } else if (path.endsWith('/admin/subscriptions/13')) data = rows[2]
      else if (path.endsWith('/groups/dynamic-quotas')) data = [policy]
      else if (path.endsWith('/groups/all')) data = groups
      else if (path.endsWith('/admin/groups')) data = { ...data, items: [groups[0]], total: 1 }
      else if (path.endsWith('/model-allowlist-candidates')) data = { models: ['synthetic-model'] }
      else if (path.endsWith('/groups/live-capability')) data = { supported: false }
      else if (path.endsWith('/subscriptions/absorbed-usage')) data = { ...data, summary: { requests: 0, known_requests: 0, known_standard_usd: 0, unknown_requests: 0 } }
      else if (path.endsWith('/admin/subscriptions')) data = { ...data, items: rows, total: rows.length }
      else if (['/api/v1/subscriptions', '/api/v1/subscriptions/active'].includes(path)) data = rows
      else if (path.endsWith('/groups/usage-summary') || path.endsWith('/groups/capacity-summary')) data = []
      return route.fulfill({ json: { code: 0, data } })
    })
    const prefix = `${width}-${locale}-${dark ? 'dark' : 'light'}`
    await page.goto(origin + '/admin/groups')
    await page.getByRole('button', { name: locale === 'zh' ? '分组动态额度' : 'Group dynamic quota', exact: true }).click()
    const dialog = page.getByRole('dialog')
    await page.locator('#dynamic-slots').fill('5')
    await page.getByTestId('dynamic-seat-change').waitFor()
    await page.getByTestId('dynamic-save').click()
    await dialog.getByRole('alert').waitFor()
    assert.equal(await page.locator('#dynamic-slots').inputValue(), '5')
    assert.equal(policy.revision, 1)
    await page.locator('#dynamic-slots').fill('6')
    await page.getByTestId('dynamic-save').click()
    await dialog.getByRole('status').filter({ hasText: locale === 'zh' ? '保存成功' : 'Saved successfully' }).waitFor()
    assert.equal(policy.revision, 2)
    assert(await dialog.isVisible(), 'saved settings stay open for review')
    await fits(dialog.locator('.modal-content'))
    await dialog.screenshot({ path: join(output, `${prefix}-settings.png`) })
    await page.goto(origin + '/admin/subscriptions')
    let card = page.locator('[data-table-card]').first()
    await card.waitFor()
    const localizedCardCheck = async (target, targetDark) => {
      await checkCard(page, target, targetDark, locale)
      const meta = await target.getByTestId('dynamic-quota-meta').innerText()
      assert(meta.includes(locale === 'zh' ? '本周期起点' : 'Cycle started'))
      assert(!meta.includes('#1'))
      assert(meta.includes(locale === 'zh' ? '最近数据同步' : 'Latest data sync'))
      const status = target.getByTestId('subscription-status')
      if (await status.count()) {
        await status.locator('button').click()
        const tooltip = page.locator('[role=tooltip]:visible')
        await tooltip.waitFor()
        assert((await tooltip.innerText()).includes(locale === 'zh' ? '订阅额度周期: #1' : 'Subscription quota cycle: #1'))
        await page.keyboard.press('Escape')
      }
    }
    await localizedCardCheck(card, dark)
    await card.screenshot({ path: join(output, `${prefix}-admin.png`) })
    const pendingCard = page.locator('[data-table-card]').nth(1)
    assert((await pendingCard.getByTestId('dynamic-pending-stage').innerText()).includes('35%'))
    const decrease = pendingCard.getByTestId('dynamic-quota-delta')
    assert((await decrease.getAttribute('class')).includes(dark ? 'dark:bg-red-950/50' : 'bg-red-50'))
    assert.notEqual(await decrease.evaluate(el => getComputedStyle(el).backgroundColor), 'rgba(0, 0, 0, 0)')
    assert.equal(await pendingCard.getByTestId('subscription-status').innerText(), locale === 'zh' ? '同步恢复中 · 可用' : 'Sync recovering · Usable')
    assert((await pendingCard.innerText()).includes(locale === 'zh' ? '上游额度同步正在恢复确认' : 'Upstream quota synchronization is being reconfirmed'))
    await pendingCard.screenshot({ path: join(output, `${prefix}-pending.png`) })
    const debugCard = page.locator('[data-table-card]').nth(2)
    await checkDebug(page, debugCard, locale, dark, 120)
    await debugCard.screenshot({ path: join(output, `${prefix}-admin-debug.png`) })
    const checkExpiry = async () => {
      const badges = page.getByTestId('subscription-expiry-badge')
      for (const [i, color] of ['green', 'yellow', 'red'].entries()) {
        const badge = badges.nth(i)
        assert((await badge.getAttribute('class')).includes(`bg-${color}-100`))
        assert((await badge.getAttribute('class')).includes(`dark:bg-${color}-900/40`))
        assert.notEqual(await badge.evaluate(el => getComputedStyle(el).backgroundColor), 'rgba(0, 0, 0, 0)')
        await fits(badge)
      }
    }
    await checkExpiry()
    const initialBadgeColor = await page.getByTestId('subscription-expiry-badge').first().evaluate(el => getComputedStyle(el).backgroundColor)
    await page.evaluate(() => document.documentElement.classList.toggle('dark'))
    await localizedCardCheck(card, !dark)
    await checkDebug(page, debugCard, locale, !dark, 120)
    await checkExpiry()
    assert.notEqual(await page.getByTestId('subscription-expiry-badge').first().evaluate(el => getComputedStyle(el).backgroundColor), initialBadgeColor)
    await page.evaluate(() => document.documentElement.classList.toggle('dark'))
    await localizedCardCheck(card, dark)
    assert.equal(await debugCard.getByTestId('fixed-seat-badge').count(), 0)
    await debugCard.getByRole('button', { name: locale === 'zh' ? '更多' : 'More', exact: true }).click()
    await page.getByRole('button', { name: locale === 'zh' ? '设置周额度' : 'Set weekly quota', exact: true }).click()
    await page.locator('#admin-debug-weekly-limit').fill('444')
    await dialog.getByRole('button', { name: locale === 'zh' ? '保存' : 'Save', exact: true }).click()
    await dialog.getByRole('alert').waitFor()
    assert.equal(await page.locator('#admin-debug-weekly-limit').inputValue(), '444')
    assert.equal(rows[2].weekly_usage_usd, 50)
    await page.locator('#admin-debug-weekly-limit').fill('250')
    await dialog.getByRole('button', { name: locale === 'zh' ? '保存' : 'Save', exact: true }).click()
    await dialog.getByRole('status').filter({ hasText: locale === 'zh' ? '保存成功' : 'Weekly quota saved' }).waitFor()
    assert(await dialog.isVisible())
    assert.equal(rows[2].admin_debug_quota.weekly_limit_usd, 250)
    assert.equal(rows[2].weekly_usage_usd, 50)
    await fits(dialog.locator('.modal-content'))
    await dialog.screenshot({ path: join(output, `${prefix}-debug-settings.png`) })
    await page.goto(origin + '/subscriptions')
    card = page.getByTestId('subscription-card').first()
    await card.waitFor()
    await localizedCardCheck(card, dark)
    assert.equal(await page.getByTestId('dynamic-next-adjustment').count(), 0, 'user subscriptions hide adjustment milestones')
    assert.equal(await page.getByTestId('dynamic-pending-stage').count(), 0, 'user subscriptions hide pending milestones')
    assert.equal(await card.getByTestId('dynamic-last-adjustment').count(), 0)
    const userCardText = await card.innerText()
    assert(!userCardText.includes(locale === 'zh' ? '上游达到' : 'Upstream reached'), 'user subscription details hide prior milestone')
    const cardBox = await card.boundingBox(), statusBox = await card.getByTestId('subscription-status').boundingBox()
    assert(Math.abs(cardBox.x + cardBox.width - statusBox.x - statusBox.width - 16) < 2, 'user status aligns to card right')
    assert(Math.abs(statusBox.y - cardBox.y - 12) < 2, 'user status aligns to card top')
    await checkExpiry()
    await card.screenshot({ path: join(output, `${prefix}-user.png`) })
    await checkDebug(page, page.getByTestId('subscription-card').nth(2), locale, dark, 250)
    await page.getByTestId('subscription-card').nth(2).screenshot({ path: join(output, `${prefix}-debug.png`) })
    await page.getByTitle(locale === 'zh' ? '查看订阅详情' : 'View subscription details', { exact: true }).click()
    await page.waitForFunction(() => document.querySelectorAll('[data-testid=subscription-card]').length === 6)
    const cards = page.getByTestId('subscription-card')
    assert.equal(await page.getByTestId('fixed-seat-badge').count(), 4)
    assert.equal(await page.getByTestId('dynamic-next-adjustment').count(), 0, 'header subscription details hide adjustment milestones')
    for (const item of await cards.all()) await fits(item)
    const headerDebug = cards.filter({ has: page.getByTestId('admin-debug-usage') }).first()
    await checkDebug(page, headerDebug, locale, dark, 250)
    await headerDebug.screenshot({ path: join(output, `${prefix}-header-debug.png`) })
    rows[1].dynamic_quota.growth_frozen_reason = 'estimate_anomaly'
    rows[1].dynamic_quota.pending_adjustment_reason = 'estimate_anomaly'
    await page.goto(origin + '/subscriptions')
    assert.equal(await page.getByTestId('subscription-card').nth(1).getByTestId('subscription-status').innerText(), locale === 'zh' ? '额度异常核验 · 可用' : 'Allowance review · Usable')
    delete rows[1].dynamic_quota.pending_adjustment_percent
    rows[1].dynamic_quota.growth_frozen = false
    delete rows[1].dynamic_quota.growth_frozen_reason
    rows[1].dynamic_quota.next_adjustment_percent = 45
    await page.goto(origin + '/subscriptions')
    assert.equal(await page.getByTestId('dynamic-next-adjustment').count(), 0)
    assert.equal(rows[1].dynamic_quota.used_usd, 50)
    assert.equal(rows[1].dynamic_quota.status, 'active')
    await page.clock.fastForward(20 * 86400_000 + 60_000)
    await page.waitForFunction(() => [...document.querySelectorAll('[data-testid=subscription-expiry-badge]')].every(el => el.classList.contains('bg-red-100')))
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth + 1), false)
    assert.deepEqual(errors, [])
    console.log(`PASS ${prefix}: settings/error/save, admin/user/header quota change and metadata, debug available/order/editing, expiry colors/time updates, bold metrics, light/dark switching and no overflow`)
    await context.close()
  }
} finally {
  await browser.close()
  await new Promise(done => server.close(done))
  console.log(`Screenshots: ${output}`)
}
