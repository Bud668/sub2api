/**
 * Admin Subscriptions API endpoints
 * Handles user subscription management for administrators
 */

import { apiClient } from '../client'
import type {
  UserSubscription,
  SubscriptionProgress,
  AssignSubscriptionRequest,
  BulkAssignSubscriptionRequest,
  ExtendSubscriptionRequest,
  DynamicQuotaInput,
  DynamicQuotaAdminStatus,
  PaginatedResponse
} from '@/types'

export interface AbsorbedUsageFilters {
  user_id?: number
  group_id?: number
  status?: string
  platform?: string
}

export interface DynamicResetResult {
  account_id: number
  cycle: number
  status: string
  members: number
}
export interface AbsorbedUsageSummary {
  requests: number
  known_requests: number
  known_standard_usd: number
  unknown_requests: number
}
export interface AbsorbedUsageRecord {
  id: string
  user_id: number
  email: string
  subscription_id: number
  group_id: number
  group_name: string
  account_id: number
  account_name: string
  cycle: number
  model: string
  reason: string
  known_standard_usd: number | null
  reference_hold_usd: number
  started_at: string
  absorbed_at: string
  closed_at: string | null
  needs_review?: boolean
  can_charge?: boolean
  charge_usd?: number | null
}
export interface AbsorbedUsageReport {
  summary: AbsorbedUsageSummary
  items: AbsorbedUsageRecord[]
  page: number
  page_size: number
  pages: number
}
export async function getAbsorbedUsage(
  params: AbsorbedUsageFilters & { scope: 'current' | 'history'; category?: 'covered' | 'review'; summary_only?: boolean; page?: number; page_size?: number },
  signal?: AbortSignal
): Promise<AbsorbedUsageReport> {
  const { data } = await apiClient.get<AbsorbedUsageReport>('/admin/subscriptions/absorbed-usage', { params, signal })
  return data
}

export async function resolveDynamicAccounting(id: string, action: 'charge' | 'cover'): Promise<void> {
  await apiClient.post(`/admin/subscriptions/absorbed-usage/${encodeURIComponent(id)}/resolve`, { action })
}

/**
 * List all subscriptions with pagination
 * @param page - Page number (default: 1)
 * @param pageSize - Items per page (default: 20)
 * @param filters - Optional filters (status, user_id, group_id, sort_by, sort_order)
 * @returns Paginated list of subscriptions
 */
export async function list(
  page: number = 1,
  pageSize: number = 20,
  filters?: {
    status?: 'active' | 'expired' | 'revoked' | 'suspended'
    user_id?: number
    group_id?: number
    platform?: string
    sort_by?: string
    sort_order?: 'asc' | 'desc'
  },
  options?: {
    signal?: AbortSignal
  }
): Promise<PaginatedResponse<UserSubscription>> {
  const { data } = await apiClient.get<PaginatedResponse<UserSubscription>>(
    '/admin/subscriptions',
    {
      params: {
        page,
        page_size: pageSize,
        ...filters
      },
      signal: options?.signal
    }
  )
  return data
}

/**
 * Get subscription by ID
 * @param id - Subscription ID
 * @returns Subscription details
 */
export async function getById(id: number): Promise<UserSubscription> {
  const { data } = await apiClient.get<UserSubscription>(`/admin/subscriptions/${id}`)
  return data
}

/**
 * Get subscription progress
 * @param id - Subscription ID
 * @returns Subscription progress with usage stats
 */
export async function getProgress(id: number): Promise<SubscriptionProgress> {
  const { data } = await apiClient.get<SubscriptionProgress>(`/admin/subscriptions/${id}/progress`)
  return data
}

/**
 * Assign subscription to user
 * @param request - Assignment request
 * @returns Created subscription
 */
export async function assign(request: AssignSubscriptionRequest): Promise<UserSubscription> {
  const { data } = await apiClient.post<UserSubscription>('/admin/subscriptions/assign', request)
  return data
}

/**
 * Bulk assign subscriptions to multiple users
 * @param request - Bulk assignment request
 * @returns Created subscriptions
 */
export async function bulkAssign(
  request: BulkAssignSubscriptionRequest
): Promise<UserSubscription[]> {
  const { data } = await apiClient.post<UserSubscription[]>(
    '/admin/subscriptions/bulk-assign',
    request
  )
  return data
}

