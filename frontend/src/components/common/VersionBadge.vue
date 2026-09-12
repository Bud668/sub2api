<template>
  <div ref="root" class="relative">
    <span v-if="!authStore.isAdmin" class="text-xs text-gray-600 dark:text-dark-300">v{{ version }}</span>
    <template v-else>
      <button
        data-testid="version-toggle"
        class="flex items-center gap-2 rounded-lg border px-2 py-1 text-xs font-semibold transition-colors focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-500"
        :class="attention ? 'border-amber-300 bg-amber-50 text-amber-800 dark:border-amber-700 dark:bg-amber-900/20 dark:text-amber-300' : 'border-gray-200 bg-gray-50 text-gray-700 dark:border-dark-600 dark:bg-dark-800 dark:text-dark-200'"
        :aria-expanded="open"
        aria-controls="version-panel"
        @click="toggle"
      >
        v{{ currentVersion || '…' }}
        <span v-if="attention" class="h-2 w-2 rounded-full bg-amber-500" />
      </button>
      <section
        v-if="open"
        id="version-panel"
        data-testid="version-panel"
        :aria-label="t('version.budTitle')"
        class="fixed left-4 top-24 z-50 mt-2 max-h-[80dvh] w-80 max-w-[calc(100vw-2rem)] overflow-y-auto whitespace-normal rounded-xl border border-gray-200 bg-white p-4 text-left text-sm shadow-xl dark:border-dark-600 dark:bg-dark-800 lg:absolute lg:left-0 lg:top-full"
      >
        <div class="mb-3 flex items-start justify-between gap-3">
          <div class="min-w-0">
            <h2 class="font-semibold text-gray-900 dark:text-white">{{ t('version.budTitle') }}</h2>
            <p class="mt-1 break-all text-xs text-gray-600 dark:text-dark-300">v{{ currentVersion }}</p>
          </div>
          <button class="btn btn-secondary !p-2" :aria-label="t('version.refresh')" :disabled="appStore.versionLoading" @click="refresh(true)">
            <Icon name="refresh" size="sm" :class="{ 'animate-spin': appStore.versionLoading }" />
          </button>
        </div>

        <div class="space-y-3">
          <p v-if="appStore.versionWarning" role="alert" class="text-xs text-amber-800 dark:text-amber-300">{{ t('version.checkWarning') }}</p>
          <p class="text-sm font-medium text-gray-800 dark:text-dark-100">
            {{ appStore.versionWarning || !appStore.versionLoaded ? t('version.budUnconfirmed') : appStore.hasUpdate ? t('version.budAvailable', { version: appStore.latestVersion }) : t('version.budCurrent') }}
          </p>
          <a v-if="appStore.releaseInfo?.html_url" :href="appStore.releaseInfo.html_url" target="_blank" rel="noopener noreferrer" class="inline-flex items-center gap-1 text-xs font-medium text-primary-700 hover:underline dark:text-primary-300">
            {{ t('version.viewChangelog') }} <Icon name="externalLink" size="xs" />
          </a>
          <button
            v-if="appStore.hasUpdate"
            data-testid="bud-update"
            class="btn btn-primary w-full"
            :disabled="busy || submitting || !appStore.canUpdate || !!appStore.versionWarning"
            @click="update"
          >
            <Icon v-if="busy || submitting" name="refresh" size="sm" class="animate-spin" />
            {{ busy || submitting ? t('version.updating') : t('version.installBud') }}
          </button>
          <p v-if="appStore.hasUpdate && !appStore.canUpdate" class="text-xs leading-5 text-amber-800 dark:text-amber-300">{{ t('version.installerRequired') }}</p>

          <div v-if="status.phase !== 'idle' || error || reconnecting" :role="failed || error ? 'alert' : 'status'"
            class="rounded-lg border p-3 text-xs leading-5"
            :class="failed || error ? 'border-red-200 bg-red-50 text-red-800 dark:border-red-800 dark:bg-red-900/20 dark:text-red-300' : 'border-blue-200 bg-blue-50 text-blue-900 dark:border-blue-800 dark:bg-blue-900/20 dark:text-blue-200'">
            <p class="font-semibold">{{ error || (reconnecting ? t('version.reconnecting') : phaseLabel) }}</p>
            <p v-if="status.version" class="break-all">v{{ status.version }}</p>
            <p v-if="busy">{{ t('version.independentUpdate') }}</p>
            <p v-if="failed">{{ t('version.reviewFailure') }}</p>
            <button v-if="status.phase === 'completed'" class="mt-2 font-semibold underline" @click="reload">{{ t('version.reloadPage') }}</button>
          </div>

          <div data-testid="official-update" class="border-t border-gray-200 pt-3 dark:border-dark-600">
            <div class="flex flex-wrap items-center justify-between gap-2">
              <h3 class="font-semibold text-gray-900 dark:text-white">{{ t('version.officialTitle') }}</h3>
              <span class="rounded border border-gray-200 px-1.5 py-0.5 text-[11px] text-gray-600 dark:border-dark-500 dark:text-dark-300">{{ t('version.noticeOnly') }}</span>
            </div>
            <p class="mt-2 text-xs text-gray-600 dark:text-dark-300">{{ t('version.officialBase', { version: appStore.officialUpdate?.base_version || '—' }) }}</p>
            <p v-if="appStore.officialUpdate?.warning" role="alert" class="mt-2 text-xs text-amber-800 dark:text-amber-300">{{ t('version.checkWarning') }}</p>
            <a v-if="appStore.officialUpdate?.html_url" :href="appStore.officialUpdate.html_url" target="_blank" rel="noopener noreferrer"
              class="mt-2 inline-flex items-center gap-1 text-xs font-semibold hover:underline"
              :class="appStore.officialUpdate.has_update ? 'text-amber-800 dark:text-amber-300' : 'text-primary-700 dark:text-primary-300'">
              {{ t(appStore.officialUpdate.has_update ? 'version.officialNew' : 'version.officialLatest', { version: appStore.officialUpdate.latest_version }) }}
              <Icon name="externalLink" size="xs" />
            </a>
            <p class="mt-2 text-xs leading-5 text-gray-600 dark:text-dark-300">{{ t('version.officialNoticeHint') }}</p>
          </div>
        </div>
      </section>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAuthStore, useAppStore } from '@/stores'
