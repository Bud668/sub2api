<template>
  <button
    type="button"
    class="btn btn-secondary w-full max-w-full border-amber-200 bg-amber-50/70 px-3 text-left shadow-none hover:bg-amber-100/70 dark:border-amber-900/60 dark:bg-amber-900/20 dark:hover:bg-amber-900/30 sm:w-auto"
    data-testid="absorption-summary"
    :aria-busy="summaryLoading"
    :aria-expanded="open"
    aria-haspopup="dialog"
    :title="t('dynamicQuota.absorption.hint')"
    @click="open = true"
  >
    <span class="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1" aria-live="polite">
      <span class="whitespace-nowrap">{{ t('dynamicQuota.absorption.title') }}</span>
      <span v-if="summary" class="whitespace-nowrap tabular-nums text-gray-500 dark:text-gray-400">
        <span aria-hidden="true">· </span>{{ t('dynamicQuota.absorption.count', { n: summary.requests }) }}
      </span>
      <span v-if="summaryLoading" class="text-gray-500 dark:text-gray-400">{{ t('common.loading') }}</span>
      <span v-else-if="summaryError" class="text-red-600 dark:text-red-400">{{ t('dynamicQuota.absorption.failed') }}</span>
      <template v-else-if="summary">
        <span class="font-semibold tabular-nums text-gray-900 dark:text-gray-100">{{ amountText(summary) }}</span>
        <span v-if="summary.unknown_requests" class="rounded-md bg-amber-100/80 px-1.5 text-xs leading-5 tabular-nums text-amber-800 dark:bg-amber-900/50 dark:text-amber-200">
          {{ t('dynamicQuota.absorption.unknownCount', { n: summary.unknown_requests }) }}
        </span>
      </template>
    </span>
    <Icon name="chevronRight" size="sm" class="ml-auto shrink-0 text-amber-600 dark:text-amber-400" aria-hidden="true" />
  </button>
  <BaseDialog :show="open" :title="t('dynamicQuota.absorption.title')" width="extra-wide" @close="open = false">
    <div class="mb-4 flex flex-wrap items-center gap-3">
      <label class="flex items-center gap-2 text-sm">{{ t('dynamicQuota.absorption.scope') }}
        <select v-model="scope" class="input w-auto" data-testid="absorption-scope" @change="page = 1">
          <option value="current">{{ t('dynamicQuota.absorption.current') }}</option>
          <option value="history">{{ t('dynamicQuota.absorption.history') }}</option>
        </select>
      </label>
      <span v-if="report && !detailsLoading" class="text-sm tabular-nums">{{ t('dynamicQuota.absorption.count', { n: report.summary.requests }) }} · {{ amountText(report.summary) }} · {{ t('dynamicQuota.absorption.unknownCount', { n: report.summary.unknown_requests }) }}</span>
    </div>
    <p class="mb-3 text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('dynamicQuota.absorption.hint') }}</p>
    <p v-if="detailsLoading" role="status">{{ t('common.loading') }}</p>
    <div v-else-if="detailsError" role="alert" class="flex items-center gap-3 text-red-600"><span>{{ t('dynamicQuota.absorption.failed') }}</span><button type="button" class="btn btn-secondary" @click="loadDetails">{{ t('common.refresh') }}</button></div>
    <p v-else-if="!report?.items.length" class="py-8 text-center text-gray-500">{{ t('common.noData') }}</p>
    <div v-else class="max-h-[55vh] overflow-auto">
      <table class="w-full min-w-[760px] text-left text-sm" data-testid="absorption-details">
        <thead class="sticky top-0 bg-gray-50 dark:bg-dark-800"><tr><th class="p-3">{{ t('dynamicQuota.absorption.user') }}</th><th class="p-3">{{ t('dynamicQuota.absorption.source') }}</th><th class="p-3">{{ t('dynamicQuota.absorption.model') }}</th><th class="p-3">{{ t('dynamicQuota.absorption.amount') }}</th><th class="p-3">{{ t('dynamicQuota.absorption.reason') }}</th><th class="p-3">{{ t('dynamicQuota.absorption.time') }}</th></tr></thead>
        <tbody><tr v-for="row in report.items" :key="row.id" class="border-t border-gray-100 align-top dark:border-dark-700">
          <td class="max-w-60 break-words p-3"><span>{{ row.email || `#${row.user_id}` }}</span><div class="mt-1 text-xs text-gray-500">{{ row.group_name }}</div></td>
          <td class="p-3">#{{ row.account_id }} · {{ row.account_name }}<div class="mt-1 text-xs text-gray-500">{{ row.cycle ? t('dynamicQuota.absorption.cycle', { n: row.cycle }) : t('dynamicQuota.absorption.baseline') }}</div></td>
          <td class="max-w-48 break-words p-3">{{ row.model || '—' }}</td>
          <td class="whitespace-nowrap p-3 tabular-nums"><span>{{ row.known_standard_usd === null ? t('dynamicQuota.absorption.unknown') : formatCurrency(row.known_standard_usd) }}</span><div class="mt-1 text-xs text-gray-500">{{ t('dynamicQuota.absorption.reference') }} {{ formatCurrency(row.reference_hold_usd) }}</div></td>
          <td class="max-w-60 p-3">{{ t(reasonKey(row.reason)) }}<div class="mt-1 text-xs text-gray-500">{{ t(row.closed_at ? 'dynamicQuota.absorption.archived' : 'dynamicQuota.absorption.current') }}</div></td>
          <td class="whitespace-nowrap p-3 text-xs">{{ t('dynamicQuota.absorption.requested') }} {{ formatDateTimeToMinute(row.started_at) }}<div class="mt-1">{{ t('dynamicQuota.absorption.processed') }} {{ formatDateTimeToMinute(row.absorbed_at) }}</div><div v-if="row.closed_at" class="mt-1">{{ t('dynamicQuota.absorption.archived') }} {{ formatDateTimeToMinute(row.closed_at) }}</div><span class="mt-1 block max-w-52 whitespace-normal break-all text-gray-400">{{ row.id }}</span></td>
        </tr></tbody>
      </table>
    </div>
    <template #footer>
      <div class="flex flex-wrap items-center justify-between gap-3">
        <div class="flex items-center gap-3 tabular-nums"><button type="button" class="btn btn-secondary" :disabled="detailsLoading || detailsError || page <= 1" @click="page--">{{ t('dynamicQuota.absorption.previous') }}</button><span>{{ page }} / {{ Math.max(1, report?.pages || 0) }}</span><button type="button" class="btn btn-secondary" :disabled="detailsLoading || detailsError || page >= (report?.pages || 0)" @click="page++">{{ t('common.next') }}</button></div>
        <button type="button" class="btn btn-secondary" @click="open = false">{{ t('common.close') }}</button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { getAbsorbedUsage, type AbsorbedUsageFilters, type AbsorbedUsageReport, type AbsorbedUsageSummary } from '@/api/admin/subscriptions'
