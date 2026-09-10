import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import CodexRequestPolicyForm from '../CodexRequestPolicyForm.vue'
import { readCodexRequestPolicy } from '@/utils/codexRequestPolicy'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

describe('Codex request controls', () => {
  it('keeps legacy accounts disabled and bounds persisted numbers', () => {
    expect(readCodexRequestPolicy().enabled).toBe(false)
    expect(readCodexRequestPolicy({ max_attempts: 100, retry_window_seconds: -1, identity_mode: 'unknown' }))
      .toMatchObject({ max_attempts: 20, retry_window_seconds: 1, identity_mode: 'canonical' })
  })

  it('emits an explicit disable while retaining the configured policy', async () => {
    const policy = { ...readCodexRequestPolicy(), enabled: true, max_attempts: 4 }
    const wrapper = mount(CodexRequestPolicyForm, { props: { modelValue: policy, fingerprintMode: 'capacity' } })
    await wrapper.get('input[type="checkbox"]').setValue(false)
    expect(wrapper.emitted('update:modelValue')?.[0]?.[0]).toMatchObject({ enabled: false, max_attempts: 4 })
  })

  it('offers the queue baseline only for capacity mode and exposes its timeout', async () => {
    const policy = { ...readCodexRequestPolicy(), enabled: true, capacity_mode: 'queue' as const }
    const wrapper = mount(CodexRequestPolicyForm, { props: { modelValue: policy, fingerprintMode: 'off' } })
    expect(wrapper.findAll('select')).toHaveLength(1)
    await wrapper.setProps({ fingerprintMode: 'capacity' })
    expect(wrapper.findAll('select')).toHaveLength(2)
    expect(wrapper.text()).toContain('capacity_wait_seconds')
  })
})
