package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type codexControlTestUpstream struct {
	HTTPUpstream
	calls atomic.Int32
}

func (u *codexControlTestUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls.Add(1)
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
}
func TestCodexControlStopsActualHTTPTransport(t *testing.T) {
	c, a, s := capacityContext(t, []byte(`{"model":"gpt-5.5"}`))
	enableCodexControl(a, nil)
	upstream := &codexControlTestUpstream{}
	s.httpUpstream = upstream
	for i := 0; i < 4; i++ {
		request, err := http.NewRequestWithContext(c.Request.Context(), "POST", "https://chatgpt.com/backend-api/codex/responses", nil)
		require.NoError(t, err)
		request.Header.Set(openAICodexRoutingHintHeader, "model=gpt-5.5")
		response, err := s.doOpenAIUpstream(request, "", a)
		if i < 3 {
			require.NoError(t, err)
			response.Body.Close()
		} else {
			require.True(t, isCodexControlStop(err))
			require.Nil(t, response)
		}
	}
	require.Equal(t, int32(3), upstream.calls.Load())
}

func enableCodexControl(a *Account, extra map[string]any) {
	p := map[string]any{"enabled": true, "max_attempts": 3, "retry_window_seconds": 30, "cooldown_seconds": 1}
	for k, v := range extra {
		p[k] = v
	}
	a.Extra["codex_request_policy"] = p
}

func TestCodexControlBudgetSharedAcrossAccountsAndTransports(t *testing.T) {
	c, a, s := capacityContext(t, []byte(`{"model":"gpt-5.5"}`))
	enableCodexControl(a, nil)
	ctx := c.Request.Context()
	for _, kind := range []string{"ws_dial", "ws_send", "http"} {
		done, err := s.beginCodexAttempt(ctx, a, "gpt-5.5", kind, nil)
		require.NoError(t, err)
		done(200, nil)
	}
	b := *a
	b.ID = 2
	b.Extra = map[string]any{}
	_, err := s.beginCodexAttempt(ctx, &b, "gpt-5.5", "http", nil)
	require.True(t, isCodexControlStop(err))
	var stop *UpstreamFailoverError
	require.True(t, errors.As(err, &stop))
	require.False(t, stop.ShouldRetryNextAccount())
	require.False(t, stop.ShouldReportAccountScheduleFailure())
	resetCodexControlTurn(ctx, 1)
	_, err = s.beginCodexAttempt(ctx, a, "gpt-5.5", "http", nil)
	require.Error(t, err)
	resetCodexControlTurn(ctx, 2)
	done, err := s.beginCodexAttempt(ctx, a, "gpt-5.5", "ws_send", nil)
	require.NoError(t, err)
	done(0, nil)
	require.Equal(t, 4, codexControl(ctx).total)
}

func TestCodexControlConcurrentBudgetAndCancellation(t *testing.T) {
	c, a, s := capacityContext(t, []byte(`{}`))
	enableCodexControl(a, nil)
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done, err := s.beginCodexAttempt(c.Request.Context(), a, "gpt-5.5", "http", nil)
			if err == nil {
				admitted.Add(1)
				done(200, nil)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(3), admitted.Load())
	ctx, cancel := context.WithCancel(context.Background())
	c.Request = c.Request.WithContext(WithCodexSessionCapacity(ctx, c, []byte(`{}`)))
	cancel()
	_, err := s.beginCodexAttempt(context.WithoutCancel(c.Request.Context()), a, "gpt-5.5", "http", nil)
	require.ErrorIs(t, err, context.Canceled)
}

func TestCodexControlWindowDoesNotCancelActiveResponse(t *testing.T) {
	c, a, s := capacityContext(t, []byte(`{}`))
	enableCodexControl(a, nil)
	done, err := s.beginCodexAttempt(c.Request.Context(), a, "gpt-5.5", "http", nil)
	require.NoError(t, err)
	r := codexControl(c.Request.Context())
	r.started = time.Now().Add(-time.Minute)
	require.NoError(t, c.Request.Context().Err())
	done(200, nil)
	_, err = s.beginCodexAttempt(c.Request.Context(), a, "gpt-5.5", "http", nil)
	require.True(t, isCodexControlStop(err))
}

