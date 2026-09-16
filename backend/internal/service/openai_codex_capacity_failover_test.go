package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCodexCapacitySchedulerFailoverAfterExposure(t *testing.T) {
	for _, scenario := range []string{"excluded_after_503", "disabled_owner", "owner_healthy", "strict_continuation", "no_capacity_alternative"} {
		t.Run(scenario, func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			c, a, svc := capacityContext(t, []byte(`{"model":"gpt-5.1","prompt_cache_key":"root"}`))
			second := *a
			second.ID = 2
			second.Credentials = map[string]any{"chatgpt_account_id": "second"}
			// Even a higher-priority alternative must not displace a healthy owner.
			second.Priority = -1
			state := codexCapacityFromGin(c)
			registry := svc.codexCapacityRegistry()
			identity := codexAccountIdentityNamespace(a)
			old, err := registry.reserve(identity, capacityAdmissionPolicy(), state.input, false, time.Now())
			require.NoError(t, err)
			old.sent(time.Now()) // Upstream failures still count exposure.
			old.rememberResponse("resp_old", time.Now())
			old.release()
			excluded := map[int64]struct{}{a.ID: {}}
			switch scenario {
			case "disabled_owner":
				a.Schedulable = false
				excluded = nil
			case "owner_healthy":
				excluded = nil
			case "strict_continuation":
				state.websocket = true
				state.input.Previous = "resp_old"
			case "no_capacity_alternative":
				second.Extra = nil
			}
			svc.accountRepo = schedulerTestOpenAIAccountRepo{accounts: []Account{*a, second}}
			selected, _, err := svc.SelectAccountWithScheduler(c.Request.Context(), nil, state.input.Previous, "", "gpt-5.1", excluded, OpenAIUpstreamTransportAny, false)
			if scenario == "strict_continuation" {
				require.Error(t, err)
				require.Equal(t, "session_owner_unavailable", AsCodexSessionCapacityError(err).Code)
				return
			}
			if scenario == "no_capacity_alternative" {
				// affinity 是偏好而非排他约束：capacity 候选耗尽后允许退回普通
				// 账号，不能因为本地容量策略把请求打成 429/503。
				require.NoError(t, err)
				require.Equal(t, second.ID, selected.Account.ID)
				require.Nil(t, capacityLeaseForAccount(c, selected.Account), "普通账号不持容量租约")
				require.False(t, HasCodexSessionCapacity(c))
				if selected.ReleaseFunc != nil {
					selected.ReleaseFunc()
				}
				return
			}
			require.NoError(t, err)
			if selected.ReleaseFunc != nil {
				defer selected.ReleaseFunc()
			}
			if scenario == "owner_healthy" {
				require.Equal(t, a.ID, selected.Account.ID)
				return
			}
			require.Equal(t, second.ID, selected.Account.ID)
			next := capacityLeaseForAccount(c, selected.Account)
			require.NotEqual(t, old.Assignment.ThreadID, next.Assignment.ThreadID)
			next.sent(time.Now())
			ReleaseCodexSessionCapacity(c)
			require.Equal(t, codexAccountIdentityNamespace(&second), registry.boundOwner(state.input))
			// Failover must not erase past exposure or response ownership.
			require.Len(t, registry.accounts[identity].Roots, 1)
			continued := state.input
			continued.Previous = "resp_old"
			responseOwner, err := registry.continuationOwner(continued, time.Now())
			require.NoError(t, err)
			require.Equal(t, identity, responseOwner)
			original, err := registry.reserve(identity, capacityAdmissionPolicy(), continued, false, time.Now())
			require.NoError(t, err)
			original.release()
		})
	}
}

func TestCodexCapacityFailoverStillEnforcesTargetCapacityAndContinuation(t *testing.T) {
	registry := newCodexCapacityRegistry()
	policy := capacityAdmissionPolicy()
	policy.MaxRootSessions = 1
	now := time.Now()
	in := capacityInput(1, "root", "root")
	old, err := registry.reserve("old", policy, in, false, now)
	require.NoError(t, err)
	old.sent(now)
	old.rememberResponse("resp_old", now)
	old.release()
	host, err := registry.reserve("new", policy, capacityInput(1, "host", "host"), false, now)
	require.NoError(t, err)
	host.sent(now)
	host.release()
	_, err = registry.reserve("new", policy, in, false, now)
	require.Equal(t, "session_roots_full", AsCodexSessionCapacityError(err).Code)
	moved, err := registry.reserve("new", policy, in, true, now)
	require.NoError(t, err)
	require.True(t, moved.Assignment.Synthetic)
	require.Equal(t, host.Assignment.ThreadID, moved.Assignment.ParentThreadID)
	moved.sent(now)
	moved.release()
	in.Previous = "resp_old"
	_, err = registry.reserve("new", policy, in, true, now)
	require.Equal(t, "session_continuation_unbound", AsCodexSessionCapacityError(err).Code)
	in.Key = 2
	_, err = registry.continuationOwner(in, now)
	require.Equal(t, "session_continuation_owner_mismatch", AsCodexSessionCapacityError(err).Code)
}

func TestCodexCapacityActiveLeasePinsAccountCapacityNotOtherAccounts(t *testing.T) {
	registry := newCodexCapacityRegistry()
	policy := capacityAdmissionPolicy()
	policy.MaxRootSessions = 1
	now := time.Now()
	in := capacityInput(1, "root", "root")
	first, err := registry.reserve("old", policy, in, false, now)
	require.NoError(t, err)
	first.sent(now)
	second, err := registry.reserve("new", policy, in, false, now)
	require.NoError(t, err)
	require.NotEqual(t, first.Assignment.ThreadID, second.Assignment.ThreadID)
	second.sent(now)
	second.release()
	// 窗口内的活跃租约仍然占住本账号的根位（另一账号刚才已独立拿到同一逻辑根）。
	_, err = registry.reserve("old", policy, capacityInput(1, "other", "other"), false, now.Add(time.Minute))
	require.Equal(t, "session_roots_full", AsCodexSessionCapacityError(err).Code)
	first.release()
	later := now.Add(time.Duration(policy.WindowSeconds)*time.Second + time.Minute)
	fresh, err := registry.reserve("old", policy, capacityInput(1, "other", "other"), false, later)
	require.NoError(t, err)
	fresh.release()
}

// 空闲超过窗口的长连接不再永久占用根位；它恢复说话时按既有会话豁免放行，
// 不会在第二轮被自己的上限打成 429。
func TestCodexCapacityIdleLeaseReleasesRootAndResumeIsGrandfathered(t *testing.T) {
	registry := newCodexCapacityRegistry()
	policy := capacityAdmissionPolicy()
	policy.MaxRootSessions = 1
	now := time.Now()
	idle := capacityInput(1, "root", "root")
	held, err := registry.reserve("account", policy, idle, false, now)
	require.NoError(t, err)
	held.sent(now)
	later := now.Add(time.Duration(policy.WindowSeconds)*time.Second + time.Minute)
	fresh, err := registry.reserve("account", policy, capacityInput(1, "other", "other"), false, later)
	require.NoError(t, err, "空闲租约让出根位")
	fresh.sent(later)
	resumed, err := registry.reserve("account", policy, idle, false, later)
	require.NoError(t, err, "既有会话继续说话不受新根上限限制")
	require.Equal(t, held.Assignment.ThreadID, resumed.Assignment.ThreadID)
	resumed.release()
	fresh.release()
	held.release()
}
