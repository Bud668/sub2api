<template>
  <HelpTooltip trigger="click" class="!ml-0" data-testid="subscription-status" :data-status="state">
    <template #trigger="{ open, tooltipId }">
      <button type="button" class="inline-flex max-w-full items-center gap-1.5 rounded-full px-2.5 py-1 text-left text-xs font-medium leading-4 outline-offset-2 focus-visible:ring-2 focus-visible:ring-primary-500" :class="tone" :title="details" :aria-expanded="open" :aria-describedby="tooltipId">
        <span class="h-1.5 w-1.5 shrink-0 rounded-full bg-current" aria-hidden="true" />
        <span>{{ label(state) }}</span>
      </button>
    </template>
    <p class="whitespace-pre-line pr-4" data-testid="subscription-status-details">{{ details }}</p>
  </HelpTooltip>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { UserSubscription } from '@/types'
import HelpTooltip from '@/components/common/HelpTooltip.vue'

const props = defineProps<{ subscription: Pick<UserSubscription, 'status' | 'admin_debug' | 'admin_debug_quota' | 'dynamic_quota'> & Partial<Pick<UserSubscription, 'weekly_usage_usd'>> }>()
const { t } = useI18n()
const lifecycle = computed(() => ['active', 'expired', 'revoked', 'suspended'].includes(props.subscription.status) ? props.subscription.status : 'unknown')
const quotaState = computed(() => {
  if (props.subscription.admin_debug && props.subscription.admin_debug_quota) {
    const debug = props.subscription.admin_debug_quota
    if (debug.reset_pending) return 'settling'
    if (debug.weekly_limit_usd > 0 && (props.subscription.weekly_usage_usd || 0) >= debug.weekly_limit_usd) return 'exhausted'
    return 'disabled'
  }
  const q = props.subscription.admin_debug ? null : props.subscription.dynamic_quota
  if (!q) return 'disabled'
  if (q.activation_pending && q.requested_enabled) return 'activation_pending'
  if (!q.enabled) return 'disabled'
  if (['active', 'learning'].includes(q.status)) {
    if (!Number.isFinite(q.remaining_usd)) return 'quota_unavailable'
    if (q.remaining_usd <= 0) return q.reserved_usd > 0 ? 'reserved' : 'exhausted'
    if (q.growth_frozen) return 'growth_frozen'
    return q.status
  }
  return ['confirming', 'settling', 'reset_unconfirmed', 'identity_changed', 'quota_unavailable', 'invalid_billing_rate', 'upstream_reserve', 'activation_pending'].includes(q.status) ? q.status : 'quota_unavailable'
})
const state = computed(() => lifecycle.value !== 'active' ? lifecycle.value : quotaState.value === 'disabled' ? 'active' : quotaState.value)
const label = (value: string): string => {
  if (['expired', 'revoked'].includes(value)) return t(`userSubscriptions.status.${value}`)
  if (['active', 'learning', 'growth_frozen', 'exhausted', 'suspended', 'unknown'].includes(value)) return t(`subscriptionStatus.labels.${value}`)
  return t(`dynamicQuota.statuses.${value}`)
}
const details = computed(() => {
  const lines = [t('subscriptionStatus.subscription') + ': ' + label(lifecycle.value)]
  if (props.subscription.admin_debug) lines.push(t('dynamicQuota.debugUsageHint'))
  else if (quotaState.value !== 'disabled') lines.push(t('subscriptionStatus.quota') + ': ' + t(`dynamicQuota.statuses.${quotaState.value}`))
  lines.push(lifecycle.value !== 'active' ? t('subscriptionStatus.inactiveHint')
    : quotaState.value === 'activation_pending' ? t(props.subscription.dynamic_quota?.fixed_slots ? 'dynamicQuota.fixedPendingActivation' : 'dynamicQuota.pendingActivation')
    : props.subscription.admin_debug ? t(props.subscription.admin_debug_quota?.reset_pending ? 'dynamicQuota.debugResetPending' : 'dynamicQuota.adminDebugHint')
    : quotaState.value === 'disabled' ? t('subscriptionStatus.nativeHint')
    : ['active', 'learning', 'growth_frozen'].includes(quotaState.value) ? t('subscriptionStatus.availableHint')
    : t('subscriptionStatus.blockedHint'))
  return lines.join('\n')
})
const tone = computed(() => {
  if (state.value === 'active') return 'bg-emerald-100 text-emerald-800 dark:bg-emerald-900/40 dark:text-emerald-200'
  if (state.value === 'learning') return 'bg-blue-100 text-blue-800 dark:bg-blue-900/40 dark:text-blue-200'
  if (['expired', 'revoked', 'suspended'].includes(state.value)) return 'bg-gray-100 text-gray-700 dark:bg-dark-700 dark:text-gray-200'
  return 'bg-amber-100 text-amber-800 dark:bg-amber-900/40 dark:text-amber-200'
})
</script>
