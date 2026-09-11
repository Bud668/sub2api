import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import zh from '@/i18n/locales/zh'
import DynamicQuotaCard from '../DynamicQuotaCard.vue'
import type { DynamicSubscriptionQuota } from '@/types'

describe('upstream cycle display', () => {
  it('separates adjustment from reset and never fabricates a local seven-day countdown', async () => {
    const quota: DynamicSubscriptionQuota = { enabled: true, revision: 1, weight: 1, max_limit_usd: 700, floor_limit_usd: 100, next_adjustment_percent: 40, cycle: 4, status: 'confirming', limit_usd: 100, used_usd: 20, reserved_usd: 5, remaining_usd: 75, started_at: '2026-09-01T01:00:00Z', updated_at: '', synced_at: '2026-09-09T01:00:00Z', expected_reset_at: '2026-09-12T01:00:00Z' }
    const wrapper = mount(DynamicQuotaCard, { props: { quota }, global: { plugins: [createI18n({ legacy: false, locale: 'zh', messages: { zh }, messageCompiler: message => () => String(message) })] } })
    expect(wrapper.text()).toContain('确认上游重置中')
    expect(wrapper.text()).toContain('尚未确认重置')
    expect(wrapper.text()).toContain('待确认')
    expect(wrapper.text()).toContain('$75.00')
    expect(wrapper.text()).not.toContain('09-08') // not started_at + 7 days
    await wrapper.setProps({ quota: { ...quota, limit_usd: 150, remaining_usd: 125 } })
    expect(wrapper.text()).toContain('$20.00')
    expect(wrapper.text()).toContain('$125.00')
    expect(wrapper.text()).toContain('#4')
    expect(wrapper.get('[role=progressbar]').attributes('aria-valuenow')).toBe('13')
    expect(wrapper.text()).not.toContain('尚未测出上游总额度')
    await wrapper.setProps({ quota: { ...quota, cycle: 1, status: 'learning', max_limit_usd: 200, limit_usd: 200, used_usd: 0, remaining_usd: 200 } })
    expect(wrapper.text()).toContain('尚未测出上游总额度')
    expect(wrapper.text()).not.toContain('80%')
    expect(wrapper.text()).toContain('分配上限')
    expect(wrapper.text()).toContain('下调保护')
    expect(wrapper.find('details').attributes('open')).toBeUndefined()
    expect(wrapper.text()).toContain('当前已分配')
    expect(wrapper.text()).toContain('$200.00')
    expect(wrapper.text()).toContain('周期编号不是上游账号编号')
    expect(wrapper.text()).toContain('仅供参考')
    await wrapper.setProps({ quota: { ...quota, status: 'active', growth_frozen: true } })
    expect(wrapper.text()).toContain('额度上调已冻结')
    expect(wrapper.text()).toContain('仍可使用本周期的可信剩余额度')
    expect(wrapper.text()).toContain('$75.00')
    expect(wrapper.text()).not.toContain('暂停')
    await wrapper.setProps({ quota: { ...quota, status: 'active', last_allocation_at: '2026-09-08T01:00:00Z', last_change: { previous_usd: 600, current_usd: 100, node: 30, reason: 'upstream_node', at: '2026-09-08T01:00:00Z' } } })
    expect(wrapper.text()).toContain('$600.00 → $100.00')
    expect(wrapper.text()).toContain('后重分配')
    expect(wrapper.text()).toContain('最近实际调额')
    expect(wrapper.text()).toContain('最近数据同步')
    expect(wrapper.text()).toContain('本周期起点')
    await wrapper.setProps({ quota: { ...quota, status: 'active', used_usd: 120, reserved_usd: 0, remaining_usd: 0 } })
    expect(wrapper.text()).toContain('当前额度已用尽')
    expect(wrapper.text()).toContain('$120.00')
    expect(wrapper.get('[role=progressbar]').attributes('aria-valuenow')).toBe('100')
    await wrapper.setProps({ compact: true, quota: { ...quota, status: 'active', remaining_usd: 0 } })
    expect(wrapper.text()).toContain('额度正在处理中')
    expect(wrapper.find('details').exists()).toBe(false)
  })
})
