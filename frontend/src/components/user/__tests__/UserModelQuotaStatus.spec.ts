import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import UserModelQuotaStatus from '../UserModelQuotaStatus.vue'

const mocks = vi.hoisted(() => ({ get: vi.fn() }))
vi.mock('@/api/modelPolicy', async () => ({
  ...await vi.importActual<typeof import('@/api/modelPolicy')>('@/api/modelPolicy'), getMyModelPolicy: mocks.get
}))
vi.mock('vue-i18n', async () => ({ ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'), useI18n: () => ({ t: (key: string) => key }) }))

describe('user model quota status', () => {
  it('shows explicit denied models and daily reset rules, not a deny-all message', async () => {
    mocks.get.mockResolvedValue({ enabled: true, rules: [
      { model: 'gpt-5.6-sol', mode: 'deny' },
      { model: 'gpt-5.6-luna', mode: 'limited', request_limit: 10, window_mode: 'daily' }
    ], windows: [] })
    const w = mount(UserModelQuotaStatus)
    await flushPromises()
    expect(w.text()).toContain('gpt-5.6-sol')
    expect(w.text()).toContain('admin.users.modelPolicy.deny')
    expect(w.text()).toContain('admin.users.modelPolicy.dailyReset')
    expect(w.text()).not.toContain('admin.users.modelPolicy.emptyHint')
  })
  it('shows no extra restrictions for an empty policy', async () => {
    mocks.get.mockResolvedValue({ enabled: true, rules: [], windows: [] })
    const w = mount(UserModelQuotaStatus)
    await flushPromises()
    expect(w.text()).toContain('admin.users.modelPolicy.emptyHint')
  })
})