import { getUpdateStatus, performUpdate, type ManagedUpdateStatus } from '@/api/admin/system'
import Icon from '@/components/icons/Icon.vue'

const props = defineProps<{ version?: string }>()
const { t } = useI18n()
const authStore = useAuthStore()
const appStore = useAppStore()
const root = ref<HTMLElement | null>(null)
const open = ref(false)
const submitting = ref(false)
const error = ref('')
const reconnecting = ref(false)
const status = ref<ManagedUpdateStatus>({ phase: 'idle' })
const currentVersion = computed(() => appStore.currentVersion || props.version || '')
const attention = computed(() => appStore.hasUpdate || appStore.officialUpdate?.has_update)
const phases = ['idle', 'downloading', 'verifying', 'preparing', 'restarting', 'checking', 'completed', 'failed', 'recovered', 'blocked']
const busy = computed(() => ['downloading', 'verifying', 'preparing', 'restarting', 'checking'].includes(status.value.phase))
const failed = computed(() => ['failed', 'recovered', 'blocked'].includes(status.value.phase))
const phaseLabel = computed(() => t('version.phases.' + (phases.includes(status.value.phase) ? status.value.phase : 'blocked')))
let timer: ReturnType<typeof setTimeout> | undefined
let disposed = false
let polling = false

function reload() { window.location.reload() }

async function pollStatus() {
  if (polling || disposed || !authStore.isAdmin) return
  polling = true
  try {
    status.value = await getUpdateStatus()
    reconnecting.value = false
    if (busy.value || status.value.phase === 'completed') error.value = ''
    if (status.value.phase === 'completed' && status.value.version !== currentVersion.value) {
      appStore.clearVersionCache()
      await appStore.fetchVersion(false)
    }
  } catch {
    // A short cutover may disconnect HTTP. Do not claim success or auto-retry installation.
    reconnecting.value = true
  } finally {
    polling = false
  }
}

async function refresh(force = false) {
  await Promise.all([appStore.fetchVersion(force), pollStatus()])
}
async function toggle() {
  open.value = !open.value
  if (open.value) await refresh(false)
}
async function update() {
  if (busy.value || submitting.value || !appStore.canUpdate) return
  submitting.value = true
  error.value = ''
  try {
    const result = await performUpdate()
    if (result.update_started) {
      status.value = { phase: 'downloading', version: appStore.latestVersion }
      appStore.showInfo(t('version.independentUpdate'))
    } else {
      await appStore.fetchVersion(true)
    }
  } catch {
    error.value = t('version.submitUnconfirmed')
  } finally {
    submitting.value = false
    await pollStatus()
  }
}
function outside(event: MouseEvent) {
  if (root.value && !root.value.contains(event.target as Node)) open.value = false
}
function escape(event: KeyboardEvent) {
  if (event.key === 'Escape' && open.value) {
    open.value = false
    root.value?.querySelector<HTMLButtonElement>('[data-testid=version-toggle]')?.focus()
  }
}
function visible() {
  if (!document.hidden && authStore.isAdmin) void refresh(false)
}
async function tick() {
  if (disposed) return
  if (authStore.isAdmin && (!document.hidden || busy.value)) {
    await appStore.fetchVersion(false)
    if (open.value || busy.value || reconnecting.value) await pollStatus()
  }
  if (!disposed) timer = setTimeout(tick, busy.value || reconnecting.value ? 2000 : 30_000)
}
onMounted(() => {
  if (authStore.isAdmin) void refresh(false)
  timer = setTimeout(tick, 2000)
  document.addEventListener('click', outside)
  document.addEventListener('keydown', escape)
  document.addEventListener('visibilitychange', visible)
})
onBeforeUnmount(() => {
  disposed = true
  clearTimeout(timer)
  document.removeEventListener('click', outside)
  document.removeEventListener('keydown', escape)
  document.removeEventListener('visibilitychange', visible)
})
</script>
