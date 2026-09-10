export interface CodexRequestPolicy {
  enabled: boolean
  max_attempts: number
  retry_window_seconds: number
  cooldown_seconds: number
  identity_mode: 'canonical' | 'preserve'
  capacity_mode: 'inherit' | 'queue' | 'subagent'
  capacity_wait_seconds: number
}

export function readCodexRequestPolicy(raw?: unknown): CodexRequestPolicy {
  const p = raw && typeof raw === 'object' ? raw as Record<string, unknown> : {}
  const number = (key: string, fallback: number, min: number, max: number) => {
    const value = p[key]
    return typeof value === 'number' && Number.isFinite(value)
      ? Math.max(min, Math.min(max, Math.trunc(value))) : fallback
  }
  return {
    enabled: p.enabled === true,
    max_attempts: number('max_attempts', 6, 1, 20),
    retry_window_seconds: number('retry_window_seconds', 30, 1, 120),
    cooldown_seconds: number('cooldown_seconds', 5, 1, 60),
    identity_mode: p.identity_mode === 'preserve' ? 'preserve' : 'canonical',
    capacity_mode: p.capacity_mode === 'queue' || p.capacity_mode === 'subagent' ? p.capacity_mode : 'inherit',
    capacity_wait_seconds: number('capacity_wait_seconds', 10, 0, 60)
  }
}
