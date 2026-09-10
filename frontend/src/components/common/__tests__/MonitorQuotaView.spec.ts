import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import type { MonitorQuotaSnapshot } from '@/api/admin/channelMonitor'
import MonitorQuotaView from '../MonitorQuotaView.vue'
import UsageProgressBar from '@/components/account/UsageProgressBar.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    // te() 恒真：已知 token 直接返回 i18n key，便于断言 window/label 映射。
    useI18n: () => ({
      t: (key: string, params?: { cost: string }) => params?.cost ? `${key} $${params.cost}` : key,
      te: () => true,
    }),
  }
})

function makeSnapshot(overrides: Partial<MonitorQuotaSnapshot> = {}): MonitorQuotaSnapshot {
  return {
    source: 'usage',
    success: true,
    fetched_at: '2026-08-18T00:00:00Z',
    ...overrides,
  }
}

describe('MonitorQuotaView', () => {
  it('renders nothing without a snapshot', () => {
    const wrapper = mount(MonitorQuotaView, { props: { snapshot: null } })
    expect(wrapper.find('[data-testid="monitor-quota-view"]').exists()).toBe(false)
    expect(wrapper.text()).toBe('')
  })

  it('maps tier window/label tokens through i18n and colors utilization', () => {
    const wrapper = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({
          tiers: [
            { window: '5h', used_percent: 42.4 },
            { window: '7d', label: 'pro', used_percent: 80 },
            { window: 'weekly', label: 'unknown-token', used_percent: 95 },
          ],
        }),
      },
    })

    const rows = wrapper.findAll('[data-testid="monitor-quota-tier"]')
    // tier 行只在 success 且有数据时渲染
    expect(rows).toHaveLength(3)
    const text = wrapper.text()
    // 已知 window token 走 i18n
    expect(text).toContain('monitorCommon.quota.windows.5h')
    expect(text).toContain('monitorCommon.quota.windows.7d')
    // 已知 label token 拼成 label/window
    expect(text).toContain('monitorCommon.quota.labels.pro/monitorCommon.quota.windows.7d')
    // 未知 label 原样透出（前向兼容）
    expect(text).toContain('unknown-token/monitorCommon.quota.windows.weekly')
    // 百分比取整
    expect(text).toContain('42%')
    expect(text).toContain('80%')
    expect(text).toContain('95%')

    const html = wrapper.html()
    // 阈值配色（三处共用的 UsageProgressBar 统一）：≥90 红 / ≥75 黄 / 其余绿
    // 42.4 → 绿、80 → 黄、95 → 红
    expect(html).toContain('bg-green-500')
    expect(html).toContain('bg-amber-500')
    expect(html).toContain('bg-red-500')
  })

  it('clamps the tier bar width into 0-100', () => {
    const wrapper = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({ tiers: [{ window: '5h', used_percent: 240 }] }),
      },
    })
    expect(wrapper.html()).toContain('width: 100%')
  })

  it('renders both measured windows, distinct A/U costs, reset and the account-cost weekly estimate', () => {
    const wrapper = mount(MonitorQuotaView, {
      props: {
        provider: 'openai',
        snapshot: makeSnapshot({ tiers: [
          { window: '5h', used_percent: 0, window_stats: { requests: 490, tokens: 54000000, cost: 93.18, user_cost: 46.59 } },
          { window: '7d', used_percent: 74, reset_at: new Date(Date.now() + 112 * 3600000 + 60000).toISOString(),
            window_stats: { requests: 11900, tokens: 1500000000, cost: 1615.59, user_cost: 807.79 } },
        ] }),
      },
    })
    const rows = wrapper.findAllComponents(UsageProgressBar)
    expect(rows[0].element.parentElement?.classList.contains('space-y-1')).toBe(true)
    expect(rows.every(row => !row.classes().includes('p-2.5'))).toBe(true)
    expect(rows[0].text()).toContain('490 req')
    expect(rows[0].text()).toContain('54.0M')
    expect(rows[0].text()).toContain('A $93.18')
    expect(rows[0].text()).toContain('U $46.59')
    expect(rows[0].text()).toContain('0%')
    expect(rows[0].text()).toContain('usage.resetNow')
    expect(rows[0].find('[data-test="estimated-total-cost"]').exists()).toBe(false)
    expect(rows[1].text()).toContain('11.9K req')
    expect(rows[1].text()).toContain('1.5B')
    expect(rows[1].text()).toContain('A $1615.59')
    expect(rows[1].text()).toContain('U $807.79')
    expect(rows[1].text()).toContain('74%')
    expect(rows[1].text()).toContain('4d 16h')
    expect(rows[1].get('[data-test="estimated-total-cost"]').text()).toContain('$2183.23')
    expect(rows[1].get('[data-test="estimated-total-cost"]').classes()).toEqual(expect.arrayContaining([
      'bg-emerald-100', 'dark:bg-emerald-900/40', 'whitespace-normal', 'block',
    ]))
    const stats = rows[1].get('[data-test="window-stats"]')
    expect(stats.findAll('span')).toHaveLength(4)
    expect(stats.classes()).not.toContain('flex-wrap')
    expect(stats.element.nextElementSibling).toBe(rows[1].get('[data-test="estimated-total-cost"]').element)
    wrapper.unmount()
  })

  it('keeps 5h blue and 7d green when windows are missing or reordered', async () => {
    const wrapper = mount(MonitorQuotaView, { props: { provider: 'openai' } })
    for (const windows of [['5h'], ['7d'], ['7d', '5h'], ['5h', '7d']]) {
      await wrapper.setProps({ snapshot: makeSnapshot({ tiers: windows.map(window => ({
        window, used_percent: 50, window_stats: { requests: 1, tokens: 100, cost: 50 },
      })) }) })
      expect(wrapper.findAllComponents(UsageProgressBar).map(row => row.props('color')))
        .toEqual(windows.map(window => window === '5h' ? 'indigo' : 'emerald'))
      wrapper.findAllComponents(UsageProgressBar).forEach((row, idx) => {
        const color = windows[idx] === '5h' ? 'indigo' : 'emerald'
        expect(row.get('[data-test="estimated-total-cost"]').classes()).toEqual(expect.arrayContaining([
          `bg-${color}-100`, `dark:bg-${color}-900/40`,
        ]))
      })
    }
    wrapper.unmount()
  })

  it.each(['5h', '7d'])('does not invent costs for legacy snapshots or estimates for zero/invalid %s usage', async (window) => {
    const wrapper = mount(MonitorQuotaView, {
      props: { provider: 'openai', snapshot: makeSnapshot({ tiers: [{ window, used_percent: 74 }] }) },
    })
    expect(wrapper.text()).not.toContain('A $')
    expect(wrapper.find('[data-test="estimated-total-cost"]').exists()).toBe(false)
    for (const [cost, percent] of [[1615.59, 0], [10, -1], [0, 74], [-10, 74], [NaN, 74], [Infinity, 74], [10, NaN], [10, Infinity], [Number.MAX_VALUE, 0.1]]) {
      await wrapper.setProps({ snapshot: makeSnapshot({ tiers: [{
        window, used_percent: percent, window_stats: { requests: 1, tokens: 1, cost },
      }] }) })
      expect(wrapper.find('[data-test="estimated-total-cost"]').exists()).toBe(false)
    }
    wrapper.unmount()
  })

  it('uses account cost and unrounded percentages only for OpenAI 5h/7d estimates', async () => {
    const wrapper = mount(MonitorQuotaView, {
      props: { provider: 'openai', snapshot: makeSnapshot({ tiers: [
        { window: '5h', used_percent: 25.6, window_stats: { requests: 1, tokens: 1, cost: 93.18, user_cost: 46.59 } },
        { window: '7d', used_percent: 74.4, window_stats: { requests: 1, tokens: 1, cost: 1615.59, user_cost: 807.79 } },
        { window: 'daily', used_percent: 50, window_stats: { requests: 1, tokens: 1, cost: 10 } },
      ] }) },
    })
    const rows = wrapper.findAllComponents(UsageProgressBar)
    expect(rows[0].props('estimatedTotalCost')).toBeCloseTo(93.18 * 100 / 25.6)
    expect(rows[1].props('estimatedTotalCost')).toBeCloseTo(1615.59 * 100 / 74.4)
    expect(rows[2].props('estimatedTotalCost')).toBeNull()
    expect(rows[0].get('[data-test="estimated-total-cost"]').text()).toContain('$363.98')
    for (const row of rows.slice(0, 2)) {
      expect(row.get('[data-test="window-stats"]').element.nextElementSibling)
        .toBe(row.get('[data-test="estimated-total-cost"]').element)
    }
    await wrapper.setProps({ provider: 'anthropic' })
    expect(wrapper.find('[data-test="estimated-total-cost"]').exists()).toBe(false)
    expect(wrapper.findComponent(UsageProgressBar).props('showNowWhenIdle')).toBe(false)
    wrapper.unmount()
  })

  it('shows the plan level badge and multi-currency balances', () => {
    const wrapper = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({
          plan_level: 'Max20',
          balances: [
            { currency: 'CNY', balance: 12.5 },
            { currency: 'USD', balance: 0 },
          ],
        }),
      },
    })

    expect(wrapper.text()).toContain('Max20')
    expect(wrapper.text()).toContain('12.50 CNY')
    expect(wrapper.text()).toContain('0.00 USD')
    // 余额为 0 用红色警示
    expect(wrapper.html()).toContain('text-red-600')
  })

  it('falls back to the single balance + currency pair', () => {
    const wrapper = mount(MonitorQuotaView, {
      props: { snapshot: makeSnapshot({ balance: 3.2, currency: 'CNY' }) },
    })
    expect(wrapper.text()).toContain('3.20 CNY')
  })

  it('renders a truncated error state when the fetch failed', () => {
    const longError = 'x'.repeat(60)
    const wrapper = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({ success: false, error: longError }),
      },
    })

    const error = wrapper.get('[data-testid="monitor-quota-error"]')
    expect(error.text()).toBe(`${'x'.repeat(48)}…`)
    expect(error.attributes('title')).toBe(longError)
  })

  it('keeps failed snapshots from rendering tier rows', () => {
    const wrapper = mount(MonitorQuotaView, {
      props: {
        snapshot: makeSnapshot({
          success: false,
          tiers: [{ window: '5h', used_percent: 10 }],
        }),
      },
    })
    expect(wrapper.text()).not.toContain('10%')
  })
})
