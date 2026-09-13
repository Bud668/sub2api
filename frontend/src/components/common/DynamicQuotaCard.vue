<template>
  <section class="quota-card rounded-xl border border-primary-200 bg-transparent p-4 text-left text-sm dark:border-primary-900" :aria-label="t('dynamicQuota.title')">
    <div class="quota-main">
      <div v-if="!compact" class="quota-heading mb-3">
        <span class="font-semibold text-gray-900 dark:text-white">{{ t('dynamicQuota.cardTitle') }}</span>
      </div>
      <dl class="quota-amounts grid grid-cols-3 gap-4 rounded-xl bg-white p-3 tabular-nums dark:bg-dark-800/60" data-testid="dynamic-allocation-stage">
        <div class="min-w-0" data-testid="dynamic-allocated">
          <dt class="flex flex-wrap items-center gap-2 text-xs font-medium text-gray-600 dark:text-gray-300">
            <span class="font-semibold text-gray-700 dark:text-gray-200">{{ t('dynamicQuota.limit') }}</span>
            <HelpTooltip v-if="showNextAdjustment" trigger="click" :content="stageHint" class="!ml-0">
              <template #trigger="{ open, tooltipId }">
                <button
                  type="button"
                  class="inline-flex rounded-md bg-primary-100/70 px-2 py-0.5 text-xs font-medium leading-4 text-primary-700 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500 dark:bg-primary-900/40 dark:text-primary-200"
                  data-testid="dynamic-next-adjustment"
                  :title="stageHint"
                  :aria-label="stageHint"
                  :aria-expanded="open"
                  :aria-describedby="tooltipId"
                >{{ quota.next_adjustment_percent ? t('dynamicQuota.stageShort', { percent: quota.next_adjustment_percent }) : t('dynamicQuota.noNextStageShort') }}</button>
              </template>
            </HelpTooltip>
            <span
              v-else
              class="inline-flex max-w-full flex-wrap items-center gap-x-1 rounded-md bg-primary-50 px-2 py-0.5 text-[11px] font-medium leading-4 text-primary-700 dark:bg-primary-900/30 dark:text-primary-200"
              data-testid="dynamic-last-adjustment"
              :title="`${t('dynamicQuota.allocatedAt')}: ${allocationTime ? date(quota.last_allocation_at) : t('dynamicQuota.notAllocated')}`"
            >
              <Icon name="refresh" size="xs" class="shrink-0" aria-hidden="true" />
              <span class="shrink-0">{{ t('dynamicQuota.allocatedAt') }}</span>
              <time v-if="allocationTime" :datetime="quota.last_allocation_at" :aria-label="date(quota.last_allocation_at)">{{ allocationTime }}</time>
              <span v-else>{{ t('dynamicQuota.notAllocated') }}</span>
            </span>
          </dt>
          <dd class="quota-amount mt-1 break-words text-lg font-bold tracking-tight text-gray-900 dark:text-gray-100">{{ usd(quota.limit_usd) }}</dd>
          <dd v-if="showNextAdjustment" class="mt-2 flex flex-wrap items-start gap-1 text-xs leading-4 text-gray-600 dark:text-gray-300" :title="`${allocationLabel}: ${allocationTime ? date(quota.last_allocation_at) : t('dynamicQuota.notAllocated')}`">
            <Icon name="refresh" size="xs" class="mt-0.5 shrink-0" aria-hidden="true" />
            <span class="shrink-0">{{ allocationLabel }}</span>
            <time v-if="allocationTime" :datetime="quota.last_allocation_at" :aria-label="date(quota.last_allocation_at)">{{ allocationTime }}</time>
            <span v-else>{{ t('dynamicQuota.notAllocated') }}</span>
          </dd>
        </div>
        <div class="min-w-0">
          <dt class="text-xs font-semibold text-gray-700 dark:text-gray-200">{{ t('dynamicQuota.used') }}</dt>
          <dd class="quota-amount mt-1 break-words text-lg font-bold tracking-tight text-gray-900 dark:text-gray-100">{{ usd(quota.used_usd) }}</dd>
          <dd v-if="quota.reserved_usd > 0" class="quota-reserved mt-2 text-xs leading-4 text-gray-600 dark:text-gray-300">{{ t('dynamicQuota.reserved') }} · {{ usd(quota.reserved_usd) }}</dd>
        </div>
        <div class="min-w-0" data-testid="dynamic-remaining">
          <dt class="text-xs font-semibold text-gray-700 dark:text-gray-200">{{ t('dynamicQuota.remaining') }}</dt>
          <dd class="quota-amount mt-1 break-words text-lg font-bold tracking-tight text-primary-700 dark:text-primary-300">{{ usd(quota.remaining_usd) }}</dd>
        </div>
      </dl>
      <div class="quota-progress my-3 h-2 overflow-hidden rounded-full bg-gray-200 dark:bg-dark-600" role="progressbar" :aria-label="t('dynamicQuota.used')" :aria-valuenow="percentage" aria-valuemin="0" aria-valuemax="100">
        <div class="h-full rounded-full bg-primary-600 dark:bg-primary-400" :style="{ width: `${percentage}%` }" />
      </div>
      <div class="quota-footer flex flex-wrap items-center justify-between gap-x-6 gap-y-2">
      <div class="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-gray-600 dark:text-gray-300" data-testid="dynamic-bounds">
        <span v-if="!quota.fixed_slots && quota.floor_limit_usd != null" class="inline-flex items-center">{{ t('dynamicQuota.floor') }} <strong class="ml-1 font-medium tabular-nums">{{ usd(quota.floor_limit_usd) }}</strong><HelpTooltip trigger="click" :content="t('dynamicQuota.floorHint')"><template #trigger><button type="button" class="rounded px-1 focus-visible:ring-2 focus-visible:ring-primary-500" :aria-label="t('dynamicQuota.floorHelp')">ⓘ</button></template></HelpTooltip></span>
        <span>{{ t('dynamicQuota.cap') }} <strong class="ml-1 font-medium tabular-nums">{{ usd(quota.max_limit_usd) }}</strong></span>
      </div>
      <div class="text-xs" data-testid="dynamic-reset" :title="t('dynamicQuota.resetHint')">
        <dl>
          <div class="flex flex-wrap gap-x-3 gap-y-1">
            <dt class="font-medium text-gray-600 dark:text-gray-300">{{ t('dynamicQuota.resetShort') }}</dt>
            <dd class="font-medium tabular-nums text-gray-900 dark:text-gray-100">
              <time v-if="resetTime" :datetime="quota.expected_reset_at">{{ resetTime }}</time>
              <span v-else>{{ t('dynamicQuota.resetUnavailable') }}</span>
            </dd>
          </div>
        </dl>
      </div>
      </div>
    </div>
    <div v-if="showNextAdjustment && quota.pending_adjustment_percent" class="mt-2 rounded-lg bg-amber-50 px-3 py-1.5 text-xs leading-5 text-amber-800 dark:bg-amber-900/20 dark:text-amber-200" role="status" data-testid="dynamic-pending-stage">
      <p><span class="font-medium">{{ t('dynamicQuota.pendingStage', { percent: quota.pending_adjustment_percent }) }} · </span>{{ t(`dynamicQuota.allocationWait.${pendingReason}`) }}</p>
    </div>
    <template v-if="!compact">
      <p v-if="quota.status === 'learning'" class="mt-2 text-xs text-gray-600 dark:text-gray-300">{{ t(showNextAdjustment ? (quota.source_fixed_slots ? 'dynamicQuota.fixedLearningHint' : 'dynamicQuota.v2LearningHint') : 'dynamicQuota.allocationWait.learning') }}</p>
      <p v-if="quota.growth_frozen" class="mt-2 text-xs text-amber-800 dark:text-amber-200">{{ growthFrozenText }}</p>
      <details class="mt-2 text-xs text-gray-600 dark:text-gray-300">
        <summary class="cursor-pointer font-medium">{{ t('dynamicQuota.details') }}</summary>
        <p class="my-2">{{ t('dynamicQuota.cycle') }} · #{{ quota.cycle }}</p>
        <p v-if="showNextAdjustment && quota.last_change" class="mb-2 flex flex-wrap gap-x-2 tabular-nums">
          <span>{{ usd(quota.last_change.previous_usd) }} → {{ usd(quota.last_change.current_usd) }}</span>
          <span>{{ t(`dynamicQuota.changes.${changeReason}`, { percent: quota.last_change.node }) }}</span>
        </p>
        <dl class="space-y-1">
      <div class="flex flex-wrap justify-between gap-1"><dt>{{ t('dynamicQuota.started') }}</dt><dd>{{ date(quota.started_at) }}</dd></div>
      <div class="flex flex-wrap justify-between gap-1"><dt>{{ t('dynamicQuota.synced') }}</dt><dd>{{ date(quota.synced_at) }}</dd></div>
      <div class="flex flex-wrap justify-between gap-1"><dt>{{ t('dynamicQuota.confirmed') }}</dt><dd>{{ quota.confirmed_at ? date(quota.confirmed_at) : t('dynamicQuota.notReset') }}</dd></div>
        </dl>
        <p class="mt-2">{{ t('dynamicQuota.cycleHint') }}</p>
      </details>
    </template>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { DynamicSubscriptionQuota } from '@/types'
