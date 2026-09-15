<template>
  <section class="quota-card rounded-xl border border-primary-200 bg-transparent p-4 text-left text-sm dark:border-primary-900" :aria-label="t('dynamicQuota.title')">
    <div class="quota-main">
      <div v-if="!compact" class="quota-heading mb-3">
        <span class="font-semibold text-gray-900 dark:text-white">{{ t('dynamicQuota.cardTitle') }}</span>
      </div>
      <dl class="quota-amounts grid grid-cols-3 gap-4 rounded-xl bg-white p-3 tabular-nums dark:bg-dark-800/60" data-testid="dynamic-allocation-stage">
        <div class="min-w-0 text-left" data-testid="dynamic-allocated">
          <dt class="flex min-w-0 flex-wrap items-center justify-start gap-1.5 text-xs font-semibold text-gray-700 dark:text-gray-200" data-testid="dynamic-bounds">
            <span>{{ t('dynamicQuota.limit') }}</span>
            <span class="inline-flex shrink-0 items-center text-[11px] font-medium leading-4 text-gray-600 dark:text-gray-300" data-testid="dynamic-cap">{{ t('dynamicQuota.capShort') }} · {{ usd(quota.max_limit_usd) }}</span>
            <span v-if="!quota.fixed_slots && quota.floor_limit_usd != null" class="inline-flex shrink-0 items-center rounded-md bg-gray-100 px-1.5 py-0.5 text-[10px] font-medium leading-4 text-gray-700 ring-1 ring-inset ring-gray-200 dark:bg-dark-700 dark:text-gray-200 dark:ring-dark-500">{{ t('dynamicQuota.floor') }} · {{ usd(quota.floor_limit_usd) }}<HelpTooltip trigger="click" :content="t('dynamicQuota.floorHint')"><template #trigger><button type="button" class="rounded px-1 focus-visible:ring-2 focus-visible:ring-primary-500" :aria-label="t('dynamicQuota.floorHelp')">ⓘ</button></template></HelpTooltip></span>
          </dt>
          <dd class="quota-amount mt-1 flex min-w-0 flex-wrap items-start justify-start gap-1.5 text-lg font-bold tracking-tight text-gray-900 dark:text-gray-100">
            <span class="min-w-0 break-words">{{ usd(quota.limit_usd) }}</span>
            <span
              v-if="quotaDelta"
              class="quota-delta inline-flex shrink-0 rounded-full px-1.5 py-0.5 text-[11px] font-semibold leading-4 ring-1 ring-inset"
              :class="quotaDelta > 0
                ? 'bg-emerald-50 text-emerald-700 ring-emerald-200 dark:bg-emerald-950/50 dark:text-emerald-300 dark:ring-emerald-800'
                : 'bg-red-50 text-red-700 ring-red-200 dark:bg-red-950/50 dark:text-red-300 dark:ring-red-800'"
              data-testid="dynamic-quota-delta"
              :title="deltaLabel"
              :aria-label="deltaLabel"
            >
              {{ deltaText }}
            </span>
          </dd>
        </div>
        <div class="min-w-0 text-center" data-testid="dynamic-used">
          <dt class="flex min-w-0 flex-wrap items-center justify-center gap-1.5 text-xs font-semibold text-gray-700 dark:text-gray-200">
            <span>{{ t('dynamicQuota.used') }}</span>
            <span v-if="quota.reserved_usd > 0" class="quota-reserved inline-flex w-fit shrink-0 rounded-md bg-amber-50 px-1.5 py-0.5 text-[10px] font-medium leading-4 text-amber-700 ring-1 ring-inset ring-amber-200 dark:bg-amber-950/40 dark:text-amber-200 dark:ring-amber-800/70" data-testid="dynamic-reserved">{{ t('dynamicQuota.reserved') }} · {{ usd(quota.reserved_usd) }}</span>
          </dt>
          <dd class="quota-amount mt-1 break-words text-lg font-bold tracking-tight text-gray-900 dark:text-gray-100">{{ usd(quota.used_usd) }}</dd>
        </div>
        <div class="min-w-0 text-right" data-testid="dynamic-remaining">
          <dt class="text-xs font-semibold text-gray-700 dark:text-gray-200">{{ t('dynamicQuota.remaining') }}</dt>
          <dd class="quota-amount mt-1 break-words text-lg font-bold tracking-tight text-primary-700 dark:text-primary-300">{{ usd(quota.remaining_usd) }}</dd>
        </div>
      </dl>
      <div class="quota-progress my-3 h-2 overflow-hidden rounded-full bg-gray-200 dark:bg-dark-600" role="progressbar" :aria-label="t('dynamicQuota.used')" :aria-valuenow="percentage" aria-valuemin="0" aria-valuemax="100">
        <div class="h-full rounded-full bg-primary-600 dark:bg-primary-400" :style="{ width: `${percentage}%` }" />
      </div>
    </div>
    <div v-if="showNextAdjustment && quota.pending_adjustment_percent" class="mt-2 rounded-lg bg-amber-50 px-3 py-1.5 text-xs leading-5 text-amber-800 dark:bg-amber-900/20 dark:text-amber-200" role="status" data-testid="dynamic-pending-stage">
      <p><span class="font-medium">{{ t('dynamicQuota.pendingStage', { percent: quota.pending_adjustment_percent }) }} · </span>{{ t(`dynamicQuota.allocationWait.${pendingReason}`) }}</p>
    </div>
    <template v-if="!compact">
      <p v-if="quota.status === 'learning'" class="mt-2 text-xs text-gray-600 dark:text-gray-300">{{ t(showNextAdjustment ? (quota.source_fixed_slots ? 'dynamicQuota.fixedLearningHint' : 'dynamicQuota.v2LearningHint') : 'dynamicQuota.allocationWait.learning') }}</p>
      <p v-if="quota.growth_frozen" class="mt-2 text-xs text-amber-800 dark:text-amber-200">{{ growthFrozenText }}</p>
    </template>
    <dl class="quota-meta mt-3 grid grid-cols-2 gap-x-4 gap-y-2 border-t border-gray-200 pt-3 text-xs dark:border-dark-600" data-testid="dynamic-quota-meta">
      <div class="min-w-0" data-testid="dynamic-start"><dt class="text-gray-500 dark:text-gray-400">{{ t('dynamicQuota.started') }}</dt><dd class="mt-0.5 break-words font-medium tabular-nums text-gray-900 dark:text-gray-100">{{ date(quota.started_at) }}</dd></div>
      <div class="min-w-0" data-testid="dynamic-sync"><dt class="text-gray-500 dark:text-gray-400">{{ t('dynamicQuota.synced') }}</dt><dd class="mt-0.5 break-words font-medium tabular-nums text-gray-900 dark:text-gray-100">{{ date(quota.synced_at) }}</dd></div>
      <div class="min-w-0" data-testid="dynamic-confirmed"><dt class="text-gray-500 dark:text-gray-400">{{ t('dynamicQuota.confirmed') }}</dt><dd class="mt-0.5 break-words font-medium tabular-nums text-gray-900 dark:text-gray-100">{{ quota.confirmed_at ? date(quota.confirmed_at) : t('dynamicQuota.notReset') }}</dd></div>
      <div class="min-w-0" data-testid="dynamic-reset" :title="t('dynamicQuota.resetHint')"><dt class="text-gray-500 dark:text-gray-400">{{ t('dynamicQuota.resetShort') }}</dt><dd class="mt-0.5 break-words font-medium tabular-nums text-gray-900 dark:text-gray-100"><time v-if="resetTime" :datetime="quota.expected_reset_at">{{ resetTime }}</time><span v-else>{{ t('dynamicQuota.resetUnavailable') }}</span></dd></div>
    </dl>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { DynamicSubscriptionQuota } from '@/types'