func TestCodexControlRetryAfterAndGroupModelCooldown(t *testing.T) {
	c, a, s := capacityContext(t, []byte(`{}`))
	enableCodexControl(a, nil)
	ctx := c.Request.Context()
	done, err := s.beginCodexAttempt(ctx, a, "gpt-5.5", "http", nil)
	require.NoError(t, err)
	done(503, nil)
	s.noteCodexOverload(ctx, a, "gpt-5.5", 503, http.Header{"Retry-After": {"120"}}, nil)
	_, err = s.beginCodexAttempt(ctx, a, "gpt-5.5", "http", nil)
	require.True(t, isCodexControlStop(err))
	require.Equal(t, 1, codexControl(ctx).attempts)
	rt := s.codexControls()
	now := time.Now()
	for _, id := range []int64{2, 2, 3} {
		rt.overload(9, "model", id, time.Second, now)
	}
	require.Zero(t, rt.cooldown(9, "model", 99, now))
	rt.overload(9, "model", 4, time.Second, now)
	require.Positive(t, rt.cooldown(9, "model", 99, now))
	require.Zero(t, rt.cooldown(10, "model", 99, now))
	require.Zero(t, rt.cooldown(9, "other-model", 99, now))
}

func TestCodexControlHeaderDiagnosticsNeverContainTokensOrRawIdentity(t *testing.T) {
	in := http.Header{"Authorization": {"Bearer PRIVATE"}, "X-Oai-Attestation": {`{"v":1,"s":0,"t":"PRIVATE_ATTESTATION"}`}, "X-Codex-Turn-State": {"PRIVATE_STATE"}, "User-Agent": {"PRIVATE_UA"}, "X-Codex-Turn-Metadata": {"PRIVATE_METADATA"}}
	encoded, err := json.Marshal(codexHeaderDiagnostics(in, nil))
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "PRIVATE")
	require.Contains(t, string(encoded), "token_returned")
	for raw, want := range map[string]string{`{"v":1,"s":1}`: "timeout", `{"v":1,"s":0}`: "missing_token", `{"v":1,"s":99}`: "unrecognized", `{bad`: "unrecognized", strings.Repeat("x", 17000): "oversize"} {
		require.Equal(t, want, codexAttestationState(http.Header{"X-Oai-Attestation": {raw}}))
	}
}

func TestCodexControlIdentityPreserveAndFallback(t *testing.T) {
	c, a, _ := capacityContext(t, []byte(`{}`))
	enableCodexControl(a, map[string]any{"identity_mode": "preserve"})
	c.Request.Header.Set("User-Agent", "codex_exec/0.153.4 (Ubuntu 26.4.0; x86_64) VTE/8400")
	c.Request.Header.Set("originator", "wrong")
	c.Request.Header.Set("version", "999.0")
	h := http.Header{"Authorization": {"Bearer target"}, "Originator": {"codex-tui"}}
	applyCodexIdentityPolicy(c, a, h)
	require.Equal(t, "codex_exec", h.Get("originator"))
	require.Equal(t, "0.153.4", h.Get("version"))
	require.Equal(t, "Bearer target", h.Get("Authorization"))
	for _, ua := range []string{"Mozilla/5.0", "codex_exec/0.1.0", strings.Repeat("x", 600)} {
		c.Request.Header.Set("User-Agent", ua)
		h.Set("User-Agent", "canonical")
		applyCodexIdentityPolicy(c, a, h)
		require.Equal(t, "canonical", h.Get("User-Agent"))
	}
}

