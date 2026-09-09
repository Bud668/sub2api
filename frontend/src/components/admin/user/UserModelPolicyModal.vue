<template>
  <BaseDialog :show="show" :title="t('admin.users.modelPolicy.title')" width="wide" @close="close">
    <div v-if="user" class="space-y-4">
      <p class="text-sm text-gray-600 dark:text-gray-300">{{ user.email }}</p>
      <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
      <p v-if="loading" role="status">{{ t('common.loading') }}</p>
      <template v-else-if="policy">
        <p class="rounded-lg bg-blue-50 p-3 text-sm text-blue-800 dark:bg-blue-950 dark:text-blue-200">
          {{ t(policy.enabled ? 'admin.users.modelPolicy.activeHint' : 'admin.users.modelPolicy.draftHint') }}
        </p>
        <p class="text-xs text-gray-500">{{ t('admin.users.modelPolicy.scopeHint') }}</p>
        <form data-testid="bulk-model-form" @submit.prevent="addSelectedRules">
          <fieldset :disabled="busy" class="rounded-lg border border-gray-200 p-3 dark:border-dark-700">
            <legend class="px-1 text-sm font-medium">{{ t('admin.users.modelPolicy.bulkAdd') }}</legend>
            <p class="mb-3 text-xs text-gray-500">{{ t('admin.users.modelPolicy.bulkHint') }}</p>
            <label class="mb-1 block text-sm font-medium" for="bulk-model-search">{{ t('admin.users.modelPolicy.chooseModels') }}</label>
            <input id="bulk-model-search" v-model="modelSearch" data-testid="bulk-model-search" type="search" class="input w-full" :placeholder="t('admin.users.modelPolicy.searchModels')" @keydown.enter.prevent />
            <div class="my-2 flex flex-col items-start gap-2 text-xs sm:flex-row sm:items-center sm:justify-between">
              <span data-testid="selection-count" role="status" class="tabular-nums text-gray-600 dark:text-gray-300">{{ t('admin.users.modelPolicy.selectionCount', { selected: selectedModels.length, visible: visibleModels.length }) }}</span>
              <div class="flex gap-3">
                <button data-testid="select-visible-models" type="button" class="text-primary-600 disabled:opacity-40 dark:text-primary-400" :disabled="!visibleModels.length" @click="selectVisibleModels">{{ t('admin.users.modelPolicy.selectResults') }}</button>
                <button data-testid="clear-model-selection" type="button" class="text-gray-600 disabled:opacity-40 dark:text-gray-300" :disabled="!selectedModels.length" @click="selectedModels = []">{{ t('admin.users.modelPolicy.clearSelection') }}</button>
              </div>
            </div>
            <div data-testid="model-checklist" class="mb-4 h-56 overflow-y-auto overscroll-contain rounded-lg border border-gray-200 p-2 dark:border-dark-700" style="scrollbar-gutter: stable">
              <div class="grid grid-cols-1 gap-1 sm:grid-cols-2">
                <label v-for="model in visibleModels" :key="model" class="flex min-w-0 cursor-pointer items-center gap-2 rounded-md px-2 py-2 text-sm hover:bg-gray-50 focus-within:ring-2 focus-within:ring-primary-500 dark:hover:bg-dark-700" :class="{ 'bg-primary-50 dark:bg-primary-950': selectedModels.includes(model) }">
                  <input v-model="selectedModels" type="checkbox" :value="model" class="h-4 w-4 shrink-0 accent-primary-600" />
                  <span class="truncate" :title="model">{{ model }}</span>
                </label>
              </div>
              <p v-if="!visibleModels.length" class="py-8 text-center text-sm text-gray-500">{{ t('admin.users.modelPolicy.noMatchingModels') }}</p>
            </div>
            <p class="mb-2 text-sm font-medium">{{ t('admin.users.modelPolicy.configureBatch') }}</p>
            <div class="flex flex-wrap items-end gap-3">
              <label class="text-sm">{{ t('admin.users.modelPolicy.permission') }}
                <select v-model="batchRule.mode" class="input mt-1" data-testid="bulk-mode">
                  <option value="limited">{{ t('admin.users.modelPolicy.limited') }}</option>
                  <option value="deny">{{ t('admin.users.modelPolicy.deny') }}</option>
                  <option value="unlimited">{{ t('admin.users.modelPolicy.unlimited') }}</option>
                </select>
              </label>
              <template v-if="batchRule.mode === 'limited'">
                <label class="text-sm">{{ t('admin.users.modelPolicy.requests') }}
                  <input v-model.number="batchRule.request_limit" data-testid="bulk-limit" class="input mt-1 w-28" type="number" min="1" max="1000000" step="1" required />
                </label>
                <label class="text-sm">{{ t('admin.users.modelPolicy.cycle') }}
                  <select v-model="batchRule.window_mode" data-testid="bulk-cycle" class="input mt-1">
                    <option value="daily">{{ t('admin.users.modelPolicy.daily') }}</option>
                    <option value="hours">{{ t('admin.users.modelPolicy.hourly') }}</option>
                  </select>
                </label>
                <label v-if="batchRule.window_mode === 'hours'" class="text-sm">{{ t('admin.users.modelPolicy.hours') }}
                  <input v-model.number="batchRule.window_hours" data-testid="bulk-hours" class="input mt-1 w-24" type="number" min="1" max="720" step="1" required />
                </label>
                <span v-else class="text-xs text-gray-500">{{ t('admin.users.modelPolicy.dailyReset') }}</span>
              </template>
            </div>
            <button type="submit" class="btn btn-secondary mt-3 w-full" :disabled="!selectedModels.length">{{ t('admin.users.modelPolicy.addSelected', { count: selectedModels.length }) }}</button>
          </fieldset>
        </form>
        <form id="user-model-policy-form" @submit.prevent="requestSave">
          <h4 class="mb-2 text-sm font-medium">{{ t('admin.users.modelPolicy.addedRules', { count: rules.length }) }}</h4>
          <div class="overflow-x-auto">
            <table class="w-full text-sm">
              <thead><tr class="border-b text-left">
                <th class="p-2">{{ t('admin.users.modelPolicy.model') }}</th>
                <th class="p-2">{{ t('admin.users.modelPolicy.permission') }}</th>
                <th class="p-2">{{ t('admin.users.modelPolicy.requests') }}</th>
                <th class="p-2">{{ t('admin.users.modelPolicy.cycle') }}</th>
                <th class="p-2">{{ t('admin.users.modelPolicy.usage') }}</th>
                <th class="p-2"><span class="sr-only">{{ t('common.actions') }}</span></th>
              </tr></thead>
              <tbody><tr v-for="(rule, index) in rules" :key="index" class="border-b border-gray-100 dark:border-dark-700">
                <td class="p-2">
                  <Select v-model="rule.model" :options="modelOptions(rule)" :aria-label="t('admin.users.modelPolicy.model')" class="min-w-48" :searchable="true" :creatable="false" :disabled="busy" :placeholder="t('admin.users.modelPolicy.selectModel')" />
                  <p v-if="rule.model && !candidates.includes(rule.model)" class="mt-1 text-xs text-amber-700">{{ t('admin.users.modelPolicy.unlistedModel') }}</p>
                </td>
                <td class="p-2"><select v-model="rule.mode" :aria-label="t('admin.users.modelPolicy.permission')" class="input min-w-28" :disabled="busy">
                  <option value="deny">{{ t('admin.users.modelPolicy.deny') }}</option>
                  <option value="unlimited">{{ t('admin.users.modelPolicy.unlimited') }}</option>
                  <option value="limited">{{ t('admin.users.modelPolicy.limited') }}</option>
                </select></td>
                <td class="p-2"><input v-if="rule.mode === 'limited'" v-model.number="rule.request_limit" :aria-label="t('admin.users.modelPolicy.requests')" class="input w-24" type="number" min="1" max="1000000" step="1" required :disabled="busy" /><span v-else>—</span></td>
                <td class="p-2"><template v-if="rule.mode === 'limited'">
                  <select v-model="rule.window_mode" :aria-label="t('admin.users.modelPolicy.cycle')" class="input min-w-32" :disabled="busy">
                    <option value="daily">{{ t('admin.users.modelPolicy.daily') }}</option>
                    <option value="hours">{{ t('admin.users.modelPolicy.hourly') }}</option>
                  </select>
                  <input v-if="rule.window_mode === 'hours'" v-model.number="rule.window_hours" :aria-label="t('admin.users.modelPolicy.hours')" class="input mt-1 w-20" type="number" min="1" max="720" step="1" required :disabled="busy" />
                  <p v-else class="mt-1 text-xs text-gray-500">{{ t('admin.users.modelPolicy.dailyReset') }}</p>
                </template><span v-else>—</span></td>
                <td class="p-2 text-xs"><template v-if="rule.mode === 'limited'">
                  {{ modelWindow(policy, rule.model)?.used ?? 0 }} / {{ rule.request_limit || '—' }}
                  <div v-if="modelWindow(policy, rule.model)?.resets_at" class="mt-1 whitespace-nowrap">{{ formatTime(modelWindow(policy, rule.model)?.resets_at) }}</div>
                  <button v-if="policy.rules.some(r => r.model === rule.model && r.mode === 'limited')" class="mt-1 text-primary-600 underline" type="button" :disabled="busy" @click="confirmation = { kind: 'reset', model: rule.model }">{{ t('admin.users.modelPolicy.reset') }}</button>
                </template><span v-else>—</span></td>
                <td class="p-2"><button type="button" class="text-red-600" :disabled="busy" @click="rules.splice(index, 1)">{{ t('common.delete') }}</button></td>
              </tr></tbody>
            </table>
          </div>
          <p v-if="!rules.length" class="py-4 text-sm text-gray-500">{{ t('admin.users.modelPolicy.emptyHint') }}</p>
          <p v-if="!candidates.length" class="text-sm text-amber-700">{{ t('admin.users.modelPolicy.noCandidates') }}</p>
        </form>
        <button v-if="!policy.enabled" type="button" class="text-sm text-primary-600 underline" :disabled="busy" @click="confirmation = { kind: 'activate' }">{{ t('admin.users.modelPolicy.activate') }}</button>
      </template>
    </div>
    <template #footer><div class="flex gap-3 justify-end">
      <button class="btn btn-secondary" type="button" :disabled="busy" @click="close">{{ t('common.cancel') }}</button>
      <button class="btn btn-primary" form="user-model-policy-form" type="submit" :disabled="busy || loading || !policy || !!selectedModels.length">{{ t('common.save') }}</button>
    </div></template>
  </BaseDialog>
  <ConfirmDialog :show="!!confirmation" :title="t('admin.users.modelPolicy.title')" :message="confirmMessage" :danger="true" @cancel="confirmation = null" @confirm="confirmAction" />
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import Select from '@/components/common/Select.vue'
import type { AdminUser } from '@/types'
import { activateUserModelPolicies, getUserModelPolicy, getModelPolicyCandidates, saveUserModelPolicy, resetUserModelQuota, validModelRequestRules, modelWindow, type ModelPolicy, type ModelRequestRule } from '@/api/modelPolicy'

