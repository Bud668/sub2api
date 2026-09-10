import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, shallowMount, type VueWrapper } from '@vue/test-utils'
import ChannelStatusV1View from '../ChannelStatusV1View.vue'
import MonitorHero from '@/components/user/monitor/MonitorHero.vue'

const { list, showError } = vi.hoisted(() => ({ list: vi.fn(), showError: vi.fn() }))
vi.mock('@/api/channelMonitor', () => ({ list, status: vi.fn() }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError, cachedPublicSettings: { channel_monitor_enabled: true } }) }))
vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key }),
}))

describe('channel usage refresh', () => {
  let wrapper: VueWrapper
  beforeEach(() => {
    vi.useFakeTimers()
    localStorage.clear()
    list.mockReset().mockResolvedValue({ items: [] })
    showError.mockReset()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
  })
  afterEach(() => {
    wrapper?.unmount()
    vi.useRealTimers()
    vi.restoreAllMocks()
  })
  async function mountPage() {
    wrapper = shallowMount(ChannelStatusV1View, {
      global: { stubs: { AppLayout: { template: '<div><slot /></div>' } } },
    })
    await flushPromises()
    return wrapper.findComponent(MonitorHero)
  }

  it('defaults to 30 seconds despite the old 60-second preference, and pauses while hidden', async () => {
    localStorage.setItem('channel-status-auto-refresh', JSON.stringify({ enabled: true, interval_seconds: 60 }))
    const hero = await mountPage()
    expect(hero.props('intervalSeconds')).toBe(30)
    expect(list).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(31_000)
    expect(list).toHaveBeenCalledTimes(2)
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(true)
    await vi.advanceTimersByTimeAsync(62_000)
    expect(list).toHaveBeenCalledTimes(2)
    wrapper.unmount()
    await vi.advanceTimersByTimeAsync(62_000)
    expect(list).toHaveBeenCalledTimes(2)
  })

  it('preserves the chosen interval after successful and failed refreshes', async () => {
    localStorage.setItem('channel-status-usage-auto-refresh', JSON.stringify({ enabled: true, interval_seconds: 120 }))
    const hero = await mountPage()
    expect(hero.props('intervalSeconds')).toBe(120)
    hero.vm.$emit('refresh')
    await flushPromises()
    expect(hero.props('autoRefresh')?.countdown.value).toBe(120)
    list.mockRejectedValueOnce(new Error('unavailable'))
    hero.vm.$emit('refresh')
    await flushPromises()
    expect(showError).toHaveBeenCalledTimes(1)
    expect(hero.props('autoRefresh')?.countdown.value).toBe(120)
    await vi.advanceTimersByTimeAsync(61_000)
    expect(list).toHaveBeenCalledTimes(3)
  })
})
