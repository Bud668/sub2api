import { describe, expect, it } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import zh from '@/i18n/locales/zh'
import DynamicQuotaCard from '../DynamicQuotaCard.vue'
import type { DynamicSubscriptionQuota } from '@/types'

describe('upstream cycle display', () => {
  it('shows fixed adjustment nodes in both themes without an independent learning stage', async () => {
    const quota: DynamicSubscriptionQuota = { enabled: true, revision: 1, weight: 1, fixed_slots: 4, max_limit_usd: 600, cycle: 1, status: 'active', limit_usd: 320, used_usd: 50, reserved_usd: 0, remaining_usd: 270, started_at: '', updated_at: '', next_adjustment_percent: 20 }
    for (const dark of [false, true]) {
      document.documentElement.classList.toggle('dark', dark)
      const wrapper = mount(DynamicQuotaCard, { props: { quota, compact: true }, global: { plugins: [createI18n({ legacy: false, locale: 'zh', messages: { zh }, messageCompiler: message => ctx => String(message).replace(/\{(\w+)\}/g, (_, key) => String(ctx.named(key))) })] } })
      const badge = wrapper.get('[data-testid=dynamic-next-adjustment]')
      const amounts = () => wrapper.findAll('.quota-amount').map(dd => dd.text())
      const initialAmounts = amounts()
      expect(badge.text()).toBe('下次调额 · 20%')
      expect(badge.attributes('title')).toContain('预计总费用')
      expect(badge.attributes('title')).toContain('阈值保护')
      expect(badge.attributes('title')).not.toContain('样本')
      await badge.trigger('click'); await flushPromises()
      expect(badge.attributes('aria-expanded')).toBe('true')
      expect(document.getElementById(badge.attributes('aria-describedby'))?.textContent).toContain('固定名额')
      document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' })); await flushPromises()
      expect(badge.attributes('aria-expanded')).toBe('false')
      await wrapper.setProps({ quota: { ...quota, next_adjustment_percent: 45, growth_frozen: true, pending_adjustment_percent: 40, pending_adjustment_reason: 'guard' } })
      expect(badge.text()).toBe('下次调额 · 45%')
      expect(wrapper.get('[role=status]').text()).toContain('等待上游数据校验通过')
      expect(amounts()).toEqual(initialAmounts)
      await wrapper.setProps({ quota: { ...quota, next_adjustment_percent: 0 } })
      expect(badge.text()).toBe('暂无下一节点')
      expect(badge.attributes('title')).not.toContain('0%')
      wrapper.unmount()
    }
    document.documentElement.classList.remove('dark')
  })

  it('separates an overdue allocation from the next milestone and displays both bounds in compact mode', () => {
    const quota: DynamicSubscriptionQuota = { enabled: true, revision: 1, weight: 1, max_limit_usd: 600, floor_limit_usd: 400, cycle: 1, status: 'active', limit_usd: 600, used_usd: 180, reserved_usd: 0, remaining_usd: 420, started_at: '', updated_at: '', next_adjustment_percent: 50, pending_adjustment_percent: 40, pending_adjustment_reason: 'budget_conflict' }
    const wrapper = mount(DynamicQuotaCard, { props: { quota, compact: true }, global: { plugins: [createI18n({ legacy: false, locale: 'zh', messages: { zh }, messageCompiler: message => ctx => String(message).replace(/\{(\w+)\}/g, (_, key) => String(ctx.named(key))) })] } })
    expect(wrapper.get('[data-testid=dynamic-pending-stage]').text()).toContain('40% 节点待分配')
    expect(wrapper.get('[data-testid=dynamic-pending-stage]').text()).toContain('下调保护合计超出')
    expect(wrapper.get('[data-testid=dynamic-allocated] [data-testid=dynamic-next-adjustment]').text()).toContain('下次调额 · 50%')
    expect(wrapper.get('[data-testid=dynamic-allocated] dt').classes()).not.toContain('justify-between')
    expect(wrapper.get('[data-testid=dynamic-next-adjustment]').attributes('title')).toContain('绑定上游账号的 7 日总用量')
    expect(wrapper.get('[data-testid=dynamic-next-adjustment]').attributes('title')).toContain('已用量不会清零')
    expect(wrapper.get('[data-testid=dynamic-remaining]').text()).not.toContain('下次调额')
    expect(wrapper.get('[data-testid=dynamic-allocated]').text()).toContain('尚未自动分配')
    expect(wrapper.get('[data-testid=dynamic-bounds]').text()).toContain('$400.00')
    expect(wrapper.get('[data-testid=dynamic-bounds]').text()).toContain('$600.00')
  })
  it.each([false, true])('shows the upstream reset outside details and follows refreshed data (compact=%s)', async compact => {
    const quota: DynamicSubscriptionQuota = { enabled: true, revision: 1, weight: 1, max_limit_usd: 600, cycle: 1, status: 'active', limit_usd: 600, used_usd: 180, reserved_usd: 0, remaining_usd: 420, started_at: '2026-09-01T01:00:00Z', updated_at: '', expected_reset_at: '2026-09-12T01:00:00Z' }
    const wrapper = mount(DynamicQuotaCard, { props: { quota, compact }, global: { plugins: [createI18n({ legacy: false, locale: 'zh', messages: { zh }, messageCompiler: message => () => String(message) })] } })
    const reset = wrapper.get('[data-testid="dynamic-reset"]')
    expect(reset.element.closest('details')).toBeNull()
    expect(reset.attributes('title')).toContain('跟随绑定上游')
    expect(reset.text()).toContain('下次上游重置（预计）')
    expect(reset.text()).not.toContain('待确认')
    expect(wrapper.findAll('time')).toHaveLength(1)
    expect(reset.get('time').attributes('datetime')).toBe(quota.expected_reset_at)
    const previousText = reset.get('time').text()
    await wrapper.setProps({ quota: { ...quota, confirmed_at: '2026-09-08T02:30:00Z', expected_reset_at: '2026-09-15T02:30:00Z' } })
    expect(reset.get('time').attributes('datetime')).toBe('2026-09-15T02:30:00Z')
    expect(reset.get('time').text()).not.toBe(previousText)
    expect(wrapper.text()).toContain('$180.00') // A changed forecast does not clear usage.
    for (const expected_reset_at of [undefined, '', 'invalid-date']) {
      await wrapper.setProps({ quota: { ...quota, expected_reset_at } })
      expect(reset.find('time').exists()).toBe(false)
      expect(reset.text()).toContain('等待上游同步')
      expect(reset.text()).not.toContain('Invalid Date')
    }
  })

  it('separates adjustment from reset and never fabricates a local seven-day countdown', async () => {
    const quota: DynamicSubscriptionQuota = { enabled: true, revision: 1, weight: 1, max_limit_usd: 700, floor_limit_usd: 100, next_adjustment_percent: 40, cycle: 4, status: 'confirming', limit_usd: 100, used_usd: 20, reserved_usd: 5, remaining_usd: 75, started_at: '2026-09-01T01:00:00Z', updated_at: '', synced_at: '2026-09-09T01:00:00Z', expected_reset_at: '2026-09-12T01:00:00Z' }
    const wrapper = mount(DynamicQuotaCard, { props: { quota }, global: { plugins: [createI18n({ legacy: false, locale: 'zh', messages: { zh }, messageCompiler: message => () => String(message) })] } })
    expect(wrapper.text()).not.toContain('确认上游重置中') // Status lives in the shared subscription header.
    expect(wrapper.text()).toContain('尚未确认重置')
    expect(wrapper.text()).toContain('下次上游重置（预计）')
    expect(wrapper.text()).toContain('$75.00')
    expect(wrapper.text()).not.toContain('09-08') // not started_at + 7 days
    await wrapper.setProps({ quota: { ...quota, limit_usd: 150, remaining_usd: 125 } })
    expect(wrapper.text()).toContain('$20.00')
    expect(wrapper.text()).toContain('$125.00')
    expect(wrapper.text()).toContain('#4')
    expect(wrapper.get('[role=progressbar]').attributes('aria-valuenow')).toBe('13')
    expect(wrapper.text()).not.toContain('等待本周期有效预计总费用')
    await wrapper.setProps({ quota: { ...quota, cycle: 1, status: 'learning', max_limit_usd: 200, limit_usd: 200, used_usd: 0, remaining_usd: 200 } })
    expect(wrapper.text()).toContain('等待本周期有效预计总费用')
    expect(wrapper.text()).not.toContain('80%')
    expect(wrapper.text()).toContain('分配上限')
    expect(wrapper.text()).toContain('下调保护')
    expect(wrapper.find('details').attributes('open')).toBeUndefined()
    expect(wrapper.get('[data-testid=dynamic-allocated] dt > span').text()).toBe('当前动态额度')
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
    expect(wrapper.text()).toContain('最近调额')
    const stage = wrapper.get('[data-testid=dynamic-allocation-stage]')
    expect(stage.element.closest('details')).toBeNull()
    expect(stage.get('[data-testid=dynamic-allocated] time').attributes('datetime')).toBe('2026-09-08T01:00:00Z')
    expect(stage.get('[data-testid=dynamic-allocated]').text()).toContain('下次调额')
    expect(stage.get('[data-testid=dynamic-next-adjustment]').find('time').exists()).toBe(false) // Usage milestones have no known date.
    expect(stage.get('time').attributes('datetime')).not.toBe(quota.synced_at) // Data sync is not allocation time.
    expect(wrapper.text()).toContain('最近数据同步')
    expect(wrapper.text()).toContain('本周期起点')
    await wrapper.setProps({ quota: { ...quota, status: 'active', used_usd: 120, reserved_usd: 0, remaining_usd: 0 } })
    expect(wrapper.text()).not.toContain('当前额度已用尽')
    expect(wrapper.text()).toContain('$120.00')
    expect(wrapper.get('[role=progressbar]').attributes('aria-valuenow')).toBe('100')
    await wrapper.setProps({ compact: true, quota: { ...quota, status: 'active', remaining_usd: 0 } })
    expect(wrapper.text()).not.toContain('额度正在处理中')
    expect(wrapper.find('details').exists()).toBe(false)
    expect(wrapper.get('[data-testid=dynamic-allocation-stage]').text()).toContain('下次调额')
  })
})