const props = defineProps<{ show: boolean; user: AdminUser | null }>()
const emit = defineEmits<{ close: []; success: [] }>()
const { t } = useI18n()
const policy = ref<ModelPolicy | null>(null)
const rules = ref<ModelRequestRule[]>([])
const candidates = ref<string[]>([])
const selectedModels = ref<string[]>([])
const modelSearch = ref('')
const batchRule = ref<Omit<ModelRequestRule, 'model'>>({ mode: 'limited', request_limit: 0, window_hours: 0, window_mode: 'daily' })
const addableModels = computed(() => candidates.value.filter(model => !rules.value.some(rule => rule.model === model)))
const visibleModels = computed(() => addableModels.value.filter(model => model.toLowerCase().includes(modelSearch.value.trim().toLowerCase())))
const selectVisibleModels = () => { selectedModels.value = [...new Set([...selectedModels.value, ...visibleModels.value])] }
watch(addableModels, models => { selectedModels.value = selectedModels.value.filter(model => models.includes(model)) })
const loading = ref(false), busy = ref(false), error = ref('')
const confirmation = ref<{ kind: 'save' | 'reset' | 'activate'; model?: string } | null>(null)
const confirmMessage = computed(() => confirmation.value?.kind === 'activate' ? t('admin.users.modelPolicy.activateConfirm') : confirmation.value?.kind === 'reset' ? t('admin.users.modelPolicy.resetConfirm', { model: confirmation.value.model }) : t('admin.users.modelPolicy.clearConfirm'))
const formatTime = (value?: string | null) => value ? `${t('admin.users.modelPolicy.resetsAt')} ${new Date(value).toLocaleString()}` : '—'
const close = () => { if (!busy.value) emit('close') }
const accept = (data: ModelPolicy) => { policy.value = data; rules.value = data.rules.map(r => ({ ...r, window_mode: r.window_mode ?? 'hours' })) }
function modelOptions(rule: ModelRequestRule) {
  const options = candidates.value.map(model => ({ value: model, label: model, disabled: rules.value.some(r => r !== rule && r.model === model) }))
  if (rule.model && !candidates.value.includes(rule.model) && policy.value?.rules.some(r => r.model === rule.model)) {
    options.push({ value: rule.model, label: rule.model, disabled: true })
  }
  return options
}

