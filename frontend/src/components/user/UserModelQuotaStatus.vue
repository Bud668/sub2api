<template>
  <section v-if="policy?.enabled || error" class="card p-5" :aria-label="t('admin.users.modelPolicy.title')">
    <div class="mb-3 flex justify-between gap-3"><h2 class="font-semibold">{{ t('admin.users.modelPolicy.title') }}</h2><button class="text-sm text-primary-600" type="button" :disabled="loading" @click="load">{{ t('common.refresh') }}</button></div>
    <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
    <p v-else-if="policy && !policy.rules.length" class="text-sm text-gray-500">{{ t('admin.users.modelPolicy.emptyHint') }}</p>
    <div v-else-if="policy" class="space-y-3">
      <div v-for="rule in policy.rules" :key="rule.model" class="flex flex-wrap justify-between gap-2 border-b pb-2 text-sm dark:border-dark-700">
        <span class="font-mono">{{ rule.model }}</span>
        <span v-if="rule.mode === 'unlimited'">{{ t('admin.users.modelPolicy.unlimited') }}</span>
        <span v-else-if="rule.mode === 'deny'" class="text-red-600">{{ t('admin.users.modelPolicy.deny') }}</span>
        <div v-else class="text-right">
          <div>{{ modelWindow(policy, rule.model)?.used ?? 0 }} / {{ rule.request_limit }} · {{ rule.window_mode === 'daily' ? t('admin.users.modelPolicy.dailyReset') : t('admin.users.modelPolicy.period', { hours: rule.window_hours }) }}</div>
          <div class="text-xs text-gray-500">{{ t('admin.users.modelPolicy.remaining') }} {{ Math.max(0, rule.request_limit - (modelWindow(policy, rule.model)?.used ?? 0)) }}</div>
          <div v-if="modelWindow(policy, rule.model)?.resets_at" class="text-xs text-gray-500">{{ t('admin.users.modelPolicy.resetsAt') }} {{ new Date(modelWindow(policy, rule.model)!.resets_at!).toLocaleString() }}</div>
        </div>
      </div>
      <p class="text-xs text-gray-500">{{ t('admin.users.modelPolicy.scopeHint') }}</p>
    </div>
  </section>
</template>
<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { getMyModelPolicy, modelWindow, type ModelPolicy } from '@/api/modelPolicy'
const { t } = useI18n()
const policy = ref<ModelPolicy | null>(null), loading = ref(false), error = ref('')
async function load() {
  loading.value = true; error.value = ''
  try { policy.value = await getMyModelPolicy() }
  catch { error.value = t('admin.users.modelPolicy.failed') }
  finally { loading.value = false }
}
onMounted(load)
</script>
