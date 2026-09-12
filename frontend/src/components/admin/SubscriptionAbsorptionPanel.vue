<template>
  <button
    type="button"
    class="btn btn-secondary w-full max-w-full bg-gray-50 px-3 text-left shadow-none dark:bg-dark-800 sm:w-auto"
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
  <BaseDialog :show="open" :title="t('dynamicQuota.absorption.title')" width="wide" @close="close">
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
    <div v-else-if="detailsError" role="alert" class="flex items-center gap-3 text-red-600 dark:text-red-400"><span>{{ t('dynamicQuota.absorption.failed') }}</span><button type="button" class="btn btn-secondary" @click="loadDetails">{{ t('common.refresh') }}</button></div>
    <p v-else-if="!report?.items.length" class="py-8 text-center text-gray-500">{{ t('common.noData') }}</p>
    <div v-else class="max-h-[55vh] space-y-3 overflow-y-auto" data-testid="absorption-details">
      <article v-for="row in report.items" :key="row.id" class="min-w-0 rounded-xl border border-gray-200 p-4 text-sm dark:border-dark-700">
        <div class="flex flex-wrap items-start justify-between gap-2">
          <div class="min-w-0"><p class="break-all font-semibold text-gray-900 dark:text-gray-100">{{ row.email || `#${row.user_id}` }}</p><p class="mt-1 break-words text-xs text-gray-600 dark:text-gray-400">{{ row.group_name }} · #{{ row.account_id }} {{ row.account_name }} · {{ row.cycle ? t('dynamicQuota.absorption.cycle', { n: row.cycle }) : t('dynamicQuota.absorption.baseline') }}</p></div>
          <span class="rounded-full bg-emerald-50 px-2 py-1 text-xs font-medium text-emerald-800 dark:bg-emerald-900/20 dark:text-emerald-200" data-testid="absorption-status">{{ t(row.reason === 'already_billed' ? 'dynamicQuota.absorption.resolvedBilled' : row.reason.startsWith('automatic_') ? 'dynamicQuota.absorption.automaticClosed' : 'dynamicQuota.absorption.resolvedCovered') }}</span>
        </div>
        <dl class="mt-3 grid min-w-0 gap-3 sm:grid-cols-2">
          <div class="min-w-0"><dt class="text-xs text-gray-600 dark:text-gray-400">{{ t('dynamicQuota.absorption.model') }}</dt><dd class="break-words">{{ row.model || '—' }}</dd></div>
          <div class="min-w-0 tabular-nums"><dt class="text-xs text-gray-600 dark:text-gray-400">{{ t('dynamicQuota.absorption.standardAmount') }}</dt><dd class="break-words font-medium">{{ row.known_standard_usd === null ? t('dynamicQuota.absorption.unknown') : formatCurrency(row.known_standard_usd) }}</dd></div>
        </dl>
        <p class="mt-3 text-xs text-gray-600 dark:text-gray-400">{{ t(reasonKey(row.reason)) }}</p>
        <details class="mt-2 text-xs text-gray-600 dark:text-gray-400">
          <summary class="cursor-pointer">{{ t('dynamicQuota.absorption.time') }}</summary>
          <p class="mt-2">{{ t('dynamicQuota.absorption.requested') }} {{ formatDateTimeToMinute(row.started_at) }}</p>
          <p>{{ t('dynamicQuota.absorption.processed') }} {{ formatDateTimeToMinute(row.absorbed_at) }}</p>
          <p>{{ t(row.closed_at ? 'dynamicQuota.absorption.archived' : 'dynamicQuota.absorption.current') }}</p>
          <p class="mt-1 break-all">{{ row.id }}</p>
          <p class="mt-1">{{ t('dynamicQuota.absorption.reference') }} {{ formatCurrency(row.reference_hold_usd) }}</p>
        </details>
      </article>
    </div>
    <template #footer>
      <div class="flex flex-wrap items-center justify-between gap-3">
        <div class="flex items-center gap-3 tabular-nums"><button type="button" class="btn btn-secondary" :disabled="detailsLoading || detailsError || page <= 1" @click="page--">{{ t('dynamicQuota.absorption.previous') }}</button><span>{{ page }} / {{ Math.max(1, report?.pages || 0) }}</span><button type="button" class="btn btn-secondary" :disabled="detailsLoading || detailsError || page >= (report?.pages || 0)" @click="page++">{{ t('common.next') }}</button></div>
        <button type="button" class="btn btn-secondary" @click="close">{{ t('common.close') }}</button>
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
const close = () => { open.value = false }
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
const reasonKey = (reason: string) => `dynamicQuota.absorption.${({ missing_evidence: 'missingEvidence', cycle_closed: 'cycleClosed', operator_decision: 'operatorDecision', already_billed: 'alreadyBilled', small_exception: 'smallException', automatic_unmetered: 'automaticUnmetered', automatic_invalid_receipt: 'automaticInvalidReceipt', automatic_receipt_mismatch: 'automaticInvalidReceipt' } as Record<string, string>)[reason] || 'missingEvidence'}`
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
