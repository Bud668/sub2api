<template>
  <BaseDialog :show="subscription !== null" :title="t('dynamicQuota.setDebugWeeklyLimit')" width="normal" @close="!busy && emit('close')">
    <form id="admin-debug-quota-form" class="space-y-4" @submit.prevent="save">
      <p class="break-all text-sm font-medium text-gray-800 dark:text-gray-200">{{ subscription?.user?.email }}</p>
      <p class="text-sm leading-6 text-gray-600 dark:text-gray-300">{{ t('dynamicQuota.debugQuotaHint') }}</p>
      <div>
        <label for="admin-debug-weekly-limit" class="input-label">{{ t('dynamicQuota.debugWeeklyLimit') }} (USD)</label>
        <input id="admin-debug-weekly-limit" v-model="amount" type="number" min="0" max="1000000000" step="0.01" required class="input" :disabled="busy || !policy" @input="saved = false" />
        <p class="mt-2 text-xs text-gray-600 dark:text-gray-300">{{ t('dynamicQuota.debugQuotaZero') }}</p>
      </div>
      <p v-if="loading" role="status" class="text-sm text-gray-600 dark:text-gray-300">{{ t('common.loading') }}</p>
      <div v-if="error" role="alert" class="rounded-lg bg-red-50 p-3 text-sm text-red-800 dark:bg-red-900/30 dark:text-red-200">
        {{ error }}
        <button type="button" class="ml-2 font-semibold underline" :disabled="busy" @click="reload">{{ t('common.refresh') }}</button>
      </div>
      <p v-if="saved" role="status" class="rounded-lg bg-green-50 p-3 text-sm font-medium text-green-800 dark:bg-green-900/30 dark:text-green-200">{{ t('dynamicQuota.debugQuotaSaved') }}</p>
    </form>
    <template #footer>
      <button type="button" class="btn btn-secondary" :disabled="busy" @click="emit('close')">{{ t('common.close') }}</button>
      <button type="submit" form="admin-debug-quota-form" class="btn btn-primary" :disabled="busy || !policy">{{ saving ? t('common.saving') : t('common.save') }}</button>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import subscriptionsAPI from '@/api/admin/subscriptions'
import type { AdminDebugQuota, UserSubscription } from '@/types'

const props = defineProps<{ subscription: UserSubscription | null }>()
const emit = defineEmits<{ close: []; saved: [] }>()
const { t } = useI18n()
const policy = ref<AdminDebugQuota | null>(null)
const amount = ref<number | string>('')
const loading = ref(false)
const saving = ref(false)
const saved = ref(false)
const error = ref('')
const busy = computed(() => loading.value || saving.value)

async function reload() {
  const id = props.subscription?.id
  if (!id) return
  loading.value = true
  saved.value = false
  error.value = ''
  policy.value = null
  try {
    const sub = await subscriptionsAPI.getById(id)
    if (props.subscription?.id !== id) return
    if (!sub.admin_debug || !sub.admin_debug_quota) throw new Error('Unavailable debug quota')
    policy.value = sub.admin_debug_quota
    amount.value = policy.value.weekly_limit_usd
  } catch {
    if (props.subscription?.id === id) error.value = t('dynamicQuota.debugQuotaFailed')
  } finally {
    if (props.subscription?.id === id) loading.value = false
  }
}

async function save() {
  if (!props.subscription || !policy.value || busy.value) return
  const limit = Number(amount.value)
  if (amount.value === '' || !Number.isFinite(limit) || limit < 0 || limit > 1e9) return
  saving.value = true
  saved.value = false
  error.value = ''
  try {
    const sub = await subscriptionsAPI.saveAdminDebugQuota(props.subscription.id, { revision: policy.value.revision, weekly_limit_usd: limit })
    if (!sub.admin_debug_quota || sub.admin_debug_quota.weekly_limit_usd !== limit) throw new Error('Save not confirmed')
    policy.value = sub.admin_debug_quota
    amount.value = policy.value.weekly_limit_usd
    saved.value = true
    emit('saved')
  } catch {
    error.value = t('dynamicQuota.debugQuotaFailed')
  } finally {
    saving.value = false
  }
}

watch(() => props.subscription?.id, () => { policy.value = null; error.value = ''; saved.value = false; void reload() }, { immediate: true })
</script>
