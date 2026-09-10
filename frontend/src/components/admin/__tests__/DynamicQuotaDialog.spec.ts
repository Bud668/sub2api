import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import zh from '@/i18n/locales/zh'
import type { DynamicQuotaAdminStatus, UserSubscription } from '@/types'
import DynamicQuotaDialog from '../DynamicQuotaDialog.vue'

const { get, save, approve } = vi.hoisted(() => ({ get: vi.fn(), save: vi.fn(), approve: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { subscriptions: { getDynamicQuota: get, saveDynamicQuota: save, approveDynamicCapacity: approve } } }))
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
    save.mockRejectedValue({ status: 409, code: 409, reason: 'DYNAMIC_QUOTA_CHANGED' })
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(wrapper.get('[role=alert]').text()).toContain('配置已被其他操作修改')
    expect((wrapper.get('#dynamic-cap').element as HTMLInputElement).value).toBe('450')
    expect(save.mock.calls[0][1].account_id).toBe(5)
    expect(wrapper.emitted('close')).toBeUndefined()
    expect(wrapper.emitted('saved')).toBeUndefined()
  })

  it.each([
    ['DYNAMIC_QUOTA_REQUESTS_PENDING', zh.dynamicQuota.pending],
    ['DYNAMIC_QUOTA_BINDING_CONFLICT', zh.dynamicQuota.bindingError],
    ['DYNAMIC_QUOTA_UNAVAILABLE', zh.dynamicQuota.unavailable],
    ['UNRECOGNIZED_ERROR', zh.dynamicQuota.failed]
  ])('shows the normalized API error %s and preserves the unsaved form', async (reason, message) => {
    const wrapper = mountDialog(); await flushPromises()
    await wrapper.get('#dynamic-source').setValue('5')
    await wrapper.get('#dynamic-cap').setValue(600)
    await wrapper.get('[data-testid=dynamic-enable]').setValue(true)
    save.mockRejectedValue({ status: 409, code: 409, reason })
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(wrapper.get('[role=alert]').text()).toBe(message)
    expect((wrapper.get('#dynamic-cap').element as HTMLInputElement).value).toBe('600')
    expect((wrapper.get('[data-testid=dynamic-enable]').element as HTMLInputElement).checked).toBe(true)
    expect(wrapper.emitted('saved')).toBeUndefined()
    expect(wrapper.emitted('close')).toBeUndefined()
  })

  it('shows subscription holds before first activation and refreshes without discarding edits', async () => {
    get.mockResolvedValue({ ...initial(), subscription_pending_requests: 3, subscription_uncertain_requests: 10, subscription_reserved_standard_usd: 29.60372 })
    const wrapper = mountDialog(); await flushPromises()
    expect(wrapper.get('[data-testid=subscription-requests]').text()).toContain('3 / 10')
    expect(wrapper.get('[data-testid=subscription-requests]').text()).toContain('$29.60')
    expect(wrapper.find('[data-testid=pool-requests]').exists()).toBe(false)
    await wrapper.get('#dynamic-source').setValue('5')
    await wrapper.get('#dynamic-cap').setValue(600)
    get.mockResolvedValue({ ...initial(), subscription_pending_requests: 0, subscription_uncertain_requests: 10, subscription_reserved_standard_usd: 21.681536 })
    await wrapper.get('[data-testid=subscription-requests] button').trigger('click'); await flushPromises()
    expect(wrapper.get('[data-testid=subscription-requests]').text()).toContain('0 / 10')
    expect((wrapper.get('#dynamic-cap').element as HTMLInputElement).value).toBe('600')
    expect((wrapper.get('#dynamic-source').element as HTMLSelectElement).value).toBe('5')
    expect(save).not.toHaveBeenCalled()
  })

  const review = (): DynamicQuotaAdminStatus => {
    const result = initial()
    result.policy = { ...result.policy, enabled: true, revision: 1, account_id: 4, status: 'active', growth_frozen: true,
      capacity_estimate_usd: 2000, capacity_approval_ready: true,
      capacity_review: { id: 'review-1', proposed_usd: 9000, observations: 3, last_observed_at: '', manual_required: true, anomaly_checks: 3 } }
    return result
  }

  it('requires a separate acknowledged approval and keeps the result open for review', async () => {
    get.mockResolvedValue(review())
    const wrapper = mountDialog(); await flushPromises()
    expect(wrapper.text()).toContain('$2000.00')
    expect(wrapper.text()).toContain('$9000.00')
    expect(wrapper.get('[data-testid=capacity-approve]').attributes('disabled')).toBeDefined()
    await wrapper.get('[data-testid=capacity-acknowledge]').setValue(true)
    const accepted = review()
    delete accepted.policy.capacity_review
    accepted.policy.capacity_approval_ready = false
    accepted.policy.growth_frozen = false
    accepted.policy.status = 'active'
    accepted.policy.capacity_estimate_usd = 9000
    approve.mockResolvedValue(accepted)
    await wrapper.get('[data-testid=capacity-approve]').trigger('click'); await flushPromises()
    expect(approve).toHaveBeenCalledTimes(1)
    expect(approve).toHaveBeenCalledWith(11, 'review-1')
    expect(save).not.toHaveBeenCalled()
    expect(wrapper.emitted('saved')).toHaveLength(1)
    expect(wrapper.emitted('close')).toBeUndefined()
    expect(wrapper.find('[data-testid=capacity-review]').exists()).toBe(false)
    expect(wrapper.text()).toContain('容量已确认')
  })

  it('preserves edits on refresh and does not let save or a dirty form approve capacity', async () => {
    get.mockResolvedValue(review())
    const wrapper = mountDialog(); await flushPromises()
    await wrapper.get('[data-testid=capacity-acknowledge]').setValue(true)
    await wrapper.get('#dynamic-cap').setValue(500)
    expect(wrapper.get('[data-testid=capacity-approve]').attributes('disabled')).toBeDefined()
    const changed = review()
    changed.policy.capacity_review!.id = 'review-2'
    changed.policy.capacity_approval_ready = false
    get.mockResolvedValue(changed)
    await wrapper.get('[data-testid=capacity-review] button').trigger('click'); await flushPromises()
    expect((wrapper.get('#dynamic-cap').element as HTMLInputElement).value).toBe('500')
    expect((wrapper.get('[data-testid=capacity-acknowledge]').element as HTMLInputElement).checked).toBe(false)
    expect(wrapper.get('[data-testid=capacity-approve]').attributes('disabled')).toBeDefined()
    save.mockResolvedValue({ ...changed, policy: { ...changed.policy, revision: 2, max_limit_usd: 500 } })
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(save.mock.calls[0][1]).not.toHaveProperty('review_id')
    expect(approve).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid=capacity-review]').exists()).toBe(true)
  })

  it('requires renewed acknowledgment after a stale approval is rejected', async () => {
    get.mockResolvedValue(review())
    approve.mockRejectedValue({ status: 409, code: 409, reason: 'DYNAMIC_QUOTA_CHANGED' })
    const wrapper = mountDialog(); await flushPromises()
    await wrapper.get('[data-testid=capacity-acknowledge]').setValue(true)
    await wrapper.get('[data-testid=capacity-approve]').trigger('click'); await flushPromises()
    expect(wrapper.get('[role=alert]').text()).toContain('配置已被其他操作修改')
    expect(wrapper.get('[data-testid=capacity-approve]').attributes('disabled')).toBeDefined()
    expect(wrapper.emitted('saved')).toBeUndefined()
    expect(wrapper.emitted('close')).toBeUndefined()
  })
})
