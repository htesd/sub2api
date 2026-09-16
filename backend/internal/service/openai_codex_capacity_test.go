package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func capacityInput(key int64, root, thread string) codexCapacityInput {
	return codexCapacityInput{Key: key, Root: root, Thread: thread, Mountable: true}
}
func exposeCapacity(t *testing.T, r *codexCapacityRegistry, p CodexSessionCapacityPolicy, in codexCapacityInput, now time.Time) *codexCapacityLease {
	t.Helper()
	l, err := r.reserve("account", p, in, false, now)
	require.NoError(t, err)
	l.sent(now)
	l.release()
	return l
}
func TestCodexCapacityOverflowAndIsolation(t *testing.T) {
	now := time.Now()
	r := newCodexCapacityRegistry()
	p := defaultCodexCapacityPolicy()
	for i := 0; i < 5; i++ {
		exposeCapacity(t, r, p, capacityInput(1, fmt.Sprint("root", i), fmt.Sprint("thread", i)), now.Add(time.Duration(i)*time.Second))
	}
	in := capacityInput(1, "sixth-root", "sixth-thread")
	_, err := r.reserve("account", p, in, false, now.Add(10*time.Second))
	require.Equal(t, "session_roots_full", AsCodexSessionCapacityError(err).Code)
	child, err := r.reserve("account", p, in, true, now.Add(10*time.Second))
	require.NoError(t, err)
	require.True(t, child.Assignment.Synthetic)
	require.Equal(t, codexCapacityNode("account", 1, "root0"), child.Assignment.SessionID)
	require.Equal(t, codexCapacityNode("account", 1, "thread0"), child.Assignment.ParentThreadID)
	require.NotEqual(t, child.Assignment.SessionID, child.Assignment.ThreadID)
	child.sent(now.Add(10 * time.Second))
	child.release()
	again, err := r.reserve("account", p, in, false, now.Add(11*time.Second))
	require.NoError(t, err)
	require.Equal(t, child.Assignment, again.Assignment)
	again.release()
	_, err = r.reserve("account", p, capacityInput(2, "other", "other"), true, now.Add(11*time.Second))
	require.Equal(t, "session_no_eligible_host", AsCodexSessionCapacityError(err).Code)
	require.NotEqual(t, codexCapacityNode("account", 1, "x"), codexCapacityNode("account", 2, "x"))
	require.NotEqual(t, codexCapacityNode("account", 1, "x"), codexCapacityNode("other-account", 1, "x"))
}
func TestCodexCapacityConcurrentReservationsAndCancel(t *testing.T) {
	r := newCodexCapacityRegistry()
	p := defaultCodexCapacityPolicy()
	now := time.Now()
	var accepted atomic.Int32
	var wg sync.WaitGroup
	leases := make(chan *codexCapacityLease, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l, err := r.reserve("account", p, capacityInput(1, fmt.Sprint(i), fmt.Sprint(i)), false, now)
			if err == nil {
				accepted.Add(1)
				leases <- l
			}
		}(i)
	}
	wg.Wait()
	close(leases)
	require.Equal(t, int32(5), accepted.Load())
	for l := range leases {
		l.release()
		l.release()
		l.sent(now)
	}
	require.Empty(t, r.accounts["account"].Bindings)
	require.Empty(t, r.accounts["account"].Roots)
	l, err := r.reserve("account", p, capacityInput(1, "sent", "sent"), false, now)
	require.NoError(t, err)
	l.sent(now)
	l.release()
	require.Len(t, r.accounts["account"].Roots, 1)
}
func TestCodexCapacityChildLimitsWindowAndNativeChild(t *testing.T) {
	r := newCodexCapacityRegistry()
	p := defaultCodexCapacityPolicy()
	p.MaxRootSessions = 1
	p.MaxChildrenPerRoot = 2
	p.MaxNewChildrenPerWindow = 2
	now := time.Now()
	exposeCapacity(t, r, p, capacityInput(1, "root", "host-thread"), now)
	for i := 0; i < 2; i++ {
		l, err := r.reserve("account", p, capacityInput(1, fmt.Sprint(i), fmt.Sprint(i)), true, now)
		require.NoError(t, err)
		l.sent(now)
		l.release()
	}
	_, err := r.reserve("account", p, capacityInput(1, "excess", "excess"), true, now)
	require.Error(t, err)
	native := capacityInput(1, "root", "native")
	native.Parent = "host-thread"
	native.Mountable = false
	l, err := r.reserve("account", p, native, true, now)
	require.NoError(t, err)
	require.False(t, l.Assignment.Synthetic)
	l.sent(now)
	l.release()
	l, err = r.reserve("account", p, capacityInput(1, "fresh", "fresh"), false, now.Add(601*time.Second))
	require.NoError(t, err)
	require.False(t, l.Assignment.Synthetic)
	l.release()
	// A live lease pins its root after the rolling window expires.
	held, err := r.reserve("account", p, capacityInput(1, "root", "host-thread"), false, now)
	require.NoError(t, err)
	_, err = r.reserve("account", p, capacityInput(1, "blocked", "blocked"), false, now.Add(time.Hour))
	require.Error(t, err)
	held.release()
}
func TestCodexCapacityContinuationOwnership(t *testing.T) {
	r := newCodexCapacityRegistry()
	p := defaultCodexCapacityPolicy()
	now := time.Now()
	in := capacityInput(1, "root", "thread")
	l := exposeCapacity(t, r, p, in, now)
	l.rememberResponse("resp_one", now)
	in.Previous = "resp_one"
	owner := r.responseOwner(in, now)
	require.Equal(t, "account", owner)
	continued, err := r.reserve("account", p, in, true, now)
	require.NoError(t, err)
	continued.release()
	for _, bad := range []codexCapacityInput{{Key: 2, Root: "root", Thread: "thread", Previous: "resp_one"}, {Key: 1, Root: "root", Thread: "other", Previous: "resp_one"}, {Key: 1, Root: "root", Thread: "thread", Previous: "resp_unknown"}} {
		_, err := r.reserve("account", p, bad, true, now)
		require.Equal(t, 409, AsCodexSessionCapacityError(err).Status)
	}
	_, err = r.reserve("account", p, in, true, now.Add(25*time.Hour))
	require.Error(t, err)
}
func capacityContext(t *testing.T, body []byte) (*gin.Context, *Account, *OpenAIGatewayService) {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Set("api_key", &APIKey{ID: 1})
	c.Request = c.Request.WithContext(WithCodexSessionCapacity(c.Request.Context(), c, body))
	t.Cleanup(func() { ReleaseCodexSessionCapacity(c) })
	a := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 10, Extra: map[string]any{"codex_fingerprint_mode": "capacity", "codex_fingerprint_seed": "00000000-0000-4000-8000-000000000001"}, Credentials: map[string]any{"chatgpt_account_id": "account"}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{*a}}, cache: &schedulerTestGatewayCache{}, concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{})}
	return c, a, svc
}
func TestCodexCapacityProjectionAndResponseRestore(t *testing.T) {
	body := []byte(`{"model":"gpt-5.1","prompt_cache_key":"child","input":[{"role":"user","content":"child","internal_chat_message_metadata_passthrough":{"turn_id":"turn"}}],"client_metadata":{"session_id":"child","thread_id":"child","x-codex-turn-metadata":"{\"turn_id\":\"turn\",\"window_id\":\"child:2\",\"note\":\"中文😀\"}"}}`)
	c, a, svc := capacityContext(t, body)
	r := svc.codexCapacityRegistry()
	p := defaultCodexCapacityPolicy()
	p.MaxRootSessions = 1
	now := time.Now()
	identity := codexAccountIdentityNamespace(a)
	host, err := r.reserve(identity, p, capacityInput(1, "root", "host-thread"), false, now)
	require.NoError(t, err)
	host.sent(now)
	host.release()
	state := codexCapacityFromGin(c)
	l, err := r.reserve(identity, p, state.input, true, now)
	require.NoError(t, err)
	state.stage(a.ID, l, l.release)
	h := make(http.Header)
	projected, err := projectCodexCapacity(c, a, body, h)
	require.NoError(t, err)
	require.Equal(t, host.Assignment.SessionID, gjson.GetBytes(projected, "prompt_cache_key").String())
	require.Equal(t, l.Assignment.ThreadID, h.Get("thread-id"))
	require.Equal(t, host.Assignment.ThreadID, h.Get("x-codex-parent-thread-id"))
	turnID := gjson.Get(gjson.GetBytes(projected, "client_metadata.x-codex-turn-metadata").String(), "turn_id").String()
	require.Equal(t, turnID, gjson.GetBytes(projected, "input.0.internal_chat_message_metadata_passthrough.turn_id").String())
	require.Equal(t, "child", gjson.GetBytes(projected, "input.0.content").String())
	for _, r := range h.Get("x-codex-turn-metadata") {
		require.Less(t, r, rune(128))
	}
	require.True(t, json.Valid([]byte(h.Get("x-codex-turn-metadata"))))
	twice, err := projectCodexCapacity(c, a, projected, nil)
	require.NoError(t, err)
	require.JSONEq(t, string(projected), string(twice))
	response := []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_child","client_metadata":%s,"prompt_cache_key":%q,"output":[{"text":%q}]}}`, gjson.GetBytes(projected, "client_metadata").Raw, l.Assignment.SessionID, l.Assignment.SessionID))
	l.sent(now)
	restored := restoreCodexToolNamesFromContext(c, response)
	require.Equal(t, "child", gjson.GetBytes(restored, "response.client_metadata.session_id").String())
	require.False(t, gjson.GetBytes(restored, "response.client_metadata.x-codex-parent-thread-id").Exists())
	require.False(t, gjson.GetBytes(restored, "response.client_metadata.x-openai-subagent").Exists())
	require.Equal(t, l.Assignment.SessionID, gjson.GetBytes(restored, "response.output.0.text").String())
	in := state.input
	in.Previous = "resp_child"
	require.Equal(t, identity, r.responseOwner(in, now))
	c.Request.URL.Path = "/v1/responses/compact"
	compact, err := projectCodexCapacity(c, a, body, h)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(compact, "client_metadata").Exists())
	require.Equal(t, l.Assignment.SessionID, h.Get("session_id"))
}
func TestCodexCapacitySchedulerNormalBeforeOverflow(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	body := []byte(`{"model":"gpt-5.1","prompt_cache_key":"new"}`)
	c, a, svc := capacityContext(t, body)
	second := *a
	second.ID = 2
	second.Credentials = map[string]any{"chatgpt_account_id": "second"}
	svc.accountRepo = schedulerTestOpenAIAccountRepo{accounts: []Account{*a, second}}
	p := defaultCodexCapacityPolicy()
	r := svc.codexCapacityRegistry()
	now := time.Now()
	for i := 0; i < 5; i++ {
		l, err := r.reserve(codexAccountIdentityNamespace(a), p, capacityInput(1, fmt.Sprint(i), fmt.Sprint(i)), false, now)
		require.NoError(t, err)
		l.sent(now)
		l.release()
	}
	selected, _, err := svc.SelectAccountWithScheduler(c.Request.Context(), nil, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.Equal(t, int64(2), selected.Account.ID)
	require.False(t, capacityLeaseForAccount(c, selected.Account).Assignment.Synthetic)
	if selected.ReleaseFunc != nil {
		selected.ReleaseFunc()
	}
	ReleaseCodexSessionCapacity(c)
	// Excluding the free account enables overflow on the full account.
	selected, _, err = svc.SelectAccountWithScheduler(c.Request.Context(), nil, "", "", "gpt-5.1", map[int64]struct{}{2: {}}, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.Equal(t, int64(1), selected.Account.ID)
	require.True(t, capacityLeaseForAccount(c, selected.Account).Assignment.Synthetic)
	if selected.ReleaseFunc != nil {
		selected.ReleaseFunc()
	}
}
func TestCodexCapacityDeviceModeDoesNotFlattenThreads(t *testing.T) {
	c, a, _ := capacityContext(t, []byte(`{"prompt_cache_key":"root"}`))
	ids := resolveCodexFingerprintIDs(a, "root", a.GetCodexFingerprintMode())
	require.NotNil(t, ids)
	h := http.Header{"Session-Id": []string{"root"}, "Thread-Id": []string{"thread"}}
	applyCodexFingerprintHeaders(h, ids)
	require.Equal(t, "root", h.Get("session-id"))
	require.Equal(t, "thread", h.Get("thread-id"))
	require.NotEmpty(t, h.Get("x-codex-installation-id"))
	require.False(t, HasCodexSessionCapacity(c))
}
func TestCodexCapacityCancellationKeepsAdmissionUntilForwardReturns(t *testing.T) {
	r := newCodexCapacityRegistry()
	l, err := r.reserve("account", defaultCodexCapacityPolicy(), capacityInput(1, "r", "t"), false, time.Now())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	// HTTP uses a detached upstream context: cancellation must not release an
	// unsent reservation while the forwarder can still dispatch it.
	l.sent(time.Now())
	require.Equal(t, 1, l.binding.Leases)
	require.False(t, l.binding.LastSent.IsZero())
	l.release()
	require.Equal(t, 0, l.binding.Leases)
}

func TestCodexCapacityConcurrentAccountScopedBindings(t *testing.T) {
	r := newCodexCapacityRegistry()
	p := defaultCodexCapacityPolicy()
	var wg sync.WaitGroup
	var accepted atomic.Int32
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lease, err := r.reserve(fmt.Sprint(i), p, capacityInput(1, "root", "thread"), false, time.Now())
			if err == nil {
				lease.sent(time.Now())
				accepted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	require.Equal(t, int32(40), accepted.Load())
	require.NotEmpty(t, r.boundOwner(capacityInput(1, "root", "thread")))
	parentOwner := r.boundOwner(capacityInput(1, "root", "thread"))
	native := capacityInput(1, "root", "native")
	native.Parent = "thread"
	require.Equal(t, parentOwner, r.boundOwner(native))
	_, err := r.reserve("foreign", p, native, false, time.Now())
	require.NoError(t, err)
}
func TestCodexCapacityMixedPoolCannotReplayForeignContinuation(t *testing.T) {
	body := []byte(`{"prompt_cache_key":"foreign","previous_response_id":"resp_known"}`)
	c, a, svc := capacityContext(t, body)
	ordinary := *a
	ordinary.ID = 2
	ordinary.Extra = nil
	ordinary.Type = AccountTypeAPIKey
	svc.accountRepo = schedulerTestOpenAIAccountRepo{accounts: []Account{ordinary, *a}}
	r := svc.codexCapacityRegistry()
	in := capacityInput(1, "root", "original")
	l, err := r.reserve(codexAccountIdentityNamespace(a), defaultCodexCapacityPolicy(), in, false, time.Now())
	require.NoError(t, err)
	l.sent(time.Now())
	l.rememberResponse("resp_known", time.Now())
	l.release()
	_, _, err = svc.SelectAccountWithScheduler(c.Request.Context(), nil, "resp_known", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.Equal(t, "session_continuation_owner_mismatch", AsCodexSessionCapacityError(err).Code)
}
func TestCodexCapacityWSOmittedIdentityAndDeviceMetadata(t *testing.T) {
	body := []byte(`{"prompt_cache_key":"root","client_metadata":{"session_id":"root","thread_id":"child","x-codex-turn-metadata":"{\"installation_id\":\"raw-device\",\"turn_id\":\"first\"}"}}`)
	c, a, svc := capacityContext(t, body)
	c.Request.Header.Set("session-id", "root")
	state := codexCapacityFromGin(c)
	l, err := svc.codexCapacityRegistry().reserve(codexAccountIdentityNamespace(a), defaultCodexCapacityPolicy(), state.input, false, time.Now())
	require.NoError(t, err)
	state.stage(a.ID, l, l.release)
	ids := resolveCodexFingerprintIDsFromRequest(a, c.Request.Header)
	stageCodexFingerprintIDs(c, ids)
	h := http.Header{"X-Codex-Turn-Metadata": []string{`{"installation_id":"raw-device"}`}}
	applyStagedCodexFingerprintHeaders(c, a, h)
	_, err = projectCodexCapacity(c, a, nil, h)
	require.NoError(t, err)
	require.Equal(t, ids.installationID, gjson.Get(h.Get("x-codex-turn-metadata"), "installation_id").String())
	for _, frame := range []string{`{"type":"response.create"}`, `{"client_metadata":{"session_id":"root"}}`, `{"client_metadata":{"x-codex-turn-metadata":"{\"turn_id\":\"second\"}"}}`} {
		require.NoError(t, validateCodexCapacityFrame(c, []byte(frame)))
	}
	require.Error(t, validateCodexCapacityFrame(c, []byte(`{"client_metadata":{"thread_id":"foreign"}}`)))
}

func TestCodexCapacityHTTPContinuationFailsExplicitly(t *testing.T) {
	c, a, svc := capacityContext(t, []byte(`{"prompt_cache_key":"root","previous_response_id":"resp_http"}`))
	in := capacityInput(1, "root", "root")
	r := svc.codexCapacityRegistry()
	l, err := r.reserve(codexAccountIdentityNamespace(a), defaultCodexCapacityPolicy(), in, false, time.Now())
	require.NoError(t, err)
	l.sent(time.Now())
	l.rememberResponse("resp_http", time.Now())
	l.release()
	_, _, err = svc.SelectAccountWithScheduler(c.Request.Context(), nil, "resp_http", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.Equal(t, "session_continuation_requires_websocket", AsCodexSessionCapacityError(err).Code)
	require.Equal(t, 400, AsCodexSessionCapacityError(err).Status)
}
