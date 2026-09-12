import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import zh from '@/i18n/locales/zh'
import type { Group } from '@/types'
import AdminDebugUsage from '../AdminDebugUsage.vue'

describe('administrator debug weekly summary', () => {
  it('shows weekly usage without monthly totals, dynamic allocation or invented available amounts', async () => {
    const group = { weekly_limit_usd: 600, monthly_limit_usd: 3200 } as Group
    const subscription = { weekly_usage_usd: 0.05, monthly_usage_usd: 94.70, group, admin_debug_quota: { weekly_limit_usd: 600, revision: 1, follow_reset: false, reset_pending: false } }
    const wrapper = mount(AdminDebugUsage, {
      props: { subscription }, slots: { reset: '6 天 21 小时后重置' },
      global: { plugins: [createI18n({ legacy: false, locale: 'zh', messages: { zh }, messageCompiler: message => () => String(message) })] }
    })
    expect(wrapper.text()).toContain('本周已用')
    expect(wrapper.text()).toContain('0.05')
    expect(wrapper.text()).toContain('600.00')
    expect(wrapper.text()).toContain('6 天 21 小时后重置')
    expect(wrapper.text()).toContain('独立周额度')
    for (const value of ['每月', '94.70', '3,200', '下次调额', '剩余可用']) expect(wrapper.text()).not.toContain(value)
    await wrapper.setProps({ subscription: { ...subscription, weekly_usage_usd: 700 } })
    expect(wrapper.get('[role=progressbar]').attributes('aria-valuenow')).toBe('100')
    expect(wrapper.get('[role=progressbar] > div').classes()).toContain('bg-red-500')
    await wrapper.setProps({ subscription: { ...subscription, weekly_usage_usd: -1 } })
    expect(wrapper.get('[role=progressbar]').attributes('aria-valuenow')).toBe('0')
    await wrapper.setProps({ subscription: { ...subscription, admin_debug_quota: { ...subscription.admin_debug_quota, weekly_limit_usd: 0 } } })
    expect(wrapper.text()).toContain('无限制')
    expect(wrapper.find('[role=progressbar]').exists()).toBe(false)
    expect(subscription.monthly_usage_usd).toBe(94.70)
    expect(group.monthly_limit_usd).toBe(3200)
    await wrapper.setProps({ subscription: { ...subscription, group: { ...group, weekly_limit_usd: 9999 }, admin_debug_quota: { ...subscription.admin_debug_quota, follow_reset: true, reset_pending: true } } })
    expect(wrapper.text()).toContain('600.00')
    expect(wrapper.text()).not.toContain('9,999')
    expect(wrapper.text()).toContain('等待旧请求结算后同步重置')
    expect(wrapper.text()).not.toContain('6 天 21 小时后重置')
  })
})
