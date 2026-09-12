import { beforeEach, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import zh from '@/i18n/locales/zh'
import type { UserSubscription } from '@/types'
import AdminDebugQuotaDialog from '../AdminDebugQuotaDialog.vue'

const { get, save } = vi.hoisted(() => ({ get: vi.fn(), save: vi.fn() }))
vi.mock('@/api/admin/subscriptions', () => ({ default: { getById: get, saveAdminDebugQuota: save } }))
const sub = { id: 11, admin_debug: true, weekly_usage_usd: 30, user: { email: 'admin@example.invalid' }, admin_debug_quota: { weekly_limit_usd: 60, revision: 1, follow_reset: true, reset_pending: false } } as UserSubscription
const render = () => mount(AdminDebugQuotaDialog, {
  props: { subscription: sub },
  global: { plugins: [createI18n({ legacy: false, locale: 'zh', messages: { zh }, messageCompiler: message => () => String(message) })], stubs: { BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' } } }
})
beforeEach(() => { vi.clearAllMocks(); get.mockResolvedValue(sub) })
it('edits existing subscriptions, preserves usage and keeps the dialog open after saving', async () => {
  const wrapper = render(); await flushPromises()
  expect((wrapper.get('input').element as HTMLInputElement).value).toBe('60')
  await wrapper.get('input').setValue('120')
  save.mockResolvedValue({ ...sub, admin_debug_quota: { ...sub.admin_debug_quota, weekly_limit_usd: 120, revision: 2 } })
  await wrapper.get('form').trigger('submit'); await flushPromises()
  expect(save).toHaveBeenCalledWith(11, { weekly_limit_usd: 120, revision: 1 })
  expect(wrapper.get('[role=status]').text()).toContain('保存成功')
  expect(wrapper.emitted('close')).toBeUndefined()
  expect(wrapper.emitted('saved')).toHaveLength(1)
  expect(sub.weekly_usage_usd).toBe(30)
  await wrapper.get('input').setValue('100')
  expect(wrapper.find('[role=status]').exists()).toBe(false)
  save.mockRejectedValue({ status: 409 })
  await wrapper.get('form').trigger('submit'); await flushPromises()
  expect(save.mock.calls[1][1].revision).toBe(2)
  expect((wrapper.get('input').element as HTMLInputElement).value).toBe('100')
  expect(wrapper.get('[role=alert]').text()).toContain('输入已保留')
  expect(wrapper.emitted('saved')).toHaveLength(1)
  wrapper.unmount()
})
it('does not save without a loaded authoritative policy', async () => {
  get.mockRejectedValue(new Error('unavailable'))
  const wrapper = render(); await flushPromises()
  await wrapper.get('form').trigger('submit'); await flushPromises()
  expect(save).not.toHaveBeenCalled()
  expect(wrapper.get('input').attributes('disabled')).toBeDefined()
  expect(wrapper.get('[role=alert]').text()).toContain('读取失败')
  wrapper.unmount()
})
