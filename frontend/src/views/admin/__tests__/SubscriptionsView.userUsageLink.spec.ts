import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'

import SubscriptionsView from '../SubscriptionsView.vue'

const { listSubscriptions, getAllGroups, enableAdminDebug } = vi.hoisted(() => ({
  listSubscriptions: vi.fn(),
  getAllGroups: vi.fn(),
  enableAdminDebug: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    subscriptions: { list: listSubscriptions, enableAdminDebug },
    groups: { getAll: getAllGroups }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn()
  })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: { id?: number }) =>
        key === 'admin.redeem.userPrefix' ? `User #${params?.id}` : key
    })
  }
})

const DataTableStub = {
  props: ['data'],
  template: `
    <div>
      <div v-for="row in data" :key="row.id">
        <slot name="cell-user" :row="row" />
        <slot name="cell-expires_at" :row="row" :value="row.expires_at" />
        <div data-testid="card-actions"><slot name="cell-actions" :row="row" /></div>
      </div>
    </div>
  `
}

const RouterLinkStub = defineComponent({
  name: 'RouterLink',
  props: { to: { type: Object, required: true } },
  template: '<a :href="`${to.path}?user_id=${to.query.user_id}`"><slot /></a>'
})

describe('admin subscription user usage link', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    localStorage.clear()
    listSubscriptions.mockResolvedValue({
      items: [{
        id: 9,
        user_id: 42,
        group_id: 3,
        status: 'active',
        starts_at: '2026-01-01T00:00:00Z',
        expires_at: null,
        daily_usage_usd: 0,
        weekly_usage_usd: 0,
        monthly_usage_usd: 0,
        daily_window_start: null,
        weekly_window_start: null,
        monthly_window_start: null,
        created_at: '2026-01-01T00:00:00Z',
        updated_at: '2026-01-01T00:00:00Z',
        user: { email: 'reader@example.com', username: 'Reader' }
      }],
      total: 1,
      pages: 1
    })
    getAllGroups.mockResolvedValue([])
  })

  const mountView = () => mount(SubscriptionsView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: { template: '<div><slot name="table" /></div>' },
        DataTable: DataTableStub,
        RouterLink: RouterLinkStub,
        Pagination: true,
        BaseDialog: true,
        ConfirmDialog: true,
        EmptyState: true,
        Select: true,
        GroupBadge: true,
        GroupOptionItem: true,
        Icon: true,
        Teleport: true
      }
    }
  })

  it('renders the user email as a link to that user filtered usage records', async () => {
    const wrapper = mountView()

    await flushPromises()

    const link = wrapper.getComponent(RouterLinkStub)
    expect(link.text()).toBe('reader@example.com')
    expect(link.props('to')).toEqual({ path: '/admin/usage', query: { user_id: 42 } })
  })

  it('uses the user ID label for the usage link when username mode has no username', async () => {
    localStorage.setItem('subscription-user-column-mode', 'username')
    listSubscriptions.mockResolvedValue({
      items: [{
        id: 9,
        user_id: 42,
        group_id: 3,
        status: 'active',
        starts_at: '2026-01-01T00:00:00Z',
        expires_at: null,
        daily_usage_usd: 0,
        weekly_usage_usd: 0,
        monthly_usage_usd: 0,
        daily_window_start: null,
        weekly_window_start: null,
        monthly_window_start: null,
        created_at: '2026-01-01T00:00:00Z',
        updated_at: '2026-01-01T00:00:00Z',
        user: { email: 'reader@example.com' }
      }],
      total: 1,
      pages: 1
    })

    const wrapper = mountView()
    await flushPromises()

    const link = wrapper.getComponent(RouterLinkStub)
    expect(link.text()).toBe('User #42')
    expect(link.props('to')).toEqual({ path: '/admin/usage', query: { user_id: 42 } })
  })

  it('keeps date adjustment visible and only offers debug conversion for administrators', async () => {
    for (const role of ['user', 'admin']) {
      const data = await listSubscriptions()
      data.items[0].user.role = role
      const wrapper = mountView()
      await flushPromises()
      expect(wrapper.get('[data-testid=subscription-expiry]').text()).toContain('admin.subscriptions.adjust')
      expect(wrapper.get('[data-testid=card-actions]').text()).not.toContain('admin.subscriptions.adjust')
      expect(wrapper.text()).not.toContain('admin.subscriptions.resetQuota')
      expect(wrapper.text()).not.toContain('dynamicQuota.groupSettings')
      expect(wrapper.find('#subscription-action-menu').exists()).toBe(false)
      await wrapper.get('[data-subscription-menu-trigger]').trigger('click')
      await flushPromises()
      const menu = wrapper.get('#subscription-action-menu')
      expect(menu.text()).toContain('admin.subscriptions.revoke')
      expect(menu.text().includes('dynamicQuota.enableAdminDebug')).toBe(role === 'admin')
      if (role === 'admin') {
        const button = menu.findAll('button').find(b => b.text() === 'dynamicQuota.enableAdminDebug')!
        await button.trigger('click')
        const dialog = wrapper.findAllComponents({ name: 'ConfirmDialog' }).find(d => d.props('title') === 'dynamicQuota.enableAdminDebug')!
        expect(dialog.props('show')).toBe(true)
        expect(enableAdminDebug).not.toHaveBeenCalled()
        dialog.vm.$emit('confirm')
        await flushPromises()
        expect(enableAdminDebug).toHaveBeenCalledTimes(1)
        expect(enableAdminDebug).toHaveBeenCalledWith(9)
      }
      wrapper.unmount()
    }
  })
})
