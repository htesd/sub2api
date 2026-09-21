<template>
  <span
    class="inline-flex shrink-0 items-center rounded px-1 text-[10px] leading-4"
    :class="isOtherProxy ? 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300' : 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-400'"
    :title="title"
    data-testid="codex-ticket-source"
    :data-source="sourceKey"
  >
    {{ t(`admin.accounts.openai.codexTicketSource.${sourceKey}`) }}
    <span v-if="ticket.source_proxy_id" class="ml-1">#{{ ticket.source_proxy_id }}</span>
  </span>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { Account } from '@/types'

const props = defineProps<{
  ticket: NonNullable<Account['codex_turn_tickets']>[number]
  currentProxyId?: number | null
}>()
const { t } = useI18n()
const sourceKey = computed(() => {
  switch (props.ticket.source) {
    case 'account_proxy':
      if (props.ticket.source_proxy_changed || (props.ticket.source_proxy_id && props.ticket.source_proxy_id !== props.currentProxyId)) return 'previousProxy'
      return 'accountProxy'
    case 'harvest_pool': return 'harvestPool'
    case 'business_response': return 'businessResponse'
    default: return 'unknown'
  }
})
const isOtherProxy = computed(() => sourceKey.value === 'harvestPool' || sourceKey.value === 'previousProxy')
const title = computed(() => {
  const parts = [t(`admin.accounts.openai.codexTicketSource.${sourceKey.value}Desc`)]
  if (isOtherProxy.value) parts.push(t('admin.accounts.openai.codexTicketSource.riskHint'))
  parts.push(t('admin.accounts.openai.codexTicketSource.scopeHint'))
  return parts.join(' ')
})
</script>
