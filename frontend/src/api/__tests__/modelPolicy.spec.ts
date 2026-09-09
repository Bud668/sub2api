import { describe, expect, it, vi } from 'vitest'
import { getModelPolicyCandidates, modelWindow, validModelRequestRules, type ModelPolicy } from '../modelPolicy'
import { apiClient } from '../client'
import { mapErrorCategory } from '@/utils/errorCategory'

const groups = vi.hoisted(() => ({ getAll: vi.fn(), getModelAllowlistCandidates: vi.fn() }))
vi.mock('../admin/groups', () => groups)

describe('model request quota form', () => {
  it('classifies local model quota denials as a rate limit, not an upstream or cyber error', () => {
    expect(mapErrorCategory('request', 'model_request_quota_exceeded')).toBe('rate_limit')
  })
  it('requires explicit finite quotas, not a zero-means-unlimited accident', () => {
    const rule = { model: 'gpt-5.6-sol', mode: 'limited' as const, request_limit: 3, window_hours: 6 }
    expect(validModelRequestRules([rule])).toBe(true)
    expect(validModelRequestRules([{ ...rule, request_limit: 0 }])).toBe(false)
    expect(validModelRequestRules([{ ...rule, window_hours: 0 }])).toBe(false)
    expect(validModelRequestRules([{ ...rule, window_hours: 1.5 }])).toBe(false)
    expect(validModelRequestRules([{ ...rule, model: 'gpt-*' }])).toBe(false)
    expect(validModelRequestRules([rule, { ...rule, model: 'GPT-5.6-SOL' }])).toBe(false)
    expect(validModelRequestRules([{ ...rule, mode: 'unlimited', request_limit: 0, window_hours: 0 }])).toBe(true)
    expect(validModelRequestRules([{ ...rule, window_mode: 'daily', window_hours: 0 }])).toBe(true)
    expect(validModelRequestRules([{ ...rule, window_mode: 'daily', request_limit: 0 }])).toBe(false)
    expect(validModelRequestRules([])).toBe(true)
  })
  it('shows expired windows as available without modifying server state', () => {
    const policy: ModelPolicy = { enabled: true, revision: 1, rules: [], windows: [{ model: 'sol', used: 3, resets_at: '2026-09-08T16:00:00Z' }] }
    expect(modelWindow(policy, 'sol', Date.parse('2026-09-08T15:00:00Z'))?.used).toBe(3)
    expect(modelWindow(policy, 'sol', Date.parse('2026-09-08T16:00:00Z'))).toBeUndefined()
    expect(policy.windows[0]?.used).toBe(3)
  })
  it('reuses system candidates without duplicate identifiers or wildcards', async () => {
    groups.getAll.mockResolvedValue([{ id: 7, platform: 'openai' }, { id: 8, platform: 'openai' }])
    groups.getModelAllowlistCandidates.mockResolvedValueOnce([' GPT-5.6-SOL ', 'gpt-*', ''])
      .mockResolvedValueOnce(['gpt-5.6-sol', 'gpt-5.6-luna'])
    expect(await getModelPolicyCandidates()).toEqual(['gpt-5.6-luna', 'gpt-5.6-sol'])
    expect(groups.getModelAllowlistCandidates).toHaveBeenCalledWith(7, 'openai', true)
  })
  it('requests canonical candidates only for the user model editor', async () => {
    const actual = await vi.importActual<typeof import('../admin/groups')>('../admin/groups')
    const get = vi.spyOn(apiClient, 'get').mockResolvedValue({ data: { models: ['gpt-5.6-sol'] } })
    try {
      await actual.getModelAllowlistCandidates(7, 'openai')
      expect(get).toHaveBeenLastCalledWith('/admin/groups/7/model-allowlist-candidates', { params: { platform: 'openai' } })
      await actual.getModelAllowlistCandidates(7, 'openai', true)
      expect(get).toHaveBeenLastCalledWith('/admin/groups/7/model-allowlist-candidates', { params: { platform: 'openai', user_model_policy: true } })
    } finally { get.mockRestore() }
  })
})