import { formatCurrency, formatDateTimeToMinute } from '@/utils/format'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'

const props = defineProps<{ filters: AbsorbedUsageFilters; refreshKey: number }>()
const { t } = useI18n()
const open = ref(false)
const scope = ref<'current' | 'history'>('current')
const page = ref(1)
const summary = ref<AbsorbedUsageSummary | null>(null)
const report = ref<AbsorbedUsageReport | null>(null)
const summaryLoading = ref(false)
const summaryError = ref(false)
const detailsLoading = ref(false)
const detailsError = ref(false)
let summaryController: AbortController | undefined
let detailsController: AbortController | undefined
const reasonKey = (reason: string) => `dynamicQuota.absorption.${({ missing_evidence: 'missingEvidence', cycle_closed: 'cycleClosed', operator_decision: 'operatorDecision', already_billed: 'alreadyBilled' } as Record<string, string>)[reason] || 'operatorDecision'}`
const amountText = (s: AbsorbedUsageSummary) => s.unknown_requests > 0 && s.known_requests === 0
  ? t('dynamicQuota.absorption.unknownTotal')
  : `${t(s.unknown_requests ? 'dynamicQuota.absorption.knownTotal' : 'dynamicQuota.absorption.total')} ${formatCurrency(s.known_standard_usd)}`

const loadSummary = async () => {
  summaryController?.abort()
  const controller = new AbortController()
  summaryController = controller
  summaryLoading.value = true
  summaryError.value = false
  summary.value = null
  try {
    const result = await getAbsorbedUsage({ ...props.filters, scope: 'current', summary_only: true }, controller.signal)
    if (summaryController === controller && !controller.signal.aborted) summary.value = result.summary
  } catch {
    if (summaryController === controller && !controller.signal.aborted) summaryError.value = true
  } finally {
    if (summaryController === controller) summaryLoading.value = false
  }
}
const loadDetails = async () => {
  detailsController?.abort()
  const controller = new AbortController()
  detailsController = controller
  detailsLoading.value = true
  detailsError.value = false
  report.value = null
  try {
    const result = await getAbsorbedUsage({ ...props.filters, scope: scope.value, page: page.value, page_size: 20 }, controller.signal)
    if (detailsController === controller && !controller.signal.aborted) report.value = result
  } catch {
    if (detailsController === controller && !controller.signal.aborted) detailsError.value = true
  } finally {
    if (detailsController === controller) detailsLoading.value = false
  }
}
watch(() => [props.filters, props.refreshKey], () => { page.value = 1; void loadSummary() }, { deep: true, immediate: true })
watch(() => [open.value, scope.value, page.value, props.filters, props.refreshKey], () => {
  if (open.value) void loadDetails()
  else { detailsController?.abort(); scope.value = 'current'; page.value = 1 }
}, { deep: true })
onUnmounted(() => { summaryController?.abort(); detailsController?.abort() })
</script>