import { formatDateTimeToMinute } from '@/utils/format'
import HelpTooltip from '@/components/common/HelpTooltip.vue'

const props = withDefaults(defineProps<{ quota: DynamicSubscriptionQuota; compact?: boolean; showNextAdjustment?: boolean }>(), { compact: false, showNextAdjustment: false })
const { t, locale } = useI18n()
const resetTime = computed(() => formatDateTimeToMinute(props.quota.expected_reset_at, locale.value))
const pendingReason = computed(() => ['budget_conflict', 'protection', 'learning', 'guard', 'sync_recovery', 'estimate_anomaly', 'awaiting_allocation'].includes(props.quota.pending_adjustment_reason || '') ? props.quota.pending_adjustment_reason : 'guard')
const growthFrozenText = computed(() => t(props.quota.growth_frozen_reason === 'sync_recovery' ? 'dynamicQuota.growthFrozenSync' : props.quota.growth_frozen_reason === 'estimate_anomaly' ? 'dynamicQuota.growthFrozenEstimate' : 'dynamicQuota.growthFrozen'))
const percentage = computed(() => props.quota.limit_usd > 0 ? Math.min(100, Math.max(0, Math.round(props.quota.used_usd / props.quota.limit_usd * 100))) : 0)
const usd = (value: number) => Number.isFinite(value) ? `${value < 0 ? '−$' : '$'}${new Intl.NumberFormat(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 }).format(Math.abs(value))}` : '—'
const quotaDelta = computed(() => {
  const value = props.quota.last_change_usd ?? (props.quota.last_change ? props.quota.last_change.current_usd - props.quota.last_change.previous_usd : 0)
  return Number.isFinite(value) && Math.abs(value) >= 0.005 ? value : 0
})
const deltaText = computed(() => `${quotaDelta.value > 0 ? '+' : '−'}${usd(Math.abs(quotaDelta.value))}`)
const deltaLabel = computed(() => t(quotaDelta.value > 0 ? 'dynamicQuota.increasedBy' : 'dynamicQuota.decreasedBy', { amount: usd(Math.abs(quotaDelta.value)) }))
const date = (value?: string) => formatDateTimeToMinute(value, locale.value) || '—'
</script>

<style scoped>
.quota-card { container-type: inline-size; }
.quota-amounts { grid-template-rows: auto auto auto; row-gap: 0; }
.quota-amounts > div { display: grid; grid-row: span 3; grid-template-rows: subgrid; }
.quota-amounts dt { align-self: center; }
.quota-amount { line-height: 1.75rem; }
.quota-delta { transform: translateY(-0.2rem); }
.quota-meta { padding-inline: 0.75rem; }
.quota-meta > div:nth-child(odd) { text-align: left; }
.quota-meta > div:nth-child(even) { text-align: right; }
@container (min-width: 36rem) {
  .quota-amounts { padding-inline: 2rem; }
  .quota-amount { font-size: 1.5rem; line-height: 2rem; }
  .quota-meta { grid-template-columns: repeat(4, minmax(0, 1fr)); }
  .quota-meta > div:nth-child(2), .quota-meta > div:nth-child(3) { text-align: center; }
}
@container (max-width: 32rem) {
  .quota-amounts { grid-template-columns: repeat(2, minmax(0, 1fr)); row-gap: 0.5rem; }
  [data-testid="dynamic-allocated"] { grid-column: 1 / -1; }
  [data-testid="dynamic-used"] { text-align: left; }
  [data-testid="dynamic-used"] dt { justify-content: flex-start; }
}
</style>