func TestCodexControlCapacityQueueAdmitsAfterExposureExpires(t *testing.T) {
	c, a, s := capacityContext(t, []byte(`{"model":"gpt-5.1","prompt_cache_key":"new"}`))
	enableCodexControl(a, map[string]any{"capacity_mode": "queue", "capacity_wait_seconds": 3})
	a.Extra["codex_session_capacity"] = map[string]any{"max_root_sessions": 1, "window_seconds": 1}
	s.accountRepo = schedulerTestOpenAIAccountRepo{accounts: []Account{*a}}
	policy, _ := a.codexCapacityPolicy()
	require.False(t, policy.SubagentFallbackEnabled)
	host, err := s.codexCapacityRegistry().reserve(codexAccountIdentityNamespace(a), policy, capacityInput(1, "old", "old"), false, time.Now())
	require.NoError(t, err)
	host.sent(time.Now())
	host.release()
	selection, _, err := s.SelectAccountWithScheduler(c.Request.Context(), nil, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
	require.False(t, capacityLeaseForAccount(c, selection.Account).Assignment.Synthetic)
	require.Zero(t, s.codexControls().waiters)
}

func TestCodexControlCapacityQueueCancellationReleasesWaiter(t *testing.T) {
	c, a, s := capacityContext(t, []byte(`{"model":"gpt-5.1","prompt_cache_key":"new"}`))
	enableCodexControl(a, map[string]any{"capacity_mode": "queue", "capacity_wait_seconds": 10})
	a.Extra["codex_session_capacity"] = map[string]any{"max_root_sessions": 1}
	s.accountRepo = schedulerTestOpenAIAccountRepo{accounts: []Account{*a}}
	policy, _ := a.codexCapacityPolicy()
	host, err := s.codexCapacityRegistry().reserve(codexAccountIdentityNamespace(a), policy, capacityInput(1, "old", "old"), false, time.Now())
	require.NoError(t, err)
	host.sent(time.Now())
	host.release()
	ctx, cancel := context.WithTimeout(c.Request.Context(), 50*time.Millisecond)
	defer cancel()
	_, _, err = s.SelectAccountWithScheduler(ctx, nil, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Zero(t, s.codexControls().waiters)
}

func TestCodexControlStopsActualWSDial(t *testing.T) {
	c, a, s := capacityContext(t, []byte("{}"))
	enableCodexControl(a, nil)
	pool := newOpenAIWSConnPool(s.cfg)
	dialer := &openAIWSCountingDialer{}
	pool.setClientDialerForTest(dialer)
	req := openAIWSAcquireRequest{Account: a, WSURL: "wss://example.com/v1/responses", BeforeDial: func(ctx context.Context, h http.Header) (func(int, http.Header, error), error) {
		return s.beginCodexDial(ctx, a, h)
	}}
	for i := 0; i < 4; i++ {
		conn, err := pool.dialConn(c.Request.Context(), req)
		if i < 3 {
			require.NoError(t, err)
			conn.close()
		} else {
			require.True(t, isCodexControlStop(err))
			require.Nil(t, conn)
		}
	}
	require.Equal(t, 3, dialer.DialCount())
}
func TestCodexControlQueueDeadlinePreventsLateAdmission(t *testing.T) {
	c, a, s := capacityContext(t, []byte(`{"model":"gpt-5.1","prompt_cache_key":"new"}`))
	enableCodexControl(a, map[string]any{"capacity_mode": "queue", "capacity_wait_seconds": 1})
	a.Extra["codex_session_capacity"] = map[string]any{"max_root_sessions": 1, "window_seconds": 1}
	s.accountRepo = schedulerTestOpenAIAccountRepo{accounts: []Account{*a}}
	p, _ := a.codexCapacityPolicy()
	host, err := s.codexCapacityRegistry().reserve(codexAccountIdentityNamespace(a), p, capacityInput(1, "old", "old"), false, time.Now())
	require.NoError(t, err)
	host.sent(time.Now())
	host.release()
	selection, _, err := s.SelectAccountWithScheduler(c.Request.Context(), nil, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.Nil(t, selection)
	require.Equal(t, "session_queue_timeout", AsCodexSessionCapacityError(err).Code)
	require.Zero(t, s.codexControls().waiters)
}

func TestCodexControlRetryReentryPreservesTurnBudget(t *testing.T) {
	c, a, s := capacityContext(t, []byte(`{}`))
	enableCodexControl(a, nil)
	ctx := c.Request.Context()
	resetCodexControlTurn(ctx, 4)
	for i := 0; i < 3; i++ {
		_, err := s.beginCodexAttempt(ctx, a, "gpt-5.5", "ws_send", nil)
		require.NoError(t, err)
	}
	base := codexControlTurnBase(ctx)
	resetCodexControlTurn(ctx, base+1)
	_, err := s.beginCodexAttempt(ctx, a, "gpt-5.5", "ws_send", nil)
	require.True(t, isCodexControlStop(err))
	resetCodexControlTurn(ctx, base+2)
	_, err = s.beginCodexAttempt(ctx, a, "gpt-5.5", "ws_send", nil)
	require.NoError(t, err)
	require.Equal(t, 5, codexControl(ctx).turn)
}
func TestCodexControlPreservedWSIdentitySeparatesPoolCompatibility(t *testing.T) {
	_, a, _ := capacityContext(t, []byte(`{}`))
	enableCodexControl(a, map[string]any{"identity_mode": "preserve"})
	x := http.Header{"User-Agent": {"codex_exec/0.153.4"}, "Originator": {"codex_exec"}, "Version": {"0.153.4"}}
	y := x.Clone()
	y.Set("User-Agent", "codex-tui/0.154.0")
	y.Set("Originator", "codex_cli_rs")
	y.Set("Version", "0.154.0")
	require.NotEqual(t, normalizeOpenAIWSHandshakeCompatibility(a, x), normalizeOpenAIWSHandshakeCompatibility(a, y))
}
func TestCodexControlStreamFailureActivatesCooldown(t *testing.T) {
	c, a, s := capacityContext(t, []byte(`{}`))
	enableCodexControl(a, nil)
	ctx := c.Request.Context()
	_, err := s.beginCodexAttempt(ctx, a, "gpt-5.5", "ws_send", nil)
	require.NoError(t, err)
	payload := []byte(`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_is_overloaded","message":"Server overloaded"}}}`)
	s.handleOpenAIWSTerminalTransientFailure(ctx, a, "gpt-5.5", nil, payload)
	require.Greater(t, s.codexControls().cooldown(0, "gpt-5.5", a.ID, time.Now()), time.Duration(0))
	r := codexControl(ctx)
	r.mu.Lock()
	require.Equal(t, "server_is_overloaded", codexForwardFailureCode(r, &OpenAIForwardResult{OpenAIWSMode: true, UpstreamTerminalEvent: "response.failed"}, nil))
	r.mu.Unlock()
}
func TestCodexControlHTTPHeadersAreOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, passthrough := range []bool{false, true} {
			c, a, s := capacityContext(t, []byte(`{"model":"gpt-5.5"}`))
			a.Extra = map[string]any{}
			if enabled {
				enableCodexControl(a, nil)
			}
			for _, name := range []string{"session-id", "thread-id", "x-client-request-id"} {
				c.Request.Header.Set(name, "test-client-session")
			}
			var req *http.Request
			var err error
			if passthrough {
				req, err = s.buildUpstreamRequestOpenAIPassthrough(c.Request.Context(), c, a, []byte(`{"model":"gpt-5.5"}`), "dummy-token")
			} else {
				req, err = s.buildUpstreamRequest(c.Request.Context(), c, a, []byte(`{"model":"gpt-5.5"}`), "dummy-token", false, "", true)
			}
			require.NoError(t, err)
			for _, name := range []string{"session-id", "thread-id", "x-client-request-id"} {
				if enabled {
					require.NotEmpty(t, req.Header.Get(name))
					require.NotEqual(t, "test-client-session", req.Header.Get(name))
				} else {
					require.Empty(t, req.Header.Get(name))
				}
			}
		}
	}
}

func TestCodexControlRealIngressResetsNextTurn(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModePassthrough, OpenAIWSIngressModeCtxPool} {
		t.Run(mode, func(t *testing.T) {
			c, a, _ := capacityContext(t, []byte(`{}`))
			a.Extra = map[string]any{"openai_oauth_responses_websockets_v2_mode": mode}
			enableCodexControl(a, map[string]any{"max_attempts": 2})
			ctx, cancel := context.WithCancel(c.Request.Context())
			defer cancel()
			upstream := newStagedPassthroughConn()
			cfg := passthroughLifecycleConfig()
			cfg.Gateway.OpenAIWS.OAuthEnabled = true
			svc := newPassthroughLifecycleService(cfg, upstream)
			svc.openaiWSPool = newOpenAIWSConnPool(cfg)
			svc.openaiWSPool.setClientDialerForTest(&stagedPassthroughDialer{conn: &codexControlStagedConn{upstream}})
			server, _ := startPassthroughHookRecordingServer(t, ctx, svc, a, nil)
			defer server.Close()
			client := dialPassthroughLifecycleClient(t, server)
			defer client.CloseNow()
			require.NotEmpty(t, requirePassthroughUpstreamWrite(t, upstream, time.Second))
			upstream.Send(`{"type":"response.completed","response":{"id":"resp_control_1","model":"gpt-5.1"}}`)
			_, err := readPassthroughLifecycleFrame(t, client, 3*time.Second)
			require.NoError(t, err)
			require.NoError(t, client.Write(ctx, 2, []byte(`{"type":"response.create","model":"gpt-5.1"}`)))
			require.NotEmpty(t, requirePassthroughUpstreamWrite(t, upstream, time.Second))
			upstream.Send(`{"type":"response.completed","response":{"id":"resp_control_2","model":"gpt-5.1"}}`)
			_, err = readPassthroughLifecycleFrame(t, client, 3*time.Second)
			require.NoError(t, err)
			r := codexControl(ctx)
			r.mu.Lock()
			require.Equal(t, 3, r.total)
			require.Equal(t, 1, r.attempts)
			require.Equal(t, 2, r.turn)
			r.mu.Unlock()
		})
	}
}
func TestCodexControlRealPassthroughIngressBudgetStopsBeforeSend(t *testing.T) {
	c, a, _ := capacityContext(t, []byte(`{}`))
	a.Extra = map[string]any{"openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModePassthrough}
	enableCodexControl(a, map[string]any{"max_attempts": 1})
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	upstream := newStagedPassthroughConn()
	cfg := passthroughLifecycleConfig()
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	svc := newPassthroughLifecycleService(cfg, upstream)
	server, serverErr := startPassthroughHookRecordingServer(t, ctx, svc, a, nil)
	defer server.Close()
	client := dialPassthroughLifecycleClient(t, server)
	defer client.CloseNow()
	select {
	case err := <-serverErr:
		require.True(t, isCodexControlStop(err))
	case <-time.After(3 * time.Second):
		t.Fatal("control did not stop")
	}
	select {
	case <-upstream.writes:
		t.Fatal("request bypassed budget")
	default:
	}
}

func TestCodexControlRejectedQueueCandidateDoesNotConfigureAvailableAccount(t *testing.T) {
	c, a, s := capacityContext(t, []byte(`{"model":"gpt-5.1","prompt_cache_key":"new"}`))
	enableCodexControl(a, map[string]any{"capacity_mode": "queue", "capacity_wait_seconds": 10, "max_attempts": 1})
	a.Priority = 1
	a.Extra["codex_session_capacity"] = map[string]any{"max_root_sessions": 1}
	b := *a
	b.ID = 2
	b.Priority = 2
	b.Extra = map[string]any{"codex_fingerprint_mode": "capacity", "codex_fingerprint_seed": "00000000-0000-4000-8000-000000000002"}
	b.Credentials = map[string]any{"chatgpt_account_id": "other"}
	s.accountRepo = schedulerTestOpenAIAccountRepo{accounts: []Account{*a, b}}
	p, _ := a.codexCapacityPolicy()
	host, err := s.codexCapacityRegistry().reserve(codexAccountIdentityNamespace(a), p, capacityInput(1, "old", "old"), false, time.Now())
	require.NoError(t, err)
	host.sent(time.Now())
	host.release()
	selection, _, err := s.SelectAccountWithScheduler(c.Request.Context(), nil, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.Equal(t, int64(2), selection.Account.ID)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
	require.False(t, codexControl(c.Request.Context()).configured)
	require.Zero(t, s.codexControls().waiters)
}

type codexControlStagedConn struct{ *stagedPassthroughConn }

func (c *codexControlStagedConn) WriteJSON(ctx context.Context, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.WriteFrame(ctx, 1, b)
}

func TestCodexControlHTTPStreamOverloadAfterOutput(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		c, a, s := capacityContext(t, []byte(`{}`))
		a.Extra = map[string]any{}
		enableCodexControl(a, nil)
		ctx := c.Request.Context()
		s.cfg.Gateway.MaxLineSize = defaultMaxLineSize
		_, err := s.beginCodexAttempt(ctx, a, "gpt-5.5", "http", nil)
		require.NoError(t, err)
		body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_test\",\"status\":\"failed\",\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"Server overloaded\"}}}\n\n"
		resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
		if passthrough {
			_, err = s.handleStreamingResponsePassthrough(ctx, resp, c, a, time.Now(), "gpt-5.5", "gpt-5.5")
		} else {
			_, err = s.handleStreamingResponse(ctx, resp, c, a, time.Now(), "gpt-5.5", "gpt-5.5")
		}
		require.Error(t, err)
		require.Greater(t, s.codexControls().cooldown(0, "gpt-5.5", a.ID, time.Now()), time.Duration(0))
		r := codexControl(ctx)
		r.mu.Lock()
		require.Equal(t, "server_is_overloaded", codexForwardFailureCode(r, nil, err))
		r.mu.Unlock()
	}
}
func TestCodexControlStreamCooldownUsesCurrentUpstreamModel(t *testing.T) {
	c, a, s := capacityContext(t, []byte(`{}`))
	enableCodexControl(a, nil)
	ctx := c.Request.Context()
	_, err := s.beginCodexAttempt(ctx, a, "model-b", "ws_send", nil)
	require.NoError(t, err)
	s.observeCodexStreamFailure(ctx, a, "model-a", nil, []byte(`{"type":"response.failed","response":{"error":{"code":"server_is_overloaded","message":"Server overloaded"}}}`))
	require.Zero(t, s.codexControls().cooldown(0, "model-a", a.ID, time.Now()))
	require.Greater(t, s.codexControls().cooldown(0, "model-b", a.ID, time.Now()), time.Duration(0))
}

func TestCodexControlNativeWSBareErrorActivatesCooldown(t *testing.T) {
	c, a, s := capacityContext(t, []byte(`{}`))
	enableCodexControl(a, nil)
	ctx := c.Request.Context()
	_, err := s.beginCodexAttempt(ctx, a, "gpt-5.5", "ws_send", nil)
	require.NoError(t, err)
	s.handleOpenAIWSErrorEventTransientFailure(ctx, a, "gpt-5.5", nil, []byte(`{"type":"error","error":{"code":"server_is_overloaded","message":"Server overloaded"}}`))
	require.Greater(t, s.codexControls().cooldown(0, "gpt-5.5", a.ID, time.Now()), time.Duration(0))
	r := codexControl(ctx)
	r.mu.Lock()
	require.Equal(t, "server_is_overloaded", codexForwardFailureCode(r, nil, errors.New("flattened error")))
	r.mu.Unlock()
}
