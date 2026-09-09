<template>
  <BaseDialog :show="true" :title="t('dynamicQuota.title')" width="normal" :close-on-escape="!saving" :show-close-button="!saving" @close="close">
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
          <select id="dynamic-source" v-model.number="form.account_id" class="input" required :disabled="form.revision > 0" @change="saved = false">
            <option :value="0" disabled>{{ t('dynamicQuota.chooseSource') }}</option>
            <option v-for="source in status.sources" :key="source.id" :value="source.id">#{{ source.id }} · {{ source.name }}</option>
            <option v-if="form.account_id && !status.sources.some(source => source.id === form.account_id)" :value="form.account_id">#{{ form.account_id }} · {{ t('dynamicQuota.sourceUnavailable') }}</option>
          </select>
          <p class="input-hint">{{ status.sources.length ? t('dynamicQuota.bindingHint') : t('dynamicQuota.noSources') }}</p>
        </div>
        <div class="grid grid-cols-2 gap-4">
          <div><label for="dynamic-weight" class="input-label">{{ t('dynamicQuota.weight') }}</label><input id="dynamic-weight" v-model.number="form.weight" type="number" min="0.0001" max="1000" step="0.0001" required class="input" @input="saved = false" /></div>
          <div><label for="dynamic-cap" class="input-label">{{ t('dynamicQuota.cap') }}</label><input id="dynamic-cap" v-model.number="form.max_limit_usd" type="number" min="0.01" max="1000000000" step="0.01" required class="input" @input="saved = false" /></div>
        </div>
        <div>
          <label for="dynamic-increase-threshold" class="input-label">{{ t('dynamicQuota.increaseThreshold') }}</label>
          <select id="dynamic-increase-threshold" v-model.number="form.increase_threshold_usd" class="input" @change="saved = false">
            <option :value="10">$10</option><option :value="5">$5</option>
          </select>
          <p class="input-hint">{{ t('dynamicQuota.thresholdHint') }}</p>
        </div>
        <p class="rounded-lg bg-gray-50 p-3 text-xs leading-relaxed text-gray-600 dark:bg-dark-800 dark:text-gray-300">{{ t('dynamicQuota.nativeProtectionHint') }}</p>
        <p class="text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('dynamicQuota.allocationHint') }}</p>
      </fieldset>
      <DynamicQuotaCard v-if="status.policy.enabled" :quota="status.policy" />
      <section v-if="status.policy.capacity_review?.manual_required" class="space-y-3 rounded-xl border border-amber-300 bg-amber-50 p-4 dark:border-amber-800 dark:bg-amber-950/30" data-testid="capacity-review">
        <h3 class="font-semibold">{{ t('dynamicQuota.capacityReviewTitle') }}</h3>
        <dl class="grid grid-cols-2 gap-3 tabular-nums">
          <div><dt class="text-xs">{{ t('dynamicQuota.trustedCapacity') }}</dt><dd class="mt-1 font-semibold">${{ (status.policy.capacity_estimate_usd || 0).toFixed(2) }}</dd></div>
          <div><dt class="text-xs">{{ t('dynamicQuota.proposedCapacity') }}</dt><dd class="mt-1 font-semibold">${{ status.policy.capacity_review.proposed_usd.toFixed(2) }}</dd></div>
        </dl>
        <p class="text-xs leading-relaxed">{{ t('dynamicQuota.capacityReviewHint') }}</p>
        <p v-if="!status.policy.capacity_approval_ready" class="text-xs">{{ t('dynamicQuota.capacityWaiting') }}</p>
        <label class="flex items-start gap-2 text-sm"><input v-model="capacityAcknowledged" type="checkbox" :disabled="saving || !status.policy.capacity_approval_ready" class="mt-1 h-4 w-4" data-testid="capacity-acknowledge" />{{ t('dynamicQuota.capacityAcknowledge') }}</label>
        <p v-if="hasUnsavedChanges" class="text-xs">{{ t('dynamicQuota.capacityUnsaved') }}</p>
        <div class="flex flex-wrap gap-2">
          <button type="button" class="btn btn-secondary" :disabled="saving" @click="refreshStatus">{{ t('dynamicQuota.refreshStatus') }}</button>
          <button type="button" class="btn btn-primary" :disabled="saving || !status.policy.enabled || !status.policy.capacity_approval_ready || !capacityAcknowledged || hasUnsavedChanges" data-testid="capacity-approve" @click="approveCapacity">{{ t('dynamicQuota.approveCapacity') }}</button>
        </div>
      </section>
      <p v-if="status.pending_requests || status.uncertain_requests" class="rounded-lg bg-amber-50 p-3 text-xs leading-relaxed text-amber-800 dark:bg-amber-900/20 dark:text-amber-200">
        {{ t('dynamicQuota.poolRequests') }} · {{ status.pending_requests || 0 }} / {{ status.uncertain_requests || 0 }}<br />{{ t('dynamicQuota.pending') }}
      </p>
    </form>
    <div class="mt-4 min-h-10 text-sm" aria-live="polite" aria-atomic="true">
      <p v-if="error" role="alert" class="text-red-600 dark:text-red-400">{{ error }}</p>
      <p v-else-if="saved" role="status" class="rounded-lg bg-green-50 p-3 font-medium text-green-700 dark:bg-green-900/20 dark:text-green-300">✓ {{ t(approved ? 'dynamicQuota.capacityApproved' : 'dynamicQuota.saved') }}</p>
    </div>
    <template #footer>
      <div class="flex justify-end gap-3">
        <button type="button" class="btn btn-secondary" :disabled="saving" @click="close">{{ t('common.close') }}</button>
        <button type="submit" form="dynamic-quota-form" class="btn btn-primary" :disabled="loading || saving || !status || !form.account_id" data-testid="dynamic-save">
          <Icon v-if="saving" name="refresh" size="sm" class="mr-2 animate-spin" />{{ saving ? t('common.saving') : t('common.save') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { DynamicQuotaAdminStatus, DynamicQuotaInput, UserSubscription } from '@/types'
import BaseDialog from '@/components/common/BaseDialog.vue'
import DynamicQuotaCard from '@/components/common/DynamicQuotaCard.vue'
import Icon from '@/components/icons/Icon.vue'

const props = defineProps<{ subscription: UserSubscription }>()
const emit = defineEmits<{ close: []; saved: [] }>()
const { t } = useI18n()
const loading = ref(true)
const saving = ref(false)
const saved = ref(false)
const error = ref('')
const status = ref<DynamicQuotaAdminStatus>()
const capacityAcknowledged = ref(false)
const approved = ref(false)
const form = reactive<DynamicQuotaInput>({ enabled: false, revision: 0, account_id: 0, weight: 1, max_limit_usd: 0, increase_threshold_usd: 10 })
const apply = (result: DynamicQuotaAdminStatus) => {
  status.value = result
  const p = result.policy
  Object.assign(form, { enabled: p.enabled, revision: p.revision, account_id: p.account_id || 0, weight: p.weight, max_limit_usd: p.max_limit_usd, increase_threshold_usd: p.increase_threshold_usd })
}
const hasUnsavedChanges = computed(() => {
  const p = status.value?.policy
  return !p || (Object.keys(form) as (keyof DynamicQuotaInput)[]).some(key => form[key] !== (p[key] ?? (key === 'account_id' ? 0 : undefined)))
})
const errorMessage = (err: unknown) => {
  const code = (err as { response?: { data?: { reason?: string } } })?.response?.data?.reason
  if (code === 'DYNAMIC_QUOTA_CHANGED') return t('dynamicQuota.conflict')
  if (code === 'DYNAMIC_QUOTA_REQUESTS_PENDING') return t('dynamicQuota.pending')
  if (code === 'DYNAMIC_QUOTA_BINDING_CONFLICT') return t('dynamicQuota.bindingError')
  if (code === 'DYNAMIC_QUOTA_UNAVAILABLE') return t('dynamicQuota.unavailable')
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
  saving.value = true; saved.value = false; approved.value = false; capacityAcknowledged.value = false; error.value = ''
  try {
    apply(await adminAPI.subscriptions.saveDynamicQuota(props.subscription.id, { ...form }))
    saved.value = true
    emit('saved') // Keep the dialog and server-returned revision visible for review.
  } catch (err) { error.value = errorMessage(err) }
  finally { saving.value = false }
}
const refreshStatus = async () => {
  if (saving.value) return
  saving.value = true; capacityAcknowledged.value = false; saved.value = false; error.value = ''
  try { status.value = await adminAPI.subscriptions.getDynamicQuota(props.subscription.id) }
  catch (err) { error.value = errorMessage(err) }
  finally { saving.value = false }
}
const approveCapacity = async () => {
  const p = status.value?.policy
  if (saving.value || !p?.enabled || !p.capacity_approval_ready || !p.capacity_review || !capacityAcknowledged.value || hasUnsavedChanges.value) return
  saving.value = true; saved.value = false; error.value = ''
  try {
    apply(await adminAPI.subscriptions.approveDynamicCapacity(props.subscription.id, p.capacity_review.id))
    approved.value = true; saved.value = true; capacityAcknowledged.value = false
    emit('saved')
  } catch (err) { capacityAcknowledged.value = false; error.value = errorMessage(err) }
  finally { saving.value = false }
}
</script>
