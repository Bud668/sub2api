<template>
  <section class="debug-usage" data-testid="admin-debug-usage" :aria-label="t('dynamicQuota.adminDebug')">
    <dl class="debug-summary grid grid-cols-3 gap-4 rounded-xl bg-white p-3 tabular-nums dark:bg-dark-800/60">
      <div class="debug-limit min-w-0">
        <dt class="text-xs font-semibold text-gray-700 dark:text-gray-200">{{ t('dynamicQuota.debugWeeklyLimit') }}</dt>
        <dd class="debug-amount mt-1 break-words text-lg font-bold tracking-tight text-gray-900 dark:text-gray-100">{{ limit === null ? '—' : limit > 0 ? formatCurrency(limit) : t('userSubscriptions.unlimited') }}</dd>
      </div>
      <div class="min-w-0">
        <dt class="text-xs font-semibold text-gray-700 dark:text-gray-200">{{ t('dynamicQuota.debugWeeklyUsed') }}</dt>
        <dd class="debug-amount mt-1 break-words text-lg font-bold tracking-tight text-gray-900 dark:text-gray-100">{{ formatCurrency(subscription.weekly_usage_usd) }}</dd>
        <dd v-if="subscription.admin_debug_quota?.reserved_usd" class="mt-2 text-xs leading-4 text-gray-600 dark:text-gray-300">{{ t('dynamicQuota.reserved') }} · {{ formatCurrency(subscription.admin_debug_quota.reserved_usd) }}</dd>
      </div>
      <div class="min-w-0" data-testid="admin-debug-remaining" :title="t('dynamicQuota.debugRemainingHint')">
        <dt class="text-xs font-semibold text-gray-700 dark:text-gray-200">{{ t('dynamicQuota.remaining') }}</dt>
        <dd class="debug-amount mt-1 break-words text-lg font-bold tracking-tight text-primary-700 dark:text-primary-300">{{ remaining }}</dd>
      </div>
    </dl>
    <div v-if="limit !== null && limit > 0" class="my-3 h-2 overflow-hidden rounded-full bg-gray-200 dark:bg-dark-600" role="progressbar" :aria-label="t('dynamicQuota.debugWeeklyUsed')" :aria-valuenow="percentage" aria-valuemin="0" aria-valuemax="100">
      <div class="h-full rounded-full" :class="percentage >= 90 ? 'bg-red-500' : percentage >= 70 ? 'bg-orange-500' : 'bg-violet-500 dark:bg-violet-400'" :style="{ width: `${percentage}%` }" />
    </div>
    <div class="mt-2 flex flex-wrap items-center justify-between gap-x-6 gap-y-2 text-xs leading-5 text-gray-600 dark:text-gray-300">
      <p :title="t('dynamicQuota.adminDebugHint')">{{ t('dynamicQuota.debugUsageHint') }}</p>
      <dl v-if="subscription.admin_debug_quota?.follow_reset || $slots.reset" class="flex flex-wrap gap-x-3 gap-y-1">
        <dt class="font-medium">{{ t('dynamicQuota.debugWeeklyReset') }}</dt>
        <dd class="font-medium text-gray-900 dark:text-gray-100">
          <template v-if="subscription.admin_debug_quota?.follow_reset">{{ subscription.admin_debug_quota.reset_pending ? t('dynamicQuota.debugResetPending') : subscription.admin_debug_quota.expected_reset_at ? t('dynamicQuota.debugResetExpected', { time: formatDateTimeToMinute(subscription.admin_debug_quota.expected_reset_at) }) : t('dynamicQuota.statuses.quota_unavailable') }}</template>
          <slot v-else name="reset" />
        </dd>
      </dl>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { UserSubscription } from '@/types'
import { formatCurrency, formatDateTimeToMinute } from '@/utils/format'

const props = defineProps<{ subscription: Pick<UserSubscription, 'weekly_usage_usd' | 'admin_debug_quota'> }>()
const { t } = useI18n()
const limit = computed(() => props.subscription.admin_debug_quota?.weekly_limit_usd ?? null)
const remaining = computed(() => {
  const quota = props.subscription.admin_debug_quota
  if (quota?.reset_pending) return formatCurrency(0)
  if (limit.value === 0) return t('userSubscriptions.unlimited')
  const value = quota?.remaining_usd
  return value != null && Number.isFinite(value) ? formatCurrency(Math.max(0, value)) : '—'
})
const percentage = computed(() => {
  const value = limit.value !== null && limit.value > 0 ? (props.subscription.weekly_usage_usd || 0) / limit.value * 100 : 0
  return Number.isFinite(value) ? Math.min(100, Math.max(0, value)) : 0
})
</script>

<style scoped>
.debug-usage { container-type: inline-size; }
@container (min-width: 36rem) {
  .debug-amount { font-size: 1.5rem; line-height: 2rem; }
}
@container (max-width: 32rem) {
  .debug-summary { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .debug-limit { grid-column: 1 / -1; }
}
</style>
