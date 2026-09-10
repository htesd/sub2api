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
			old, err := registry.reserve(identity, defaultCodexCapacityPolicy(), state.input, false, time.Now())
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
				require.Error(t, err)
				require.NotEqual(t, "session_owner_unavailable", err.Error())
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
			original, err := registry.reserve(identity, defaultCodexCapacityPolicy(), continued, false, time.Now())
			require.NoError(t, err)
			original.release()
		})
	}
}

func TestCodexCapacityFailoverPinsActiveFamily(t *testing.T) {
	registry := newCodexCapacityRegistry()
	policy := defaultCodexCapacityPolicy()
	now := time.Now()
	root := capacityInput(1, "session", "parent")
	old, err := registry.reserve("old", policy, root, false, now)
	require.NoError(t, err)
	old.sent(now)
	old.release()
	child := capacityInput(1, "session", "child")
	child.Parent = "parent"
	active, err := registry.reserve("old", policy, child, false, now)
	require.NoError(t, err)
	active.sent(now)
	_, err = registry.reserveWithFailover("new", policy, root, false, true, now)
	require.Equal(t, "session_in_use", AsCodexSessionCapacityError(err).Code)
	require.Equal(t, 429, AsCodexSessionCapacityError(err).Status)
	active.release()
	next, err := registry.reserveWithFailover("new", policy, root, false, true, now.Add(time.Second))
	require.NoError(t, err)
	next.sent(now.Add(time.Second))
	// The old child binding must not override the actively migrated parent.
	require.Equal(t, "new", registry.boundOwner(child))
	_, err = registry.reserveWithFailover("old", policy, child, false, true, now)
	require.Error(t, err)
	next.release()
	// A separate API key is independent even when clients reuse logical IDs.
	root.Key = 2
	independent, err := registry.reserveWithFailover("other", policy, root, false, true, now)
	require.NoError(t, err)
	independent.release()
}

func TestCodexCapacityFailoverStillEnforcesTargetCapacityAndContinuation(t *testing.T) {
	registry := newCodexCapacityRegistry()
	policy := defaultCodexCapacityPolicy()
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
	_, err = registry.reserveWithFailover("new", policy, in, false, true, now)
	require.Equal(t, "session_roots_full", AsCodexSessionCapacityError(err).Code)
	moved, err := registry.reserveWithFailover("new", policy, in, true, true, now)
	require.NoError(t, err)
	require.True(t, moved.Assignment.Synthetic)
	require.Equal(t, host.Assignment.ThreadID, moved.Assignment.ParentThreadID)
	moved.sent(now)
	moved.release()
	in.Previous = "resp_old"
	_, err = registry.reserveWithFailover("new", policy, in, true, true, now)
	require.Equal(t, "session_continuation_unbound", AsCodexSessionCapacityError(err).Code)
	in.Key = 2
	_, err = registry.continuationOwner(in, now)
	require.Equal(t, "session_continuation_owner_mismatch", AsCodexSessionCapacityError(err).Code)
}

func TestCodexCapacityFailoverPinsTransitiveFamily(t *testing.T) {
	for _, liveThread := range []int{0, 2} {
		t.Run([]string{"parent_active", "unused", "grandchild_active"}[liveThread], func(t *testing.T) {
			registry := newCodexCapacityRegistry()
			policy := defaultCodexCapacityPolicy()
			now := time.Now()
			inputs := []codexCapacityInput{capacityInput(1, "P", "P"), capacityInput(1, "C", "C"), capacityInput(1, "G", "G")}
			inputs[1].Parent = "P"
			inputs[2].Parent = "C"
			leases := make([]*codexCapacityLease, 3)
			for i, in := range inputs {
				var err error
				leases[i], err = registry.reserve("old", policy, in, false, now)
				require.NoError(t, err)
				leases[i].sent(now)
			}
			for i, l := range leases {
				if i != liveThread {
					l.release()
				}
			}
			target := inputs[2-liveThread]
			// An active root retains its ancestral mappings beyond the idle TTL.
			registry.accounts["old"].prune(now.Add(25 * time.Hour))
			_, err := registry.reserveWithFailover("new", policy, target, false, true, now.Add(25*time.Hour))
			require.Equal(t, "session_in_use", AsCodexSessionCapacityError(err).Code)
			leases[liveThread].release()
			moved, err := registry.reserveWithFailover("new", policy, target, false, true, now.Add(25*time.Hour))
			require.NoError(t, err)
			moved.release()
		})
	}
}