import { formatDate, formatDateTimeToMinute } from '@/utils/format'
import HelpTooltip from '@/components/common/HelpTooltip.vue'
import Icon from '@/components/icons/Icon.vue'

const props = withDefaults(defineProps<{ quota: DynamicSubscriptionQuota; compact?: boolean; showNextAdjustment?: boolean }>(), { compact: false, showNextAdjustment: false })
const { t, locale } = useI18n()
const stageHint = computed(() => {
  const q = props.quota
  if (!q.next_adjustment_percent) return t('dynamicQuota.noNextStage')
  return t('dynamicQuota.stageAt', { percent: q.next_adjustment_percent })
})
const resetTime = computed(() => formatDateTimeToMinute(props.quota.expected_reset_at, locale.value))
const allocationTime = computed(() => formatDate(props.quota.last_allocation_at, { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false }, locale.value))
const allocationLabel = computed(() => t(props.quota.last_change?.reason === 'initial' ? 'dynamicQuota.initialAllocatedAt' : 'dynamicQuota.allocatedAt'))
const changeReason = computed(() => ['initial', 'bounds', 'upstream_node', 'reset', 'seats', 'budget_safety'].includes(props.quota.last_change?.reason || '') ? props.quota.last_change!.reason : 'bounds')
const pendingReason = computed(() => ['budget_conflict', 'protection', 'learning', 'guard', 'sync_recovery', 'estimate_anomaly', 'awaiting_allocation'].includes(props.quota.pending_adjustment_reason || '') ? props.quota.pending_adjustment_reason : 'guard')
const growthFrozenText = computed(() => t(props.quota.growth_frozen_reason === 'sync_recovery' ? 'dynamicQuota.growthFrozenSync' : props.quota.growth_frozen_reason === 'estimate_anomaly' ? 'dynamicQuota.growthFrozenEstimate' : 'dynamicQuota.growthFrozen'))
const percentage = computed(() => props.quota.limit_usd > 0 ? Math.min(100, Math.max(0, Math.round(props.quota.used_usd / props.quota.limit_usd * 100))) : 0)
const usd = (value: number) => Number.isFinite(value) ? new Intl.NumberFormat(undefined, { style: 'currency', currency: 'USD' }).format(value) : '—'
const date = (value?: string) => formatDateTimeToMinute(value, locale.value) || '—'
</script>

<style scoped>
.quota-card { container-type: inline-size; }
.quota-amounts { grid-template-rows: auto auto auto; row-gap: 0; }
.quota-amounts > div { display: grid; grid-row: span 3; grid-template-rows: subgrid; }
.quota-amounts dt { align-self: center; }
.quota-amount { line-height: 1.75rem; }
@container (min-width: 36rem) {
  .quota-amount { font-size: 1.5rem; line-height: 2rem; }
}
@container (max-width: 32rem) {
  .quota-amounts { grid-template-columns: repeat(2, minmax(0, 1fr)); row-gap: 0.5rem; }
  [data-testid="dynamic-allocated"] { grid-column: 1 / -1; }
}
</style>
