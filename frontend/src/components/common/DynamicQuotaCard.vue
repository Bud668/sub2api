<template>
  <section class="rounded-xl border border-primary-200 bg-primary-50/50 p-3 text-sm dark:border-primary-900 dark:bg-primary-900/10" :aria-label="t('dynamicQuota.title')">
    <div class="mb-3 flex flex-wrap items-center justify-between gap-2">
      <span class="font-semibold text-gray-900 dark:text-white">{{ t('dynamicQuota.cycle') }} · #{{ quota.cycle }}</span>
      <span class="rounded-full px-2 py-0.5 text-xs font-medium" :class="available ? 'bg-primary-100 text-primary-700 dark:bg-primary-900 dark:text-primary-200' : 'bg-amber-100 text-amber-800 dark:bg-amber-900 dark:text-amber-100'">
        {{ t(`dynamicQuota.statuses.${knownStatus}`) }}
      </span>
    </div>
    <dl class="grid grid-cols-3 gap-3 tabular-nums">
      <div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('dynamicQuota.used') }}</dt><dd class="mt-1 font-semibold">{{ usd(quota.used_usd) }}</dd></div>
      <div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('dynamicQuota.limit') }}</dt><dd class="mt-1 font-semibold">{{ usd(quota.limit_usd) }}</dd></div>
      <div><dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('dynamicQuota.remaining') }}</dt><dd class="mt-1 font-semibold text-primary-700 dark:text-primary-300">{{ usd(quota.remaining_usd) }}</dd></div>
    </dl>
    <div class="my-3 h-1.5 overflow-hidden rounded-full bg-gray-200 dark:bg-dark-600" role="progressbar" :aria-label="t('dynamicQuota.used')" :aria-valuenow="percentage" aria-valuemin="0" aria-valuemax="100">
      <div class="h-full rounded-full bg-primary-500" :style="{ width: `${percentage}%` }" />
    </div>
    <dl class="space-y-1 text-xs text-gray-500 dark:text-gray-400">
      <div v-if="quota.reserved_usd > 0" class="flex justify-between gap-2"><dt>{{ t('dynamicQuota.reserved') }}</dt><dd>{{ usd(quota.reserved_usd) }}</dd></div>
      <div class="flex flex-wrap justify-between gap-1"><dt>{{ t('dynamicQuota.synced') }}</dt><dd>{{ date(quota.synced_at) }}</dd></div>
      <div class="flex flex-wrap justify-between gap-1"><dt>{{ t('dynamicQuota.confirmed') }}</dt><dd>{{ quota.confirmed_at ? date(quota.confirmed_at) : t('dynamicQuota.notReset') }}</dd></div>
      <div v-if="quota.expected_reset_at" class="flex flex-wrap justify-between gap-1"><dt>{{ t('dynamicQuota.expected') }}</dt><dd>{{ date(quota.expected_reset_at) }}</dd></div>
    </dl>
    <p v-if="quota.status === 'learning'" class="mt-2 text-xs text-gray-600 dark:text-gray-300">{{ t('dynamicQuota.learningHint') }}</p>
    <p v-if="quota.growth_frozen" class="mt-2 text-xs text-amber-800 dark:text-amber-200">{{ t('dynamicQuota.growthFrozen') }}</p>
    <p class="mt-2 text-xs text-gray-500 dark:text-gray-400">{{ t('dynamicQuota.cycleHint') }}</p>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { DynamicSubscriptionQuota } from '@/types'
import { formatDateTimeToMinute } from '@/utils/format'

const props = defineProps<{ quota: DynamicSubscriptionQuota }>()
const { t } = useI18n()
const statuses = ['active', 'learning', 'confirming', 'settling', 'reset_unconfirmed', 'identity_changed', 'quota_unavailable', 'quota_paused', 'invalid_billing_rate', 'upstream_reserve', 'disabled']
const knownStatus = computed(() => statuses.includes(props.quota.status) ? props.quota.status : 'quota_unavailable')
const available = computed(() => ['active', 'learning'].includes(props.quota.status))
const percentage = computed(() => props.quota.limit_usd > 0 ? Math.min(100, Math.max(0, Math.round(props.quota.used_usd / props.quota.limit_usd * 100))) : 0)
const usd = (value: number) => `$${Number.isFinite(value) ? value.toFixed(2) : '—'}`
const date = (value?: string) => value ? formatDateTimeToMinute(value) : '—'
</script>
