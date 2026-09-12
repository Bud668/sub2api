<template>
  <section class="rounded-xl border border-primary-200 bg-primary-50/50 p-3 text-sm dark:border-primary-900 dark:bg-primary-900/10" :aria-label="t('dynamicQuota.title')">
    <div class="mb-3 flex flex-wrap items-center justify-between gap-2">
      <span class="font-semibold text-gray-900 dark:text-white">{{ t('dynamicQuota.cardTitle') }}</span>
      <span class="rounded-full px-2 py-0.5 text-xs font-medium" :class="available ? 'bg-primary-100 text-primary-700 dark:bg-primary-900 dark:text-primary-200' : 'bg-amber-100 text-amber-800 dark:bg-amber-900 dark:text-amber-100'">
        {{ t(`dynamicQuota.statuses.${knownStatus}`) }}
      </span>
    </div>
    <div v-if="!compact" class="mb-4 tabular-nums"><p class="text-xs text-gray-500">{{ t('dynamicQuota.remaining') }}</p><p class="mt-1 text-3xl font-semibold tracking-tight text-primary-700 dark:text-primary-300">{{ usd(quota.remaining_usd) }}</p></div>
    <dl class="grid gap-3 tabular-nums" :class="!compact ? 'grid-cols-2' : 'grid-cols-3'">
      <div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('dynamicQuota.used') }}</dt><dd class="mt-1 break-words font-semibold">{{ usd(quota.used_usd) }}</dd></div>
      <div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('dynamicQuota.limit') }}</dt><dd class="mt-1 break-words font-semibold">{{ usd(quota.limit_usd) }}</dd></div>
      <div v-if="compact"><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('dynamicQuota.remaining') }}</dt><dd class="mt-1 break-words font-semibold text-primary-700 dark:text-primary-300">{{ usd(quota.remaining_usd) }}</dd></div>
    </dl>
    <div class="my-3 h-1.5 overflow-hidden rounded-full bg-gray-200 dark:bg-dark-600" role="progressbar" :aria-label="t('dynamicQuota.used')" :aria-valuenow="percentage" aria-valuemin="0" aria-valuemax="100">
      <div class="h-full rounded-full bg-primary-500" :style="{ width: `${percentage}%` }" />
    </div>
    <p v-if="quota.reserved_usd > 0" class="text-xs text-gray-500">{{ t('dynamicQuota.reserved') }} · {{ usd(quota.reserved_usd) }}</p>
    <div class="my-3 border-t border-primary-100 pt-3 text-xs dark:border-primary-900" data-testid="dynamic-reset">
      <dl>
        <div class="flex flex-wrap justify-between gap-x-3 gap-y-1">
          <dt class="text-gray-500 dark:text-gray-400">{{ t('dynamicQuota.expected') }}</dt>
          <dd class="font-medium tabular-nums text-gray-900 dark:text-gray-100">
            <time v-if="resetTime" :datetime="quota.expected_reset_at">{{ resetTime }}</time>
            <span v-else>{{ t('dynamicQuota.resetUnavailable') }}</span>
          </dd>
        </div>
      </dl>
      <p class="mt-1 text-gray-500 dark:text-gray-400">{{ t('dynamicQuota.resetHint') }}</p>
    </div>
    <template v-if="!compact">
      <div class="mb-3 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-gray-500">
        <span>{{ t('dynamicQuota.cap') }} {{ usd(quota.max_limit_usd) }}</span>
        <span v-if="quota.floor_limit_usd != null" class="inline-flex items-center">{{ t('dynamicQuota.floor') }} {{ usd(quota.floor_limit_usd) }}<HelpTooltip trigger="click" :content="t('dynamicQuota.floorHint')"><template #trigger><button type="button" class="rounded px-1 focus-visible:ring-2 focus-visible:ring-primary-500" :aria-label="t('dynamicQuota.floorHelp')">ⓘ</button></template></HelpTooltip></span>
      </div>
      <p v-if="quota.next_adjustment_percent" class="mb-2 text-xs text-gray-500">{{ t('dynamicQuota.nextAdjustment', { percent: quota.next_adjustment_percent }) }}</p>
      <p v-if="quota.status === 'learning'" class="mt-2 text-xs text-gray-600 dark:text-gray-300">{{ t('dynamicQuota.v2LearningHint') }}</p>
      <p v-if="quota.growth_frozen" class="mt-2 text-xs text-amber-800 dark:text-amber-200">{{ t('dynamicQuota.growthFrozen') }}</p>
      <details class="mt-3 text-xs text-gray-500 dark:text-gray-400">
        <summary class="cursor-pointer font-medium">{{ t('dynamicQuota.details') }}</summary>
        <p class="my-2">{{ t('dynamicQuota.cycle') }} · #{{ quota.cycle }}</p>
        <p v-if="quota.last_change" class="mb-2 flex flex-wrap gap-x-2 tabular-nums">
          <span>{{ usd(quota.last_change.previous_usd) }} → {{ usd(quota.last_change.current_usd) }}</span>
          <span>{{ t(`dynamicQuota.changes.${changeReason}`, { percent: quota.last_change.node }) }}</span>
        </p>
        <dl class="space-y-1">
      <div class="flex flex-wrap justify-between gap-1"><dt>{{ t('dynamicQuota.started') }}</dt><dd>{{ date(quota.started_at) }}</dd></div>
      <div class="flex flex-wrap justify-between gap-1"><dt>{{ t('dynamicQuota.synced') }}</dt><dd>{{ date(quota.synced_at) }}</dd></div>
      <div v-if="quota.last_allocation_at" class="flex flex-wrap justify-between gap-1"><dt>{{ t('dynamicQuota.allocatedAt') }}</dt><dd>{{ date(quota.last_allocation_at) }}</dd></div>
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
import { formatDateTimeToMinute } from '@/utils/format'
import HelpTooltip from '@/components/common/HelpTooltip.vue'

const props = withDefaults(defineProps<{ quota: DynamicSubscriptionQuota; compact?: boolean }>(), { compact: false })
const { t, locale } = useI18n()
const resetTime = computed(() => formatDateTimeToMinute(props.quota.expected_reset_at, locale.value))
const changeReason = computed(() => ['initial', 'bounds', 'upstream_node', 'reset'].includes(props.quota.last_change?.reason || '') ? props.quota.last_change!.reason : 'bounds')
const statuses = ['active', 'learning', 'confirming', 'settling', 'reset_unconfirmed', 'identity_changed', 'quota_unavailable', 'invalid_billing_rate', 'upstream_reserve', 'disabled', 'activation_pending']
const canSpend = computed(() => ['active', 'learning'].includes(props.quota.status))
const knownStatus = computed(() => {
  if (canSpend.value && props.quota.remaining_usd <= 0) return props.quota.reserved_usd > 0 ? 'reserved' : 'exhausted'
  return canSpend.value && props.quota.growth_frozen ? 'growth_frozen' : (statuses.includes(props.quota.status) ? props.quota.status : 'quota_unavailable')
})
const available = computed(() => canSpend.value && props.quota.remaining_usd > 0)
const percentage = computed(() => props.quota.limit_usd > 0 ? Math.min(100, Math.max(0, Math.round(props.quota.used_usd / props.quota.limit_usd * 100))) : 0)
const usd = (value: number) => Number.isFinite(value) ? new Intl.NumberFormat(undefined, { style: 'currency', currency: 'USD' }).format(value) : '—'
const date = (value?: string) => value ? formatDateTimeToMinute(value) : '—'
</script>
