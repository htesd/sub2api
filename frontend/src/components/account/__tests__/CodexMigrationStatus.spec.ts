import { describe, it, expect, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import type { Account } from '@/types'
import CodexMigrationStatus from '../CodexMigrationStatus.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
describe('CodexMigrationStatus', () => {
  it('hides unused accounts and presents known results without guessing unknown states', async () => {
    const wrapper = mount(CodexMigrationStatus, { props: { account: { extra: {} } as Account } })
    expect(wrapper.find('[data-testid="codex-migration-status"]').exists()).toBe(false)
    await wrapper.setProps({ account: { extra: { codex_ticket_auto_migrate: true } } as Account })
    expect(wrapper.text()).toContain('codexMigrationResults.pending')
    await wrapper.setProps({ account: { extra: { codex_ticket_migration_state: { result: 'migrated', from_proxy_id: 1, to_proxy_id: 2 } } } as Account })
    expect(wrapper.text()).toContain('codexMigrationResults.migrated')
    await wrapper.setProps({ account: { extra: { codex_ticket_migration_state: { result: 'future_status' } } } as Account })
    expect(wrapper.text()).toContain('codexMigrationResults.pending')
  })
})
