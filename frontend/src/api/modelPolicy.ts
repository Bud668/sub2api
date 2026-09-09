import { apiClient } from './client'
import { getAll, getModelAllowlistCandidates } from './admin/groups'

export interface ModelRequestRule {
  model: string
  mode: 'deny' | 'unlimited' | 'limited'
  request_limit: number
  window_hours: number
  window_mode?: 'daily' | 'hours'
}
export interface ModelRequestWindow { model: string; used: number; resets_at: string | null }
export interface ModelPolicy {
  enabled: boolean
  revision: number
  rules: ModelRequestRule[]
  windows: ModelRequestWindow[]
}

export async function getUserModelPolicy(id: number, signal?: AbortSignal): Promise<ModelPolicy> {
  return (await apiClient.get<ModelPolicy>(`/admin/users/${id}/model-policy`, { signal })).data
}
export async function saveUserModelPolicy(id: number, revision: number, rules: ModelRequestRule[]): Promise<ModelPolicy> {
  return (await apiClient.put<ModelPolicy>(`/admin/users/${id}/model-policy`, { revision, rules })).data
}
export async function resetUserModelQuota(id: number, model: string): Promise<ModelPolicy> {
  return (await apiClient.post<ModelPolicy>(`/admin/users/${id}/model-policy/reset`, { model })).data
}
export async function activateUserModelPolicies(): Promise<void> {
  await apiClient.post('/admin/model-request-policies/activate', { confirm: 'enable-user-model-policies' })
}
export async function getMyModelPolicy(): Promise<ModelPolicy> {
  return (await apiClient.get<ModelPolicy>('/user/model-policy')).data
}

export async function getModelPolicyCandidates(): Promise<string[]> {
  const groups = await getAll()
  // ponytail: one existing request per group; aggregate server-side if group count becomes large.
  const lists = await Promise.all(groups.map(g => getModelAllowlistCandidates(g.id, g.platform, true)))
  return [...new Set(lists.flat().map(m => m.trim().toLowerCase())
    .filter(m => /^[a-z0-9][a-z0-9._:/+\[\]-]{0,199}$/.test(m)))].sort()
}

export function validModelRequestRules(rules: ModelRequestRule[]): boolean {
  const seen = new Set<string>()
  return rules.length <= 200 && rules.every(rule => {
    const model = rule.model.trim().toLowerCase()
    if (!/^[a-z0-9][a-z0-9._:/+\[\]-]{0,199}$/.test(model) || seen.has(model)) return false
    seen.add(model)
    return rule.mode === 'deny' || rule.mode === 'unlimited' || (rule.mode === 'limited' &&
      Number.isInteger(rule.request_limit) && rule.request_limit >= 1 && rule.request_limit <= 1000000 &&
      (rule.window_mode === 'daily' || ((rule.window_mode === undefined || rule.window_mode === 'hours') &&
        Number.isInteger(rule.window_hours) && rule.window_hours >= 1 && rule.window_hours <= 720)))
  })
}

export function modelWindow(policy: ModelPolicy, model: string, now = Date.now()): ModelRequestWindow | undefined {
  return policy.windows.find(w => w.model === model && w.resets_at && Date.parse(w.resets_at) > now)
}
