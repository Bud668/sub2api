import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import Panel from '../SubscriptionAbsorptionPanel.vue'
import messages from '@/i18n/locales/zh/common'
import type { AbsorbedUsageReport } from '@/api/admin/subscriptions'

const { query } = vi.hoisted(() => ({ query: vi.fn() }))
vi.mock('@/api/admin/subscriptions', () => ({ getAbsorbedUsage: query }))
const report = (n = 14, amount = 12.5, unknown = 3): AbsorbedUsageReport => ({
  summary: { requests: n, known_requests: n - unknown, known_standard_usd: amount, unknown_requests: unknown },
  items: [], page: 1, page_size: 20, pages: 1
})
const render = () => mount(Panel, {
  props: { filters: {}, refreshKey: 0 },
  global: { plugins: [createI18n({ legacy: false, locale: 'zh', messages: { zh: messages }, messageCompiler: message => context => String(message).replace(/\{(\w+)\}/g, (_, name) => String(context.named(name))) })],
    stubs: { BaseDialog: { props: ['show'], template: '<div v-if="show"><slot/><slot name="footer"/></div>' } } }
})

describe('subscription toolbar site-covered usage', () => {
  beforeEach(() => { query.mockReset(); query.mockResolvedValue(report()) })
  it('loads counts and a verified subtotal without opening the details', async () => {
    const w = render(); await flushPromises()
    const b = w.get('[data-testid="absorption-summary"]')
    expect(b.text()).toContain('14 条'); expect(b.text()).toContain('已确认合计'); expect(b.text()).toContain('12.50'); expect(b.text()).toContain('未核实金额 3 条')
    expect(query).toHaveBeenCalledWith({ scope: 'current', summary_only: true }, expect.any(AbortSignal))
    expect(w.find('[data-testid="absorption-scope"]').exists()).toBe(false)
    w.unmount()
  })
  it('keeps zero visible and does not show a failed read as zero', async () => {
    query.mockResolvedValueOnce(report(0, 0, 0))
    const w = render(); await flushPromises()
    expect(w.get('button').text()).toContain('0 条'); expect(w.get('button').text()).toContain('总金额')
    query.mockRejectedValueOnce(new Error('offline'))
    await w.setProps({ refreshKey: 1 }); await flushPromises()
    expect(w.get('button').text()).toContain('读取失败'); expect(w.get('button').text()).not.toContain('0 条')
    w.unmount()
  })
  it('does not label entirely unpriced usage as a zero-dollar total', async () => {
    query.mockResolvedValueOnce(report(14, 0, 14))
    const w = render(); await flushPromises()
    expect(w.get('button').text()).toContain('无已核实金额')
    expect(w.get('button').text()).not.toContain('$0.00')
    w.unmount()
  })
  it('ignores a slow old filter result and refreshes all matching records', async () => {
    let resolveOld!: (r: AbsorbedUsageReport) => void
    query.mockImplementationOnce(() => new Promise(resolve => { resolveOld = resolve }))
    const w = render()
    query.mockResolvedValueOnce(report(2, 7, 0))
    await w.setProps({ filters: { user_id: 7, group_id: 4 }, refreshKey: 1 }); await flushPromises()
    resolveOld(report(99, 999, 99)); await flushPromises()
    expect(w.get('button').text()).toContain('2 条'); expect(w.get('button').text()).not.toContain('99 条')
    expect(query).toHaveBeenLastCalledWith({ user_id: 7, group_id: 4, scope: 'current', summary_only: true }, expect.any(AbortSignal))
    w.unmount()
  })
  it('opens read-only details with an email and separates historical cycles', async () => {
    const w = render(); await flushPromises()
    const details = report()
    details.items = [{ id: 'synthetic-request', user_id: 7, email: 'reader@example.invalid', subscription_id: 1, group_id: 4, group_name: 'Synthetic group', account_id: 5, account_name: 'Synthetic source', cycle: 1, model: 'synthetic-model', reason: 'missing_evidence', known_standard_usd: null, reference_hold_usd: 999, started_at: '2026-09-10T00:00:00Z', absorbed_at: '2026-09-10T00:06:00Z', closed_at: null }]
    details.items.push({ ...details.items[0], id: 'already-billed-request', reason: 'already_billed', known_standard_usd: 0 })
    query.mockResolvedValueOnce(details)
    await w.get('[data-testid="absorption-summary"]').trigger('click'); await flushPromises()
    expect(w.get('[data-testid="absorption-details"]').text()).toContain('reader@example.invalid')
    const rows = w.findAll('[data-testid="absorption-details"] article')
    expect(rows[0].text()).toContain('实际金额无法核实')
    expect(rows[0].get('[data-testid="absorption-status"]').text()).toBe('已处理 · 站点承担')
    expect(rows[1].get('[data-testid="absorption-status"]').text()).toBe('已处理 · 原账已计费')
    expect(w.text()).toContain('无需手工核销，不再追加扣款')
    expect(w.get('[data-testid="absorption-summary"]').text()).not.toContain('999')
    await w.get('select').setValue('history'); await flushPromises()
    expect(query).toHaveBeenLastCalledWith({ scope: 'history', page: 1, page_size: 20 }, expect.any(AbortSignal))
    expect(query.mock.calls.every(([params]) => !('execute' in params))).toBe(true)
    w.unmount()
  })

  it('shows automatic closure as read-only without review or payment actions', async () => {
    const w = render()
    await flushPromises()
    const details = report(1, 0, 1)
    details.items = [{ id: 'automatic-request', user_id: 7, email: 'reader@example.invalid', subscription_id: 1, group_id: 4, group_name: 'Synthetic group', account_id: 5, account_name: 'Synthetic source', cycle: 1, model: 'synthetic', reason: 'automatic_unmetered', known_standard_usd: null, reference_hold_usd: 999, started_at: '2026-09-10T00:00:00Z', absorbed_at: '2026-09-10T00:06:00Z', closed_at: null }]
    query.mockResolvedValue(details)
    await w.get('[data-testid=absorption-summary]').trigger('click'); await flushPromises()
    expect(w.get('[data-testid=absorption-status]').text()).toBe('已结案 · 未向用户计费')
    expect(w.text()).toContain('不按预占估算扣款')
    expect(w.find('[data-testid=accounting-charge]').exists()).toBe(false)
    expect(w.find('[data-testid=accounting-cover]').exists()).toBe(false)
    expect(query.mock.calls.every(([params]) => !('category' in params))).toBe(true)
    expect(w.find('[data-testid=absorption-details] details').text()).toContain('999')
    expect(w.find('.min-w-\\[760px\\]').exists()).toBe(false)
    w.unmount()
  })
})
