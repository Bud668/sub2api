<template>
  <section class="quota-card rounded-xl border border-primary-200 bg-primary-50/50 p-2.5 text-left text-sm dark:border-primary-900 dark:bg-primary-900/10" :aria-label="t('dynamicQuota.title')">
    <div class="quota-main">
      <div class="quota-heading mb-2 flex flex-wrap items-center justify-between gap-2">
        <span class="font-semibold text-gray-900 dark:text-white">{{ t('dynamicQuota.cardTitle') }}</span>
        <span class="rounded-full px-2 py-0.5 text-xs font-medium" :class="available ? 'bg-primary-100 text-primary-700 dark:bg-primary-900 dark:text-primary-200' : 'bg-amber-100 text-amber-800 dark:bg-amber-900 dark:text-amber-100'">
          {{ t(`dynamicQuota.statuses.${knownStatus}`) }}
        </span>
      </div>
      <dl class="quota-amounts grid grid-cols-3 gap-2 rounded-lg bg-white/70 px-2.5 py-1.5 tabular-nums dark:bg-dark-800/60" data-testid="dynamic-allocation-stage">
        <div class="min-w-0" data-testid="dynamic-remaining">
          <dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('dynamicQuota.remaining') }}</dt>
          <dd class="quota-amount mt-1 break-words text-lg font-semibold tracking-tight text-primary-700 dark:text-primary-300">{{ usd(quota.remaining_usd) }}</dd>
          <dd class="mt-1 flex items-start gap-1 text-[11px] leading-4 text-gray-500 dark:text-gray-400" :title="quota.next_adjustment_percent ? t('dynamicQuota.stageAt', { percent: quota.next_adjustment_percent }) : t('dynamicQuota.noNextStage')">
            <Icon name="refresh" size="xs" class="mt-0.5 shrink-0" aria-hidden="true" />
            <span><span class="sr-only">{{ t('dynamicQuota.nextStage') }}: </span>{{ quota.next_adjustment_percent ? t('dynamicQuota.stageShort', { percent: quota.next_adjustment_percent }) : t('dynamicQuota.noNextStageShort') }}</span>
          </dd>
        </div>
        <div class="min-w-0"><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('dynamicQuota.used') }}</dt><dd class="quota-amount mt-1 break-words text-lg font-semibold tracking-tight">{{ usd(quota.used_usd) }}</dd></div>
        <div class="min-w-0" data-testid="dynamic-allocated">
          <dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('dynamicQuota.limit') }}</dt>
          <dd class="quota-amount mt-1 break-words text-lg font-semibold tracking-tight">{{ usd(quota.limit_usd) }}</dd>
          <dd class="mt-1 flex items-start gap-1 text-[11px] leading-4 text-gray-500 dark:text-gray-400" :title="`${allocationLabel}: ${allocationTime ? date(quota.last_allocation_at) : t('dynamicQuota.notAllocated')}`">
            <Icon name="refresh" size="xs" class="mt-0.5 shrink-0" aria-hidden="true" />
            <span class="sr-only">{{ allocationLabel }}: </span>
            <time v-if="allocationTime" :datetime="quota.last_allocation_at" :aria-label="date(quota.last_allocation_at)">{{ allocationTime }}</time>
            <span v-else>{{ t('dynamicQuota.notAllocated') }}</span>
          </dd>
        </div>
      </dl>
      <div class="quota-progress my-2 h-1 overflow-hidden rounded-full bg-gray-200 dark:bg-dark-600" role="progressbar" :aria-label="t('dynamicQuota.used')" :aria-valuenow="percentage" aria-valuemin="0" aria-valuemax="100">
        <div class="h-full rounded-full bg-primary-500" :style="{ width: `${percentage}%` }" />
      </div>
      <p v-if="quota.reserved_usd > 0" class="quota-reserved text-xs text-gray-500">{{ t('dynamicQuota.reserved') }} · {{ usd(quota.reserved_usd) }}</p>
      <div class="quota-footer grid gap-2">
      <div class="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-gray-600 dark:text-gray-300" data-testid="dynamic-bounds">
        <span v-if="quota.floor_limit_usd != null" class="inline-flex items-center">{{ t('dynamicQuota.floor') }} <strong class="ml-1 font-medium tabular-nums">{{ usd(quota.floor_limit_usd) }}</strong><HelpTooltip trigger="click" :content="t('dynamicQuota.floorHint')"><template #trigger><button type="button" class="rounded px-1 focus-visible:ring-2 focus-visible:ring-primary-500" :aria-label="t('dynamicQuota.floorHelp')">ⓘ</button></template></HelpTooltip></span>
        <span>{{ t('dynamicQuota.cap') }} <strong class="ml-1 font-medium tabular-nums">{{ usd(quota.max_limit_usd) }}</strong></span>
      </div>
      <div class="text-xs" data-testid="dynamic-reset" :title="t('dynamicQuota.resetHint')">
        <dl>
          <div class="flex flex-wrap justify-between gap-x-3 gap-y-1">
            <dt class="text-gray-500 dark:text-gray-400">{{ t('dynamicQuota.resetShort') }}</dt>
            <dd class="font-medium tabular-nums text-gray-900 dark:text-gray-100">
              <time v-if="resetTime" :datetime="quota.expected_reset_at">{{ resetTime }}</time>
              <span v-else>{{ t('dynamicQuota.resetUnavailable') }}</span>
            </dd>
          </div>
        </dl>
      </div>
      </div>
    </div>
    <div v-if="quota.pending_adjustment_percent" class="mt-1.5 rounded-lg bg-amber-50 px-2.5 py-1.5 text-xs text-amber-800 dark:bg-amber-900/20 dark:text-amber-200" role="status" data-testid="dynamic-pending-stage">
      <p><span class="font-medium">{{ t('dynamicQuota.pendingStage', { percent: quota.pending_adjustment_percent }) }} · </span>{{ t(`dynamicQuota.allocationWait.${pendingReason}`) }}</p>
    </div>
    <template v-if="!compact">
      <p v-if="quota.status === 'learning'" class="mt-2 text-xs text-gray-600 dark:text-gray-300">{{ t('dynamicQuota.v2LearningHint') }}</p>
      <p v-if="quota.growth_frozen" class="mt-2 text-xs text-amber-800 dark:text-amber-200">{{ t('dynamicQuota.growthFrozen') }}</p>
      <details class="mt-2 text-xs text-gray-500 dark:text-gray-400">
        <summary class="cursor-pointer font-medium">{{ t('dynamicQuota.details') }}</summary>
        <p class="my-2">{{ t('dynamicQuota.cycle') }} · #{{ quota.cycle }}</p>
        <p v-if="quota.last_change" class="mb-2 flex flex-wrap gap-x-2 tabular-nums">
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

