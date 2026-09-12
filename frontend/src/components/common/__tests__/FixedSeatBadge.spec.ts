import { describe, expect, it } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import zh from '@/i18n/locales/zh'
import en from '@/i18n/locales/en'
import type { DynamicSubscriptionQuota } from '@/types'
import FixedSeatBadge from '../FixedSeatBadge.vue'
import DynamicQuotaCard from '../DynamicQuotaCard.vue'

const quota: DynamicSubscriptionQuota = { enabled: true, revision: 1, weight: 1, max_limit_usd: 600, fixed_slots: 4, source_fixed_slots: 8, cycle: 1, status: 'learning', limit_usd: 60, used_usd: 5, reserved_usd: 1, remaining_usd: 54, next_adjustment_percent: 2, started_at: '', updated_at: '' }
const i18n = (locale: string) => createI18n({ legacy: false, locale, messages: { zh, en }, messageCompiler: message => ctx => String(message).replace(/\{(\w+)\}/g, (_, key) => String(ctx.named(key))) })

describe('fixed seat display', () => {
  it.each(['zh', 'en'])('shows effective group count with accessible shared-source explanation (%s)', async locale => {
    const wrapper = mount(FixedSeatBadge, { props: { quota }, global: { plugins: [i18n(locale)] } })
    const button = wrapper.get('button')
    expect(button.text()).toContain('· 4')
    expect(button.attributes('aria-label')).toContain('8')
    expect(button.classes()).toEqual(expect.arrayContaining(['font-semibold', 'text-blue-700', 'dark:text-blue-300']))
    await button.trigger('click'); await flushPromises()
    expect(button.attributes('aria-expanded')).toBe('true')
    await wrapper.setProps({ quota: { ...quota, fixed_slots: 6 } })
    expect(button.text()).toContain('· 6')
    await wrapper.setProps({ quota: { ...quota, enabled: false } })
    expect(wrapper.find('button').exists()).toBe(false)
    wrapper.unmount()
  })

  it('removes manual floor and explains finite startup with the two-point milestone', () => {
    const wrapper = mount(DynamicQuotaCard, { props: { quota: { ...quota, floor_limit_usd: 400 } }, global: { plugins: [i18n('zh')] } })
    expect(wrapper.get('[data-testid=dynamic-next-adjustment]').text()).toContain('2%')
    expect(wrapper.get('[data-testid=dynamic-bounds]').text()).not.toContain('下调保护')
    expect(wrapper.text()).toContain('不放满分配上限')
    expect(wrapper.text()).toContain('每 2 个百分点')
    expect(wrapper.text()).toContain('每 5 个百分点')
    expect(wrapper.text()).toContain('每 10 个百分点')
  })
})
