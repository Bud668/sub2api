<template>
  <HelpTooltip v-if="quota?.fixed_slots && (quota.enabled || (quota.requested_enabled && quota.activation_pending))" trigger="click" :content="hint" class="!ml-0">
    <template #trigger="{ open, tooltipId }">
      <button type="button" class="inline-flex shrink-0 items-center rounded-md border border-gray-300 px-2 py-0.5 text-xs font-semibold leading-4 text-blue-700 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500 dark:border-dark-500 dark:text-blue-300" :aria-label="hint" :aria-expanded="open" :aria-describedby="tooltipId" data-testid="fixed-seat-badge">
        {{ t('dynamicQuota.fixedSlotsBadge', { n: quota.fixed_slots }) }}
      </button>
    </template>
  </HelpTooltip>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { DynamicSubscriptionQuota } from '@/types'
import HelpTooltip from './HelpTooltip.vue'

const props = defineProps<{ quota?: DynamicSubscriptionQuota | null }>()
const { t } = useI18n()
const hint = computed(() => {
  const q = props.quota
  const source = q?.source_fixed_slots && q.source_fixed_slots > (q.fixed_slots || 0)
    ? t('dynamicQuota.sourceSeatsHint', { n: q.source_fixed_slots }) : ''
  return t('dynamicQuota.fixedSlotsHint', { n: q?.fixed_slots || 0 }) + source
})
</script>
