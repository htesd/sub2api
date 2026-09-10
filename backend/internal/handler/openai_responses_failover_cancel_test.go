//go:build unit

package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// openAIResponsesFailoverCancelUpstream 固定返回 HTTP 520，可在首次上游调用时
// 触发回调（用于模拟“上游在途期间客户端断开”）。
type openAIResponsesFailoverCancelUpstream struct {
	service.HTTPUpstream
	mu         sync.Mutex
	accountIDs []int64
	onFirstDo  func()
}

func (u *openAIResponsesFailoverCancelUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, accountID)
	first := len(u.accountIDs) == 1
	u.mu.Unlock()
	if first && u.onFirstDo != nil {
		u.onFirstDo()
	}
	return &http.Response{
		StatusCode: 520,
		Header:     http.Header{"Content-Type": []string{"text/html"}},
		Body:       io.NopCloser(bytes.NewBufferString("<html>520: unknown error</html>")),
	}, nil
}

func (u *openAIResponsesFailoverCancelUpstream) calls() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.accountIDs...)
}

func newOpenAIResponsesFailoverTestHandler(t *testing.T, upstream service.HTTPUpstream, configure ...func([]service.Account, *config.Config)) *OpenAIGatewayHandler {
	t.Helper()
	accounts := []service.Account{
		{
			ID:          1,
			Name:        "responses-account-1",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Concurrency: 0,
			Priority:    0,
			Credentials: map[string]any{"access_token": "token-1"},
		},
		{
			ID:          2,
			Name:        "responses-account-2",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Concurrency: 0,
			Priority:    1,
			Credentials: map[string]any{"access_token": "token-2"},
		},
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	for _, apply := range configure {
		apply(accounts, cfg)
	}
	accountRepo := openAIImagesFailoverAccountRepo{accounts: accounts}
	gatewayService := service.NewOpenAIGatewayService(
		accountRepo,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		nil,
		nil,
		nil,
		nil,
		nil,
		upstream,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	billingService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingService.Stop)
	concurrencyService := service.NewConcurrencyService(nil)
	handler := NewOpenAIGatewayHandler(
		gatewayService,
		concurrencyService,
		billingService,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil,
		nil,
		nil,
		nil,
		cfg,
	)
	handler.maxAccountSwitches = 10
	return handler
}

func newOpenAIResponsesFailoverTestContext(t *testing.T, ctx context.Context) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	groupID := int64(3131)
	body := []byte(`{"model":"gpt-5.1","stream":false,"input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID:      99,
		GroupID: &groupID,
		Group: &service.Group{
			ID:       groupID,
			Platform: service.PlatformOpenAI,
		},
		User: &service.User{ID: 100},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 0})
	return c, rec
}

// TestOpenAIGatewayHandlerResponses_FailoverAbortsWhenClientDisconnected 复现
// #4257：客户端在上游请求在途期间断开，上游随后返回可 failover 的 520。
// 期望：不再用已取消的 context 重新选号（不触达账号 2）、不把取消误报成
// 502 账号耗尽、请求按 499 归类。
func TestOpenAIGatewayHandlerResponses_FailoverAbortsWhenClientDisconnected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream := &openAIResponsesFailoverCancelUpstream{onFirstDo: cancel}
	handler := newOpenAIResponsesFailoverTestHandler(t, upstream)
	c, rec := newOpenAIResponsesFailoverTestContext(t, ctx)

	handler.Responses(c)

	require.Equal(t, []int64{1}, upstream.calls(), "客户端断开后不应再切换到账号 2")
	require.Equal(t, statusClientClosedRequest, c.Writer.Status(), "应按 499 归类")
	require.Zero(t, rec.Body.Len(), "不应写入 502 错误响应体")

	_, hasFinalUpstreamErr := c.Get(service.OpsUpstreamStatusCodeKey)
	require.False(t, hasFinalUpstreamErr, "不应记录 failover 耗尽的上游错误终态")

	// 真实发生过的 520 应保留 failover 事件（service 层在返回 failover 错误前记录）
	rawEvents, ok := c.Get(service.OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := rawEvents.([]*service.OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, events, 1)
	require.Equal(t, "failover", events[0].Kind)
	require.Equal(t, 520, events[0].UpstreamStatusCode)
}

// TestOpenAIGatewayHandlerResponses_FailoverContinuesForConnectedClient 回归
// 守卫：客户端在线时 failover 行为不变——切换到账号 2，两个账号都 520 后按
// 耗尽返回 502。
func TestOpenAIGatewayHandlerResponses_FailoverContinuesForConnectedClient(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &openAIResponsesFailoverCancelUpstream{}
	handler := newOpenAIResponsesFailoverTestHandler(t, upstream)
	c, rec := newOpenAIResponsesFailoverTestContext(t, nil)

	handler.Responses(c)

	require.Equal(t, []int64{1, 2}, upstream.calls(), "在线客户端应正常切换账号")
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "upstream_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
}

func TestOpenAIGatewayHandlerResponses_CapacityIdentityAdmission(t *testing.T) {
	for _, scenario := range []string{"missing", "thread_only", "stable_session", "compatible_device"} {
		t.Run(scenario, func(t *testing.T) {
			upstream := &openAIResponsesFailoverCancelUpstream{}
			handler := newOpenAIResponsesFailoverTestHandler(t, upstream, func(accounts []service.Account, _ *config.Config) {
				for i := range accounts {
					accounts[i].Credentials["chatgpt_account_id"] = fmt.Sprint("upstream-account-", i)
					accounts[i].Extra = map[string]any{"codex_fingerprint_mode": "capacity"}
				}
				if scenario == "compatible_device" {
					accounts[1].Extra["codex_fingerprint_mode"] = "device"
				}
			})
			c, rec := newOpenAIResponsesFailoverTestContext(t, nil)
			switch scenario {
			case "thread_only":
				c.Request.Header.Set("thread-id", "conversation-1")
			case "stable_session":
				c.Request.Header.Set("session-id", "conversation-1")
			}
			handler.Responses(c)
			switch scenario {
			case "missing", "thread_only":
				require.Equal(t, http.StatusBadRequest, rec.Code)
				require.Equal(t, "session_identity_required", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
				require.Contains(t, gjson.GetBytes(rec.Body.Bytes(), "error.message").String(), "reuse it across turns and retries")
				require.Empty(t, rec.Header().Get("Retry-After"))
				require.Empty(t, upstream.calls(), "本地身份校验失败不应触达任何上游账号")
			case "stable_session":
				require.Equal(t, []int64{1, 2}, upstream.calls(), "一个 session-id 即可准入；模拟上游 520 仍允许普通会话换号")
			case "compatible_device":
				require.Equal(t, []int64{2}, upstream.calls(), "缺少 session 身份仍可使用授权的 device 兼容账号")
			}
		})
	}
}

func TestOpenAIGatewayHandlerResponses_CodexBudgetStopsBeforeNextAccount(t *testing.T) {
	for _, nextEnabled := range []bool{false, true} {
		t.Run(fmt.Sprint("next_enabled_", nextEnabled), func(t *testing.T) {
			upstream := &openAIResponsesFailoverCancelUpstream{}
			handler := newOpenAIResponsesFailoverTestHandler(t, upstream, func(accounts []service.Account, _ *config.Config) {
				for i := range accounts {
					accounts[i].Credentials["chatgpt_account_id"] = fmt.Sprint("upstream-account-", i)
					accounts[i].Extra = map[string]any{
						"codex_fingerprint_mode": "capacity",
						"codex_request_policy":   map[string]any{"enabled": nextEnabled, "max_attempts": 20},
					}
				}
				accounts[0].Extra["codex_request_policy"] = map[string]any{"enabled": true, "max_attempts": 1}
			})
			c, rec := newOpenAIResponsesFailoverTestContext(t, nil)
			c.Request.Header.Set("session-id", "conversation-1")
			handler.Responses(c)
			require.Equal(t, []int64{1}, upstream.calls(), "第二个账号关闭策略或放宽预算都不能重置本次请求上限")
			require.Equal(t, http.StatusServiceUnavailable, rec.Code)
			require.Contains(t, rec.Body.String(), "codex_retry_budget_exhausted")
			require.Equal(t, "1", rec.Header().Get("Retry-After"))
		})
	}
}

func TestOpenAIGatewayHandlerResponsesWebSocket_CapacityIdentityAdmission(t *testing.T) {
	upstream := &openAIResponsesFailoverCancelUpstream{}
	handler := newOpenAIResponsesFailoverTestHandler(t, upstream, func(accounts []service.Account, cfg *config.Config) {
		cfg.Gateway.OpenAIWS.Enabled = true
		cfg.Gateway.OpenAIWS.OAuthEnabled = true
		cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
		cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
		for i := range accounts {
			accounts[i].Credentials["chatgpt_account_id"] = fmt.Sprint("upstream-account-", i)
			accounts[i].Extra = map[string]any{"codex_fingerprint_mode": "capacity", "responses_websockets_v2_enabled": true, "openai_oauth_responses_websockets_v2_mode": "ctx_pool"}
		}
	})
	template, _ := newOpenAIResponsesFailoverTestContext(t, nil)
	router := gin.New()
	router.GET("/v1/responses", func(c *gin.Context) {
		for key, value := range template.Keys {
			c.Set(key, value)
		}
		handler.ResponsesWebSocket(c)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	require.NoError(t, err)
	defer conn.CloseNow()
	require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","input":"hello"}`)))
	_, event, err := conn.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(400), gjson.GetBytes(event, "status").Int())
	require.Equal(t, "session_identity_required", gjson.GetBytes(event, "error.code").String())
	require.Contains(t, gjson.GetBytes(event, "error.message").String(), "reuse it across turns and retries")
	require.False(t, gjson.GetBytes(event, "retry_after").Exists())
	require.Empty(t, upstream.calls())
}