/**
 * Extend subscription validity
 * @param id - Subscription ID
 * @param request - Extension request with days
 * @returns Updated subscription
 */
export async function extend(
  id: number,
  request: ExtendSubscriptionRequest
): Promise<UserSubscription> {
  const { data } = await apiClient.post<UserSubscription>(
    `/admin/subscriptions/${id}/extend`,
    request
  )
  return data
}

/**
 * Revoke subscription
 * @param id - Subscription ID
 * @returns Success confirmation
 */
export async function revoke(id: number): Promise<{ message: string }> {
  const { data } = await apiClient.post<{ message: string }>(`/admin/subscriptions/${id}/revoke`)
  return data
}

/**
 * Restore revoked subscription
 * @param id - Subscription ID
 * @returns Restored subscription
 */
export async function restore(id: number): Promise<UserSubscription> {
  const { data } = await apiClient.post<UserSubscription>(`/admin/subscriptions/${id}/restore`)
  return data
}

/**
 * Reset daily, weekly, and/or monthly usage quota for a subscription
 * @param id - Subscription ID
 * @param options - Which windows to reset
 * @returns Updated subscription
 */
export async function resetQuota(
  id: number,
  options: { daily: boolean; weekly: boolean; monthly: boolean }
): Promise<UserSubscription> {
  const { data } = await apiClient.post<UserSubscription>(
    `/admin/subscriptions/${id}/reset-quota`,
    options
  )
  return data
}

/**
 * List subscriptions by group
 * @param groupId - Group ID
 * @param page - Page number
 * @param pageSize - Items per page
 * @returns Paginated list of subscriptions in the group
 */
export async function listByGroup(
  groupId: number,
  page: number = 1,
  pageSize: number = 20
): Promise<PaginatedResponse<UserSubscription>> {
  const { data } = await apiClient.get<PaginatedResponse<UserSubscription>>(
    `/admin/groups/${groupId}/subscriptions`,
    {
      params: { page, page_size: pageSize }
    }
  )
  return data
}

/**
 * List subscriptions by user
 * @param userId - User ID
 * @param page - Page number
 * @param pageSize - Items per page
 * @returns Paginated list of user's subscriptions
 */
export async function listByUser(
  userId: number,
  page: number = 1,
  pageSize: number = 20
): Promise<PaginatedResponse<UserSubscription>> {
  const { data } = await apiClient.get<PaginatedResponse<UserSubscription>>(
    `/admin/users/${userId}/subscriptions`,
    {
      params: { page, page_size: pageSize }
    }
  )
  return data
}

export const subscriptionsAPI = {
  saveAdminDebugQuota: async (id: number, input: { revision: number; weekly_limit_usd: number }): Promise<UserSubscription> => {
    const { data } = await apiClient.put<UserSubscription>(`/admin/subscriptions/${id}/admin-debug/quota`, input)
    return data
  },
  previewDynamicReset: async (id: number): Promise<DynamicResetResult> => {
    const { data } = await apiClient.get<DynamicResetResult>(`/admin/subscriptions/${id}/dynamic-quota/reset`)
    return data
  },
  syncDynamicReset: async (id: number, source: { account_id: number; cycle: number }): Promise<DynamicResetResult> => {
    const { data } = await apiClient.post<DynamicResetResult>(`/admin/subscriptions/${id}/dynamic-quota/reset`, source, { timeout: 30000 })
    return data
  },
  enableAdminDebug: async (id: number): Promise<void> => {
    await apiClient.post(`/admin/subscriptions/${id}/admin-debug`)
  },
  getDynamicQuota: async (id: number): Promise<DynamicQuotaAdminStatus> => {
    const { data } = await apiClient.get<DynamicQuotaAdminStatus>(`/admin/subscriptions/${id}/dynamic-quota`)
    return data
  },
  saveDynamicQuota: async (id: number, input: DynamicQuotaInput): Promise<DynamicQuotaAdminStatus> => {
    const { data } = await apiClient.put<DynamicQuotaAdminStatus>(`/admin/subscriptions/${id}/dynamic-quota`, input)
    return data
  },
  list,
  getById,
  getProgress,
  assign,
  bulkAssign,
  extend,
  revoke,
  restore,
  resetQuota,
  listByGroup,
  listByUser
}

export default subscriptionsAPI