watch(() => [props.show, props.user?.id] as const, async ([show, id], _, onCleanup) => {
  const controller = new AbortController()
  onCleanup(() => controller.abort())
  policy.value = null; candidates.value = []; error.value = ''; confirmation.value = null
  selectedModels.value = []; modelSearch.value = ''; batchRule.value = { mode: 'limited', request_limit: 0, window_hours: 0, window_mode: 'daily' }
  if (!show || !id) return
  loading.value = true
  try {
    const [data, models] = await Promise.all([getUserModelPolicy(id, controller.signal), getModelPolicyCandidates()])
    if (!controller.signal.aborted) { candidates.value = models; accept(data) }
  }
  catch (e: any) { if (!controller.signal.aborted) error.value = e?.message || t('admin.users.modelPolicy.failed') }
  finally { if (!controller.signal.aborted) loading.value = false }
}, { immediate: true })

function addSelectedRules() {
  if (!policy.value || busy.value || !selectedModels.value.length) return
  const additions = selectedModels.value.map(model => ({ ...batchRule.value, model }))
  if (selectedModels.value.some(model => !addableModels.value.includes(model)) || !validModelRequestRules([...rules.value, ...additions])) {
    error.value = t('admin.users.modelPolicy.invalid'); return
  }
  rules.value.push(...additions)
  selectedModels.value = []; error.value = ''
}