const props = withDefaults(defineProps<{ quota: DynamicSubscriptionQuota; compact?: boolean }>(), { compact: false })
const { t, locale } = useI18n()
const resetTime = computed(() => formatDateTimeToMinute(props.quota.expected_reset_at, locale.value))
const allocationTime = computed(() => formatDate(props.quota.last_allocation_at, { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false }, locale.value))
const allocationLabel = computed(() => t(props.quota.last_change?.reason === 'initial' ? 'dynamicQuota.initialAllocatedAt' : 'dynamicQuota.allocatedAt'))
const changeReason = computed(() => ['initial', 'bounds', 'upstream_node', 'reset'].includes(props.quota.last_change?.reason || '') ? props.quota.last_change!.reason : 'bounds')
const statuses = ['active', 'learning', 'confirming', 'settling', 'reset_unconfirmed', 'identity_changed', 'quota_unavailable', 'invalid_billing_rate', 'upstream_reserve', 'disabled', 'activation_pending']
const canSpend = computed(() => ['active', 'learning'].includes(props.quota.status))
const knownStatus = computed(() => {
  if (canSpend.value && props.quota.remaining_usd <= 0) return props.quota.reserved_usd > 0 ? 'reserved' : 'exhausted'
  return canSpend.value && props.quota.growth_frozen ? 'growth_frozen' : (statuses.includes(props.quota.status) ? props.quota.status : 'quota_unavailable')
})
const available = computed(() => canSpend.value && props.quota.remaining_usd > 0)
const pendingReason = computed(() => ['budget_conflict', 'protection', 'learning', 'guard', 'awaiting_allocation'].includes(props.quota.pending_adjustment_reason || '') ? props.quota.pending_adjustment_reason : 'guard')
const percentage = computed(() => props.quota.limit_usd > 0 ? Math.min(100, Math.max(0, Math.round(props.quota.used_usd / props.quota.limit_usd * 100))) : 0)
const usd = (value: number) => Number.isFinite(value) ? new Intl.NumberFormat(undefined, { style: 'currency', currency: 'USD' }).format(value) : '—'
const date = (value?: string) => formatDateTimeToMinute(value, locale.value) || '—'
</script>

<style scoped>
.quota-card { container-type: inline-size; }
.quota-amount { line-height: 1.5rem; }
@container (min-width: 36rem) {
  .quota-main {
    display: grid;
    grid-template-columns: minmax(0, 1.2fr) minmax(15rem, 1fr);
    grid-template-areas: 'amounts heading' 'amounts footer' 'progress footer' 'reserved reserved';
    gap: 0.25rem 1rem;
    align-items: start;
  }
  .quota-heading { grid-area: heading; margin: 0; }
  .quota-amounts { grid-area: amounts; }
  .quota-progress { grid-area: progress; margin: 0; }
  .quota-footer { grid-area: footer; gap: 0.25rem; }
  .quota-reserved { grid-area: reserved; }
}
@container (max-width: 18rem) {
  .quota-amount { font-size: 0.875rem; overflow-wrap: anywhere; }
}
</style>
