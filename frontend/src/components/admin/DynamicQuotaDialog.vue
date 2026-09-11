<template>
  <BaseDialog :show="true" :title="t('dynamicQuota.title')" width="wide" :close-on-escape="!saving" :show-close-button="!saving" @close="close">
    <p class="mb-4 break-all text-sm font-medium">{{ subscription.user?.email || `#${subscription.user_id}` }} · {{ subscription.group?.name }}</p>
    <p v-if="loading" role="status">{{ t('common.loading') }}</p>
    <form v-else-if="status" id="dynamic-quota-form" class="space-y-4" @submit.prevent="save">
      <fieldset :disabled="saving" class="space-y-4">
        <label class="flex cursor-pointer items-center gap-3 rounded-xl border border-gray-200 p-3 dark:border-dark-600">
          <input v-model="form.enabled" type="checkbox" class="h-5 w-5 rounded" data-testid="dynamic-enable" @change="saved = false" />
          <span class="font-semibold">{{ t('dynamicQuota.enable') }}</span>
        </label>
        <p class="text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('dynamicQuota.optInHint') }}</p>
        <div>
          <label for="dynamic-source" class="input-label">{{ t('dynamicQuota.source') }}</label>
          <select v-if="form.revision === 0" id="dynamic-source" v-model.number="form.account_id" class="input" required @change="saved = false">
            <option :value="0" disabled>{{ t('dynamicQuota.chooseSource') }}</option>
            <option v-for="source in status.sources" :key="source.id" :value="source.id">#{{ source.id }} · {{ source.name }}</option>
            <option v-if="form.account_id && !status.sources.some(source => source.id === form.account_id)" :value="form.account_id">#{{ form.account_id }} · {{ t('dynamicQuota.sourceUnavailable') }}</option>
          </select>
          <output v-else id="dynamic-source" class="block rounded-lg bg-gray-50 p-3 text-sm dark:bg-dark-800">#{{ form.account_id }} · {{ status.sources.find(source => source.id === form.account_id)?.name || t('dynamicQuota.sourceUnavailable') }}</output>
          <p class="input-hint">{{ status.sources.length ? t('dynamicQuota.bindingHint') : t('dynamicQuota.noSources') }}</p>
        </div>
        <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <div><label for="dynamic-cap" class="input-label">{{ t('dynamicQuota.cap') }}</label><input id="dynamic-cap" v-model.number="form.max_limit_usd" type="number" min="0.01" max="1000000000" step="0.01" required class="input" @input="saved = false" /></div>
          <div>
            <div class="flex items-center gap-1">
              <label for="dynamic-floor" class="input-label">{{ t('dynamicQuota.floor') }}</label>
              <HelpTooltip trigger="click" :content="t('dynamicQuota.floorHint')"><template #trigger><button type="button" class="mb-1 rounded px-1 text-gray-500 focus-visible:ring-2 focus-visible:ring-primary-500" :aria-label="t('dynamicQuota.floorHelp')" data-testid="dynamic-floor-help">ⓘ</button></template></HelpTooltip>
            </div>
            <input id="dynamic-floor" v-model.number="form.floor_limit_usd" type="number" min="0.01" :max="form.max_limit_usd" step="0.01" required class="input" :aria-invalid="floorError || undefined" aria-describedby="dynamic-floor-error" @input="saved = false; floorError = false" />
            <p id="dynamic-floor-error" class="min-h-5 text-xs text-red-600" aria-live="polite">{{ floorError ? t('dynamicQuota.floorInvalid') : '' }}</p>
          </div>
          <div><label for="dynamic-weight" class="input-label">{{ t('dynamicQuota.weight') }}</label><input id="dynamic-weight" v-model.number="form.weight" type="number" min="0.0001" max="1000" step="0.0001" required class="input" @input="saved = false" /></div>
        </div>
        <p class="text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('dynamicQuota.v2AllocationHint') }}</p>
        <details class="rounded-lg bg-gray-50 p-3 text-xs leading-relaxed dark:bg-dark-800"><summary class="cursor-pointer font-medium">{{ t('dynamicQuota.rules') }}</summary><p class="mt-2">{{ t('dynamicQuota.nativeProtectionHint') }}</p></details>
      </fieldset>
      <p v-if="status.policy.activation_pending" role="status" class="rounded-lg bg-primary-50 p-3 text-sm dark:bg-primary-900/20">{{ t('dynamicQuota.pendingActivation') }}</p>
      <p v-if="status.policy.allocation_budget_conflict" class="rounded-lg bg-amber-50 p-3 text-sm dark:bg-amber-900/20">{{ t('dynamicQuota.budgetConflict') }}</p>
      <DynamicQuotaCard v-if="status.policy.enabled" :quota="status.policy" />
      <p class="text-xs text-gray-500">{{ t('dynamicQuota.accountingIndependent') }}</p>
    </form>
    <div class="mt-4 min-h-10 text-sm" aria-live="polite" aria-atomic="true">
      <p v-if="error" role="alert" class="text-red-600 dark:text-red-400">{{ error }}</p>
      <p v-else-if="saved" role="status" class="rounded-lg bg-green-50 p-3 font-medium text-green-700 dark:bg-green-900/20 dark:text-green-300">✓ {{ t(status?.policy.activation_pending ? 'dynamicQuota.pendingActivation' : 'dynamicQuota.saved') }}</p>
    </div>
    <template #footer>
      <div class="flex justify-end gap-3">
        <button type="button" class="btn btn-secondary" :disabled="saving || loading" data-testid="dynamic-refresh" @click="refreshStatus">{{ t('common.refresh') }}</button>
        <button type="button" class="btn btn-secondary" :disabled="saving" @click="close">{{ t('common.close') }}</button>
        <button type="submit" form="dynamic-quota-form" class="btn btn-primary" :disabled="loading || saving || !status || !form.account_id" data-testid="dynamic-save">
          <Icon v-if="saving" name="refresh" size="sm" class="mr-2 animate-spin" />{{ saving ? t('common.saving') : t('common.save') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { DynamicQuotaAdminStatus, DynamicQuotaInput, DynamicSubscriptionQuota, UserSubscription } from '@/types'
import { extractApiErrorCode } from '@/utils/apiError'
import BaseDialog from '@/components/common/BaseDialog.vue'
import DynamicQuotaCard from '@/components/common/DynamicQuotaCard.vue'
import HelpTooltip from '@/components/common/HelpTooltip.vue'
import Icon from '@/components/icons/Icon.vue'

const props = defineProps<{ subscription: UserSubscription }>()
const emit = defineEmits<{ close: []; saved: [] }>()
const { t } = useI18n()
const loading = ref(true)
const saving = ref(false)
const saved = ref(false)
const error = ref('')
const status = ref<DynamicQuotaAdminStatus>()
const floorError = ref(false)
const form = reactive<DynamicQuotaInput>({ enabled: false, revision: 0, account_id: 0, weight: 1, max_limit_usd: 0, floor_limit_usd: null })
const policyInput = (p: DynamicSubscriptionQuota): DynamicQuotaInput => ({
  enabled: p.requested_enabled ?? p.enabled, revision: p.revision, account_id: p.account_id || 0,
  weight: p.weight, max_limit_usd: p.max_limit_usd, floor_limit_usd: p.floor_limit_usd ?? null
})
const apply = (result: DynamicQuotaAdminStatus) => {
  status.value = result
  Object.assign(form, policyInput(result.policy))
}
const errorMessage = (err: unknown) => {
  const code = extractApiErrorCode(err)
  if (code === 'DYNAMIC_QUOTA_CHANGED') return t('dynamicQuota.conflict')
  if (code === 'DYNAMIC_QUOTA_BINDING_CONFLICT') return t('dynamicQuota.bindingError')
  if (code === 'DYNAMIC_QUOTA_UNAVAILABLE') return t('dynamicQuota.unavailable')
  if (code === 'INVALID_DYNAMIC_QUOTA_PROTECTION') return t('dynamicQuota.floorInvalid')
  return t('dynamicQuota.failed')
}
const close = () => { if (!saving.value) emit('close') }
onMounted(async () => {
  try { apply(await adminAPI.subscriptions.getDynamicQuota(props.subscription.id)) }
  catch (err) { error.value = errorMessage(err) }
  finally { loading.value = false }
})
const save = async () => {
  if (saving.value) return
  floorError.value = (typeof form.floor_limit_usd !== 'number' || !Number.isFinite(form.floor_limit_usd) || form.floor_limit_usd <= 0 || form.floor_limit_usd > form.max_limit_usd)
  if (floorError.value) return
  saving.value = true; saved.value = false; error.value = ''
  try {
    apply(await adminAPI.subscriptions.saveDynamicQuota(props.subscription.id, { ...form }))
    saved.value = true
    emit('saved') // Keep the dialog and server-returned revision visible for review.
  } catch (err) {
    const code = extractApiErrorCode(err)
    const httpStatus = (err as { status?: number } | null)?.status
    if (!code || (httpStatus != null && httpStatus >= 500) || ['0', 'ECONNABORTED', 'ETIMEDOUT', 'ERR_NETWORK'].includes(code)) {
      error.value = t('dynamicQuota.saveUnconfirmed')
      try {
        const result = await adminAPI.subscriptions.getDynamicQuota(props.subscription.id)
        const actual = policyInput(result.policy)
        if (actual.revision === form.revision + 1 && (Object.keys(form) as (keyof DynamicQuotaInput)[]).every(key => key === 'revision' || actual[key] === form[key])) {
          apply(result); saved.value = true; error.value = ''; emit('saved')
        }
      } catch { /* Keep the unconfirmed state and edits; never retry a monetary/config write blindly. */ }
    } else { error.value = errorMessage(err) }
  }
  finally { saving.value = false }
}
const refreshStatus = async () => {
  if (saving.value) return
  saving.value = true; saved.value = false; error.value = ''
  try {
    const result = await adminAPI.subscriptions.getDynamicQuota(props.subscription.id)
    if (!status.value) apply(result)
    else status.value = result // Refresh status without replacing unsaved inputs/revision.
  }
  catch (err) { error.value = errorMessage(err) }
  finally { saving.value = false }
}
</script>
