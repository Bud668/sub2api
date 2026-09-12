<template>
  <section class="debug-usage" data-testid="admin-debug-usage" :aria-label="t('dynamicQuota.adminDebug')">
    <dl class="debug-summary grid grid-cols-2 gap-4 rounded-xl bg-white p-3 tabular-nums dark:bg-dark-800/60">
      <div class="min-w-0">
        <dt class="text-xs font-medium text-gray-600 dark:text-gray-300">{{ t('dynamicQuota.debugWeeklyUsed') }}</dt>
        <dd class="debug-amount mt-1 break-words text-lg font-semibold tracking-tight text-gray-900 dark:text-gray-100">{{ formatCurrency(subscription.weekly_usage_usd) }}</dd>
      </div>
      <div class="min-w-0">
        <dt class="text-xs font-medium text-gray-600 dark:text-gray-300">{{ t('dynamicQuota.debugWeeklyLimit') }}</dt>
        <dd class="debug-amount mt-1 break-words text-lg font-semibold tracking-tight text-gray-900 dark:text-gray-100">{{ limit === null ? '—' : limit > 0 ? formatCurrency(limit) : t('userSubscriptions.unlimited') }}</dd>
      </div>
      <div v-if="subscription.admin_debug_quota?.follow_reset || $slots.reset" class="debug-reset col-span-2 min-w-0">
        <dt class="text-xs font-medium text-gray-600 dark:text-gray-300">{{ t('dynamicQuota.debugWeeklyReset') }}</dt>
        <dd class="mt-2 text-sm font-medium text-gray-700 dark:text-gray-200">
          <template v-if="subscription.admin_debug_quota?.follow_reset">{{ subscription.admin_debug_quota.reset_pending ? t('dynamicQuota.debugResetPending') : subscription.admin_debug_quota.expected_reset_at ? t('dynamicQuota.debugResetExpected', { time: formatDateTimeToMinute(subscription.admin_debug_quota.expected_reset_at) }) : t('dynamicQuota.statuses.quota_unavailable') }}</template>
          <slot v-else name="reset" />
        </dd>
      </div>
    </dl>
    <div v-if="limit !== null && limit > 0" class="my-3 h-2 overflow-hidden rounded-full bg-gray-200 dark:bg-dark-600" role="progressbar" :aria-label="t('dynamicQuota.debugWeeklyUsed')" :aria-valuenow="percentage" aria-valuemin="0" aria-valuemax="100">
      <div class="h-full rounded-full" :class="percentage >= 90 ? 'bg-red-500' : percentage >= 70 ? 'bg-orange-500' : 'bg-violet-500 dark:bg-violet-400'" :style="{ width: `${percentage}%` }" />
    </div>
    <p class="mt-2 text-xs leading-5 text-gray-600 dark:text-gray-300" :title="t('dynamicQuota.adminDebugHint')">{{ t('dynamicQuota.debugUsageHint') }}</p>
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
const percentage = computed(() => {
  const value = limit.value !== null && limit.value > 0 ? (props.subscription.weekly_usage_usd || 0) / limit.value * 100 : 0
  return Number.isFinite(value) ? Math.min(100, Math.max(0, value)) : 0
})
</script>

<style scoped>
.debug-usage { container-type: inline-size; }
@container (min-width: 36rem) {
  .debug-summary { grid-template-columns: repeat(3, minmax(0, 1fr)); }
  .debug-reset { grid-column: auto; }
  .debug-amount { font-size: 1.5rem; line-height: 2rem; }
}
</style>
