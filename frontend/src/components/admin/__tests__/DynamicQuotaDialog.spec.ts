import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import zh from '@/i18n/locales/zh'
import type { DynamicQuotaAdminStatus, UserSubscription } from '@/types'
import DynamicQuotaDialog from '../DynamicQuotaDialog.vue'

const { get, save } = vi.hoisted(() => ({ get: vi.fn(), save: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { subscriptions: { getDynamicQuota: get, saveDynamicQuota: save } } }))
const initial = (): DynamicQuotaAdminStatus => ({
  policy: { enabled: false, revision: 0, weight: 1, max_limit_usd: 700, increase_threshold_usd: 10, cycle: 0, status: 'disabled', used_usd: 0, limit_usd: 0, remaining_usd: 0, reserved_usd: 0, started_at: '', updated_at: '' },
  sources: [{ id: 4, name: 'Test source' }, { id: 5, name: 'Other source' }]
})
const subscription = { id: 11, user_id: 1, user: { email: 'admin@example.test' }, group: { name: 'Test group' } } as UserSubscription
const mountDialog = () => mount(DynamicQuotaDialog, {
  props: { subscription },
  global: { plugins: [createI18n({ legacy: false, locale: 'zh', messages: { zh }, messageCompiler: message => () => String(message) })], stubs: { BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' }, Icon: true } }
})

describe('dynamic quota settings', () => {
  beforeEach(() => { vi.clearAllMocks(); get.mockResolvedValue(initial()) })

  it('defaults OFF; saves explicit source, keeps the dialog open and reloads the server revision', async () => {
    const wrapper = mountDialog(); await flushPromises()
    expect((wrapper.get('input[type=checkbox]').element as HTMLInputElement).checked).toBe(false)
    expect(wrapper.get('[data-testid=dynamic-save]').attributes('disabled')).toBeDefined()
    await wrapper.get('#dynamic-source').setValue('4')
    expect((wrapper.get('#dynamic-increase-threshold').element as HTMLSelectElement).value).toBe('10')
    expect(wrapper.find('#dynamic-usage-ceiling').exists()).toBe(false)
    expect(wrapper.text()).toContain('账号管理 → 编辑 → 5h / 7d 自动暂停')
    await wrapper.get('#dynamic-increase-threshold').setValue('5')
    await wrapper.get('input[type=checkbox]').setValue(true)
    const response = initial()
    response.policy = { ...response.policy, enabled: true, account_id: 4, revision: 1, increase_threshold_usd: 5, cycle: 1, status: 'learning', used_usd: 20, limit_usd: 500, remaining_usd: 480 }
    let resolve!: (value: DynamicQuotaAdminStatus) => void
    save.mockReturnValue(new Promise<DynamicQuotaAdminStatus>(done => { resolve = done }))
    await wrapper.get('form').trigger('submit')
    expect(save).toHaveBeenCalledWith(11, { enabled: true, account_id: 4, revision: 0, weight: 1, max_limit_usd: 700, increase_threshold_usd: 5 })
    expect(wrapper.get('[data-testid=dynamic-save]').attributes('disabled')).toBeDefined()
    expect(wrapper.text()).toContain(zh.common.saving)
    resolve(response); await flushPromises()
    expect(wrapper.emitted('close')).toBeUndefined()
    expect(wrapper.emitted('saved')).toHaveLength(1)
    expect(wrapper.text()).toContain('保存成功')
    expect(wrapper.text()).toContain('$480.00')
    expect(wrapper.get('#dynamic-source').attributes('disabled')).toBeDefined()
    await wrapper.get('#dynamic-cap').setValue(600)
    expect(wrapper.text()).not.toContain('保存成功')
    save.mockResolvedValue({ ...response, policy: { ...response.policy, revision: 2, max_limit_usd: 600 } })
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(save.mock.calls[1][1].revision).toBe(1)
    expect(save.mock.calls[1][1]).not.toHaveProperty('pool_settings')
  })

  it('keeps unsaved inputs and reports a stale-save conflict without closing', async () => {
    const wrapper = mountDialog(); await flushPromises()
    await wrapper.get('#dynamic-source').setValue('4')
    await wrapper.get('#dynamic-source').setValue('5')
    await wrapper.get('#dynamic-cap').setValue(450)
    save.mockRejectedValue({ response: { data: { reason: 'DYNAMIC_QUOTA_CHANGED' } } })
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(wrapper.get('[role=alert]').text()).toContain('配置已被其他操作修改')
    expect((wrapper.get('#dynamic-cap').element as HTMLInputElement).value).toBe('450')
    expect(save.mock.calls[0][1].account_id).toBe(5)
    expect(wrapper.emitted('close')).toBeUndefined()
    expect(wrapper.emitted('saved')).toBeUndefined()
  })
})
