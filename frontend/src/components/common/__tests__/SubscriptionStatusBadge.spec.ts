import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import zh from '@/i18n/locales/zh'
import type { DynamicSubscriptionQuota, UserSubscription } from '@/types'
import SubscriptionStatusBadge from '../SubscriptionStatusBadge.vue'

describe('combined subscription status', () => {
  it('prioritizes lifecycle and blocked quota, preserves native/debug semantics, and exposes details', async () => {
    const quota: DynamicSubscriptionQuota = { enabled: true, revision: 1, weight: 1, max_limit_usd: 600, cycle: 1, status: 'active', limit_usd: 600, used_usd: 100, reserved_usd: 0, remaining_usd: 500, started_at: '', updated_at: '' }
    const subscription = { status: 'active' as const, dynamic_quota: quota }
    const wrapper = mount(SubscriptionStatusBadge, {
      attachTo: document.body, props: { subscription },
      global: { plugins: [createI18n({ legacy: false, locale: 'zh', messages: { zh }, messageCompiler: message => () => String(message) })] }
    })
    const button = wrapper.get('button')
    for (const [patch, state, label] of [
      [{}, 'active', '生效中'],
      [{ status: 'learning' }, 'learning', '学习中 · 可用'],
      [{ growth_frozen: true }, 'growth_frozen', '调额暂缓 · 可用'],
      [{ growth_frozen: true, remaining_usd: 0 }, 'exhausted', '额度已用尽'],
      [{ status: 'learning', remaining_usd: -1, reserved_usd: 20 }, 'reserved', '额度正在处理中'],
      [{ status: 'confirming' }, 'confirming', '确认上游重置中'],
      [{ status: 'upstream_reserve' }, 'upstream_reserve', '上游暂不可用'],
      [{ remaining_usd: Number.NaN }, 'quota_unavailable', '额度数据待同步'],
      [{ remaining_usd: Number.POSITIVE_INFINITY }, 'quota_unavailable', '额度数据待同步'],
      [{ status: 'unexpected' }, 'quota_unavailable', '额度数据待同步'],
      [{ enabled: false, status: 'quota_unavailable' }, 'active', '生效中'],
      [{ enabled: false, requested_enabled: true, activation_pending: true }, 'activation_pending', '等待来源数据生效']
    ] as [Partial<DynamicSubscriptionQuota>, string, string][]) {
      await wrapper.setProps({ subscription: { ...subscription, dynamic_quota: { ...quota, ...patch } } })
      expect(wrapper.attributes('data-status')).toBe(state)
      expect(button.text()).toBe(label)
    }
    expect(button.attributes('title')).toContain('生效前继续使用原规则')
    for (const [status, label] of [['expired', '已过期'], ['revoked', '已撤销'], ['suspended', '已暂停']] as const) {
      await wrapper.setProps({ subscription: { ...subscription, status } })
      expect(button.text()).toBe(label)
      expect(button.attributes('title')).toContain('订阅当前不可用')
      expect(button.attributes('title')).not.toContain('当前动态额度可用')
    }
    await wrapper.setProps({ subscription: { ...subscription, status: 'invalid' as UserSubscription['status'] } })
    expect(button.text()).toBe('状态待核对')
    await wrapper.setProps({ subscription: { ...subscription, admin_debug: true, dynamic_quota: { ...quota, remaining_usd: 0 } } })
    expect(button.text()).toBe('生效中')
    expect(button.attributes('title')).toContain('不参与动态分配')
    expect(button.attributes('title')).toContain('独立周额度')
    const debug = { weekly_limit_usd: 50, revision: 1, follow_reset: true, reset_pending: false }
    await wrapper.setProps({ subscription: { ...subscription, admin_debug: true, admin_debug_quota: debug, weekly_usage_usd: 50 } })
    expect(button.text()).toBe('额度已用尽')
    await wrapper.setProps({ subscription: { ...subscription, admin_debug: true, admin_debug_quota: { ...debug, reset_pending: true } } })
    expect(button.text()).toBe('等待旧请求结算')
    await wrapper.setProps({ subscription: { status: 'active', dynamic_quota: null } })
    expect(button.text()).toBe('生效中')
    expect(button.attributes('title')).toContain('原分组限额')
    await wrapper.setProps({ subscription: { ...subscription, dynamic_quota: { ...quota, status: 'learning' } } })
    expect(button.attributes('aria-expanded')).toBe('false')
    await button.trigger('click')
    expect(button.attributes('aria-expanded')).toBe('true')
    const tip = document.getElementById(button.attributes('aria-describedby'))!
    expect(tip.textContent).toContain('订阅状态: 生效中')
    expect(tip.textContent).toContain('动态额度: 学习期')
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }))
    await wrapper.vm.$nextTick()
    expect(button.attributes('aria-expanded')).toBe('false')
    expect(quota.used_usd).toBe(100)
    wrapper.unmount()
  })
})
