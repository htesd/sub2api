<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { Account } from '@/types'

const props = defineProps<{ account: Account | null }>()
const { t } = useI18n()
const state = computed(() => {
  const raw = props.account?.extra?.codex_ticket_migration_state
  if (!raw || typeof raw !== 'object') return null
  return raw as Record<string, unknown>
})
const results = new Set(['migrated', 'current_proxy_ok', 'current_proxy_ok_no_ticket', 'migration_cooldown', 'candidate_ticket_invalid', 'probe_failed', 'source_proxy_missing', 'token_unavailable', 'upstream_limited', 'egress_unknown', 'proxy_lookup_failed', 'no_same_region_candidate', 'candidate_not_astra', 'ticket_replay_failed', 'egress_changed', 'candidate_removed', 'configuration_changed'])
const result = computed(() => {
  const value = state.value?.result
  return typeof value === 'string' && results.has(value) ? value : 'pending'
})
</script>

<template>
  <div v-if="account?.extra?.codex_ticket_auto_migrate === true || state" class="mt-2 space-y-1 text-xs text-gray-500 dark:text-gray-400" data-testid="codex-migration-status">
    <p>{{ t('admin.accounts.openai.codexMigrationResults.' + result) }}</p>
    <p v-if="state?.to_proxy_id">
      {{ t('admin.accounts.openai.codexMigrationRoute', { from: state.from_proxy_id, to: state.to_proxy_id }) }}
      <span v-if="state.country"> · {{ state.country }}</span>
    </p>
    <p v-if="state?.checked_at">{{ state.checked_at }}</p>
  </div>
</template>
