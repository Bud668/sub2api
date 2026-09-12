<template>
  <HelpTooltip trigger="click" :content="t('dynamicQuota.adminDebugHint')" class="!ml-0">
    <template #trigger="{ open, tooltipId }">
      <button type="button" class="inline-flex shrink-0 items-center rounded-md border border-gray-300 px-2 py-0.5 text-xs font-semibold leading-4 text-violet-700 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500 dark:border-dark-500 dark:text-violet-300" :aria-label="t('dynamicQuota.adminDebugHint')" :aria-expanded="open" :aria-describedby="tooltipId" data-testid="admin-debug-badge">
        {{ t('dynamicQuota.adminDebug') }}
      </button>
    </template>
  </HelpTooltip>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import HelpTooltip from './HelpTooltip.vue'

const { t } = useI18n()
</script>
