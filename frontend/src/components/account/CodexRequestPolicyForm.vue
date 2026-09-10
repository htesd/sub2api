<template>
  <fieldset class="space-y-3 border-t border-gray-200 pt-4 dark:border-dark-600">
    <legend class="input-label">{{ t(`${key}.title`) }}</legend>
    <label class="flex items-center gap-2 text-sm">
      <input type="checkbox" :checked="modelValue.enabled" @change="patch({ enabled: ($event.target as HTMLInputElement).checked })" />
      {{ t(`${key}.enabled`) }}
    </label>
    <p class="input-hint">{{ t(`${key}.description`) }}</p>
    <div v-if="modelValue.enabled" class="space-y-3">
      <div class="grid grid-cols-1 gap-3 sm:grid-cols-3">
        <label v-for="field in numericFields" :key="field.name" class="text-sm">
          {{ t(`${key}.${field.name}`) }}
          <input class="input mt-1" type="number" required :min="field.min" :max="field.max" :value="modelValue[field.name]" @input="patch({ [field.name]: Number(($event.target as HTMLInputElement).value) })" />
        </label>
      </div>
      <p class="input-hint">{{ t(`${key}.budgetHint`) }}</p>
      <label class="block text-sm">
        {{ t(`${key}.identity`) }}
        <select class="input mt-1" :value="modelValue.identity_mode" @change="patch({ identity_mode: ($event.target as HTMLSelectElement).value as CodexRequestPolicy['identity_mode'] })">
          <option value="canonical">{{ t(`${key}.canonical`) }}</option>
          <option value="preserve">{{ t(`${key}.preserve`) }}</option>
        </select>
      </label>
      <p class="input-hint">{{ t(`${key}.identityHint`) }}</p>
      <template v-if="fingerprintMode === 'capacity'">
        <label class="block text-sm">
          {{ t(`${key}.capacity`) }}
          <select class="input mt-1" :value="modelValue.capacity_mode" @change="patch({ capacity_mode: ($event.target as HTMLSelectElement).value as CodexRequestPolicy['capacity_mode'] })">
            <option value="inherit">{{ t(`${key}.inherit`) }}</option>
            <option value="subagent">{{ t(`${key}.subagent`) }}</option>
            <option value="queue">{{ t(`${key}.queue`) }}</option>
          </select>
        </label>
        <label v-if="modelValue.capacity_mode === 'queue'" class="block text-sm">
          {{ t(`${key}.capacity_wait_seconds`) }}
          <input class="input mt-1" type="number" required min="0" max="60" :value="modelValue.capacity_wait_seconds" @input="patch({ capacity_wait_seconds: Number(($event.target as HTMLInputElement).value) })" />
        </label>
        <p class="input-hint">{{ t(`${key}.queueHint`) }}</p>
      </template>
    </div>
  </fieldset>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { CodexRequestPolicy } from '@/utils/codexRequestPolicy'
const props = defineProps<{ modelValue: CodexRequestPolicy; fingerprintMode?: string }>()
const emit = defineEmits<{ 'update:modelValue': [value: CodexRequestPolicy] }>()
const { t } = useI18n()
const key = 'admin.accounts.openai.requestPolicy'
const numericFields = [
  { name: 'max_attempts', min: 1, max: 20 },
  { name: 'retry_window_seconds', min: 1, max: 120 },
  { name: 'cooldown_seconds', min: 1, max: 60 }
] as const
function patch(value: Partial<CodexRequestPolicy>) {
  emit('update:modelValue', { ...props.modelValue, ...value })
}
</script>