function requestSave() {
  if (!policy.value || busy.value || loading.value) return
  if (selectedModels.value.length) return // The persistent batch instructions explain the disabled Save button.
  if (!validModelRequestRules(rules.value) || rules.value.some(r => !candidates.value.includes(r.model) && !policy.value?.rules.some(saved => saved.model === r.model))) { error.value = t('admin.users.modelPolicy.invalid'); return }
  if (!rules.value.length && policy.value.rules.length) { confirmation.value = { kind: 'save' }; return }
  void perform('save')
}
async function confirmAction() {
  const action = confirmation.value
  confirmation.value = null
  if (action) await perform(action.kind, action.model)
}
async function perform(kind: 'save' | 'reset' | 'activate', model?: string) {
  if (!props.user || !policy.value || busy.value) return
  const id = props.user.id
  busy.value = true; error.value = ''
  try {
    if (kind === 'activate') {
      await activateUserModelPolicies()
      // Keep unsaved edits; activation is never an implicit save.
      if (props.user?.id === id && policy.value) policy.value.enabled = true
    } else {
      const data = kind === 'reset' ? await resetUserModelQuota(id, model!) : await saveUserModelPolicy(id, policy.value.revision, rules.value)
      if (props.user?.id === id) {
        if (kind === 'reset' && policy.value) policy.value.windows = data.windows
        else accept(data)
      }
    }
    emit('success')
  } catch (e: any) { if (props.user?.id === id) error.value = e?.message || t('admin.users.modelPolicy.failed') }
  finally { busy.value = false }
}
</script>
