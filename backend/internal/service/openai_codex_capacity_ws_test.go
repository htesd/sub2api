package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCodexCapacityWSMultiTurnWire(t *testing.T) { testCodexCapacityWSMultiTurnWire(t, false) }
func TestCodexCapacityWSLaterFailureCannotReplayFirstTurn(t *testing.T) {
	testCodexCapacityWSMultiTurnWire(t, true)
}
func testCodexCapacityWSMultiTurnWire(t *testing.T, failLater bool) {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	capture := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp_first","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`), []byte(`{"type":"response.completed","response":{"id":"resp_second","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`)}}
	if failLater {
		capture.events[1] = []byte(`{"type":"error","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"rate limit exceeded"}}`)
	}
	pool := newOpenAIWSConnPool(cfg)
	dialer := &openAIWSCaptureDialer{conn: capture}
	pool.setClientDialerForTest(dialer)
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), openaiWSPool: pool}
	account := &Account{ID: 114, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"chatgpt_account_id": "wire-account"}, Extra: map[string]any{"codex_fingerprint_mode": "capacity", "codex_fingerprint_seed": "00000000-0000-4000-8000-000000000001", "responses_websockets_v2_enabled": true, "openai_oauth_responses_websockets_v2_mode": "ctx_pool"}}
	registry := svc.codexCapacityRegistry()
	identity := codexAccountIdentityNamespace(account)
	policy := defaultCodexCapacityPolicy()
	policy.MaxRootSessions = 1
	host, err := registry.reserve(identity, policy, capacityInput(1, "root", "host-thread"), false, time.Now())
	require.NoError(t, err)
	host.sent(time.Now())
	host.release()
	done := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			done <- err
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		_, first, err := conn.Read(ctx)
		if err != nil {
			done <- err
			return
		}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = r.Clone(ctx)
		groupID := int64(1)
		c.Set("api_key", &APIKey{ID: 1, GroupID: &groupID})
		SetOpenAIClientTransport(c, OpenAIClientTransportWS)
		ctx = WithCodexSessionCapacity(ctx, c, first)
		defer ReleaseCodexSessionCapacity(c)
		state := codexCapacityFromGin(c)
		lease, err := registry.reserve(identity, policy, state.input, true, time.Now())
		if err != nil {
			done <- err
			return
		}
		state.stage(account.ID, lease, lease.release)
		preemptCtx, cleanup, armed := svc.BeginOpenAIWSIngressSessionPreemption(ctx, c, account, first)
		cleanup()
		if armed || preemptCtx.Err() != nil {
			done <- fmt.Errorf("capacity must not arm shared session preemption")
			return
		}
		// Neither a legacy unscoped cache entry nor another credential's
		// scoped entry may supply this account's handshake routing state.
		sessionHash := svc.GenerateSessionHash(c, first)
		store := svc.getOpenAIWSStateStore()
		store.BindSessionTurnState(1, sessionHash, "foreign-unscoped-token", time.Minute)
		store.BindSessionTurnState(1, codexCapacityNode("other-account", 1, "ws-state:"+sessionHash), "foreign-scoped-token", time.Minute)
		done <- svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "test-token", first, nil)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	require.NoError(t, err)
	defer client.CloseNow()
	for i := 1; i <= 2; i++ {
		previous := ""
		if i == 2 {
			previous = `,"previous_response_id":"resp_first"`
		}
		body := fmt.Sprintf(`{"type":"response.create","model":"gpt-5.1","store":false,"prompt_cache_key":"child","client_metadata":{"session_id":"child","thread_id":"child","x-codex-turn-metadata":"{\"turn_id\":\"turn-%d\"}"}%s}`, i, previous)
		require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(body)))
		_, response, err := client.Read(ctx)
		if failLater && i == 2 {
			require.Error(t, err)
			break
		}
		require.NoError(t, err)
		require.Equal(t, "response.completed", gjson.GetBytes(response, "type").String())
	}
	_ = client.Close(coderws.StatusNormalClosure, "done")
	select {
	case err := <-done:
		if failLater {
			var closeErr *OpenAIWSClientCloseError
			require.ErrorAs(t, err, &closeErr)
			require.Equal(t, "session_retry_requires_full_history", closeErr.Reason())
			var retryErr *UpstreamFailoverError
			require.False(t, errors.As(err, &retryErr), "handler must not retry its retained first frame")
		} else {
			require.NoError(t, err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.Len(t, capture.writes, 2)
	require.Empty(t, dialer.lastHeaders.Get(openAICodexTurnStateHeader))
	for i, raw := range capture.writes {
		body := payloadAsJSONBytes(raw)
		require.Equal(t, host.Assignment.SessionID, gjson.GetBytes(body, "prompt_cache_key").String())
		require.Equal(t, host.Assignment.ThreadID, gjson.GetBytes(body, "client_metadata.x-codex-parent-thread-id").String())
		require.Equal(t, "collab_spawn", gjson.GetBytes(body, "client_metadata.x-openai-subagent").String())
		turn := gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String()
		require.Equal(t, codexCapacityNode(identity, 1, fmt.Sprintf("turn-%d", i+1)), gjson.Get(turn, "turn_id").String())
	}
	require.Equal(t, "resp_first", gjson.GetBytes(payloadAsJSONBytes(capture.writes[1]), "previous_response_id").String())
}
