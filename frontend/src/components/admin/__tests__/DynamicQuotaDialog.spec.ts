import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import zh from '@/i18n/locales/zh'
import type { DynamicQuotaAdminStatus, UserSubscription } from '@/types'
import DynamicQuotaDialog from '../DynamicQuotaDialog.vue'

const { get, save } = vi.hoisted(() => ({ get: vi.fn(), save: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { subscriptions: { getDynamicQuota: get, saveDynamicQuota: save } } }))
const initial = (): DynamicQuotaAdminStatus => ({
  policy: { enabled: false, revision: 0, weight: 1, max_limit_usd: 700, floor_limit_usd: null, cycle: 0, status: 'disabled', used_usd: 0, limit_usd: 0, remaining_usd: 0, reserved_usd: 0, started_at: '', updated_at: '' },
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
    expect(wrapper.find('#dynamic-increase-threshold').exists()).toBe(false)
    expect((wrapper.get('#dynamic-floor').element as HTMLInputElement).value).toBe('')
    expect(wrapper.find('[data-testid=capacity-review]').exists()).toBe(false)
    expect(wrapper.get('[data-testid=dynamic-floor-help]').attributes('aria-label')).toBe('下调保护说明')
    expect(wrapper.find('#dynamic-usage-ceiling').exists()).toBe(false)
    expect(wrapper.text()).toContain('账号管理 → 编辑 → 5h / 7d 自动暂停')
    await wrapper.get('#dynamic-floor').setValue(200)
    await wrapper.get('input[type=checkbox]').setValue(true)
    const response = initial()
    response.policy = { ...response.policy, enabled: true, account_id: 4, revision: 1, floor_limit_usd: 200, cycle: 1, status: 'learning', used_usd: 20, limit_usd: 500, remaining_usd: 480 }
    let resolve!: (value: DynamicQuotaAdminStatus) => void
    save.mockReturnValue(new Promise<DynamicQuotaAdminStatus>(done => { resolve = done }))
    await wrapper.get('form').trigger('submit')
    expect(save).toHaveBeenCalledWith(11, { enabled: true, account_id: 4, revision: 0, weight: 1, max_limit_usd: 700, floor_limit_usd: 200 })
    expect(wrapper.get('[data-testid=dynamic-save]').attributes('disabled')).toBeDefined()
    expect(wrapper.text()).toContain(zh.common.saving)
    resolve(response); await flushPromises()
    expect(wrapper.emitted('close')).toBeUndefined()
    expect(wrapper.emitted('saved')).toHaveLength(1)
    expect(wrapper.text()).toContain('保存成功')
    expect(wrapper.text()).toContain('$480.00')
    expect(wrapper.get('#dynamic-source').element.tagName).toBe('OUTPUT')
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
    await wrapper.get('#dynamic-floor').setValue(200)
    save.mockRejectedValue({ status: 409, code: 409, reason: 'DYNAMIC_QUOTA_CHANGED' })
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(wrapper.get('[role=alert]').text()).toContain('配置已被其他操作修改')
    expect((wrapper.get('#dynamic-cap').element as HTMLInputElement).value).toBe('450')
    expect(save.mock.calls[0][1].account_id).toBe(5)
    expect(wrapper.emitted('close')).toBeUndefined()
    expect(wrapper.emitted('saved')).toBeUndefined()
  })

  it.each([
    ['INVALID_DYNAMIC_QUOTA_PROTECTION', zh.dynamicQuota.floorInvalid],
    ['DYNAMIC_QUOTA_BINDING_CONFLICT', zh.dynamicQuota.bindingError],
    ['DYNAMIC_QUOTA_UNAVAILABLE', zh.dynamicQuota.unavailable],
    ['UNRECOGNIZED_ERROR', zh.dynamicQuota.failed]
  ])('shows the normalized API error %s and preserves the unsaved form', async (reason, message) => {
    const wrapper = mountDialog(); await flushPromises()
    await wrapper.get('#dynamic-source').setValue('5')
    await wrapper.get('#dynamic-cap').setValue(600)
    await wrapper.get('#dynamic-floor').setValue(200)
    await wrapper.get('[data-testid=dynamic-enable]').setValue(true)
    save.mockRejectedValue({ status: 409, code: 409, reason })
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(wrapper.get('[role=alert]').text()).toBe(message)
    expect((wrapper.get('#dynamic-cap').element as HTMLInputElement).value).toBe('600')
    expect((wrapper.get('[data-testid=dynamic-enable]').element as HTMLInputElement).checked).toBe(true)
    expect(wrapper.emitted('saved')).toBeUndefined()
    expect(wrapper.emitted('close')).toBeUndefined()
  })

  it('keeps accounting independent and refreshes without discarding edits', async () => {
    get.mockResolvedValue({ ...initial(), subscription_pending_requests: 3, subscription_uncertain_requests: 10, subscription_reserved_standard_usd: 29.60372 })
    const wrapper = mountDialog(); await flushPromises()
    expect(wrapper.text()).toContain('不阻止合法配置保存')
    expect(wrapper.find('[data-testid=subscription-requests]').exists()).toBe(false)
    expect(wrapper.find('[data-testid=pool-requests]').exists()).toBe(false)
    await wrapper.get('#dynamic-source').setValue('5')
    await wrapper.get('#dynamic-cap').setValue(600)
    get.mockResolvedValue({ ...initial(), subscription_pending_requests: 0, subscription_uncertain_requests: 10, subscription_reserved_standard_usd: 21.681536 })
    await wrapper.get('[data-testid=dynamic-refresh]').trigger('click'); await flushPromises()
    expect((wrapper.get('#dynamic-cap').element as HTMLInputElement).value).toBe('600')
    expect((wrapper.get('#dynamic-source').element as HTMLSelectElement).value).toBe('5')
    expect(save).not.toHaveBeenCalled()
  })


  it('rejects missing, zero and above-cap protection without submitting', async () => {
    const wrapper = mountDialog(); await flushPromises()
    await wrapper.get('#dynamic-source').setValue('4')
    for (const value of ['', 0, 701]) {
      await wrapper.get('#dynamic-floor').setValue(value)
      await wrapper.get('form').trigger('submit'); await flushPromises()
      expect(wrapper.get('#dynamic-floor-error').text()).toContain('不超过分配上限')
      expect(save).not.toHaveBeenCalled()
    }
  })

  it('keeps the requested switch on while source activation is pending', async () => {
    const result = initial()
    result.policy = { ...result.policy, account_id: 4, revision: 1, requested_enabled: true, activation_pending: true, floor_limit_usd: 200 }
    get.mockResolvedValue(result)
    const wrapper = mountDialog(); await flushPromises()
    expect((wrapper.get('[data-testid=dynamic-enable]').element as HTMLInputElement).checked).toBe(true)
    expect(wrapper.text()).toContain('生效前继续使用原规则')
    save.mockResolvedValue({ ...result, policy: { ...result.policy, revision: 2 } })
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(wrapper.emitted('saved')).toHaveLength(1)
    expect(wrapper.emitted('close')).toBeUndefined()
  })

  it.each([{ code: 'ECONNABORTED' }, { status: 500, code: 'INTERNAL_SERVER_ERROR' }])('checks an ambiguous save %j by reading once, never blindly resubmits', async (error) => {
    const wrapper = mountDialog(); await flushPromises()
    await wrapper.get('#dynamic-source').setValue('4')
    await wrapper.get('#dynamic-floor').setValue(200)
    save.mockRejectedValue(error)
    const result = initial()
    result.policy = { ...result.policy, account_id: 4, revision: 1, floor_limit_usd: 200 }
    get.mockResolvedValue(result)
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(save).toHaveBeenCalledTimes(1)
    expect(wrapper.text()).toContain('保存成功')
    expect(wrapper.emitted('close')).toBeUndefined()
  })
})
