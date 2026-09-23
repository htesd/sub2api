package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAICodexDowngradeNormalization(t *testing.T) {
	require.Equal(t, openAICodexDowngradeDefaultMinutes, NormalizeOpenAICodexDowngradeMinutes(0))
	require.Equal(t, openAICodexDowngradeDefaultMinutes, NormalizeOpenAICodexDowngradeMinutes(-5))
	require.Equal(t, 45, NormalizeOpenAICodexDowngradeMinutes(45))
	require.Equal(t, openAICodexDowngradeMaxMinutes, NormalizeOpenAICodexDowngradeMinutes(99999))

	require.Equal(t, []string{"gpt-6-astra"}, NormalizeOpenAICodexDowngradeModels(""))
	require.Equal(t, []string{"gpt-6-astra"}, NormalizeOpenAICodexDowngradeModels("  GPT-6-Astra , gpt-6-astra\n"))
	require.Equal(t, []string{"gpt-6-astra", "gpt-6-nova"}, NormalizeOpenAICodexDowngradeModels("gpt-6-nova\ngpt-6-astra"))
}

func TestOpenAICodexDowngradeDetection(t *testing.T) {
	models := NormalizeOpenAICodexDowngradeModels("")
	require.True(t, openAICodexDowngradeModelWatched(models, "gpt-6-astra"))
	require.True(t, openAICodexDowngradeModelWatched(models, " GPT-6-ASTRA "))
	require.False(t, openAICodexDowngradeModelWatched(models, "gpt-5.6-sol"))
	require.False(t, openAICodexDowngradeModelWatched(models, ""))

	for _, tc := range []struct {
		name, payload, want string
	}{
		{"response_model", `{"type":"response.created","response":{"model":"gpt-5.6-luna"}}`, "gpt-5.6-luna"},
		{"top_level_model", `{"type":"response.created","model":"GPT-5.6-Luna"}`, "gpt-5.6-luna"},
		{"no_model", `{"type":"response.output_text.delta","delta":"x"}`, ""},
		{"malformed", `not-json`, ""},
		{"non_string_model", `{"response":{"model":123}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, openAICodexDowngradeObservedModel([]byte(tc.payload)))
		})
	}

	require.True(t, openAICodexDowngraded("gpt-6-astra", "gpt-5.6-luna"))
	require.False(t, openAICodexDowngraded("gpt-6-astra", "GPT-6-astra"))
	require.False(t, openAICodexDowngraded("gpt-6-astra", ""))
	require.False(t, openAICodexDowngraded("", "gpt-5.6-luna"))
}

// 降级判定必须冷却该 (账号, 模型)、抛出换号错误，并且不污染其他模型。
func TestOpenAICodexDowngradeStreamCheckCooldownAndFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := ticketTestAccount(77)
	account.Platform = PlatformOpenAI
	repo := &downgradeTestAccountRepo{}
	svc := &OpenAIGatewayService{accountRepo: repo}
	policy := OpenAICodexDowngradePolicy{Enabled: true, Cooldown: 30 * time.Minute, Models: NormalizeOpenAICodexDowngradeModels("")}

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	headers := http.Header{openAICodexSafetyBufferingModelHeader: []string{"gpt-5.6-luna"}}

	// 名单外的模型即使被换掉也不拦。
	require.Nil(t, svc.openAICodexDowngradeStreamCheck(context.Background(), c, account, "gpt-5.6-sol",
		[]byte(`{"type":"response.created","response":{"model":"gpt-5.6-luna"}}`), "req-1", headers, policy))
	require.Zero(t, repo.calls)

	// 模型一致时不拦。
	require.Nil(t, svc.openAICodexDowngradeStreamCheck(context.Background(), c, account, "gpt-6-astra",
		[]byte(`{"type":"response.created","response":{"model":"gpt-6-astra"}}`), "req-2", headers, policy))
	require.Zero(t, repo.calls)

	// 策略关闭时不拦。
	disabled := policy
	disabled.Enabled = false
	require.Nil(t, svc.openAICodexDowngradeStreamCheck(context.Background(), c, account, "gpt-6-astra",
		[]byte(`{"type":"response.created","response":{"model":"gpt-5.6-luna"}}`), "req-3", headers, disabled))
	require.Zero(t, repo.calls)

	failover := svc.openAICodexDowngradeStreamCheck(context.Background(), c, account, "gpt-6-astra",
		[]byte(`{"type":"response.created","response":{"model":"gpt-5.6-luna"}}`), "req-4", headers, policy)
	require.NotNil(t, failover)
	require.Equal(t, 1, repo.calls)
	require.Equal(t, account.ID, repo.accountID)
	require.Equal(t, "gpt-6-astra", repo.scope)
	require.Equal(t, openAICodexDowngradeReason, repo.reason)
	require.WithinDuration(t, time.Now().Add(30*time.Minute), repo.until, time.Minute)

	require.True(t, failover.ShouldRetryNextAccount())
	require.True(t, failover.ShouldReportAccountScheduleFailure())
	require.False(t, failover.RetryableOnSameAccount)
	require.False(t, failover.SafeToFailoverAfterWrite)
	require.Equal(t, http.StatusTooManyRequests, failover.ClientStatusCode)
	require.Equal(t, http.StatusBadGateway, failover.StatusCode)
	require.True(t, IsOpenAICodexDowngradeErrorBody(failover.ResponseBody))
	require.Contains(t, string(failover.ResponseBody), "gpt-5.6-luna")
	require.NotEmpty(t, failover.ClientMessage)

	// 生效冷却来自持久化的 (账号, 模型) 限流；进程内 transient 只负责连续失败时提前熔断，
	// 单次降级不会立即 block，这里明确固定这个边界，避免误以为单次就进程内拦住了。
	require.False(t, svc.isOpenAIAccountModelRuntimeBlocked(account, "gpt-5.6-sol"))
}

// 冷却写库失败不得吞掉换号：否则降级响应会直接回给客户端。
func TestOpenAICodexDowngradeFailoverSurvivesPersistError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := ticketTestAccount(78)
	account.Platform = PlatformOpenAI
	repo := &downgradeTestAccountRepo{err: context.DeadlineExceeded}
	svc := &OpenAIGatewayService{accountRepo: repo}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	failover := svc.openAICodexDowngradeStreamCheck(context.Background(), c, account, "gpt-6-astra",
		[]byte(`{"type":"response.created","response":{"model":"gpt-5.6-luna"}}`), "req-5", nil,
		OpenAICodexDowngradePolicy{Enabled: true, Cooldown: time.Minute, Models: NormalizeOpenAICodexDowngradeModels("")})
	require.NotNil(t, failover)
	require.Equal(t, 1, repo.calls)
}

func TestOpenAICodexDowngradeNonStreamCheck(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := ticketTestAccount(79)
	account.Platform = PlatformOpenAI
	policy := OpenAICodexDowngradePolicy{Enabled: true, Cooldown: time.Minute, Models: NormalizeOpenAICodexDowngradeModels("")}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"json_downgraded", `{"model":"gpt-5.6-luna","status":"completed"}`, true},
		{"json_expected", `{"model":"gpt-6-astra","status":"completed"}`, false},
		{"sse_downgraded", "data: {\"type\":\"response.created\",\"response\":{\"model\":\"gpt-5.6-luna\"}}\n\ndata: [DONE]\n\n", true},
		{"sse_expected", "data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-6-astra\"}}\n\n", false},
		{"no_model", `{"status":"completed"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &downgradeTestAccountRepo{}
			svc := &OpenAIGatewayService{accountRepo: repo}
			failover := svc.openAICodexDowngradeNonStreamCheck(context.Background(), c, account, "gpt-6-astra",
				[]byte(tc.body), "req", nil, policy)
			require.Equal(t, tc.want, failover != nil)
			if tc.want {
				require.True(t, IsOpenAICodexDowngradeErrorBody(failover.ResponseBody))
				require.Equal(t, 1, repo.calls)
			} else {
				require.Zero(t, repo.calls)
			}
		})
	}
}

func TestOpenAICodexDowngradeClientMessageMentionsRetry(t *testing.T) {
	message := OpenAICodexDowngradeClientMessage()
	require.Contains(t, message, "OpenAI")
	require.True(t, strings.Contains(message, "重试"))
}

type downgradeTestAccountRepo struct {
	AccountRepository
	calls     int
	accountID int64
	scope     string
	until     time.Time
	reason    string
	err       error
}

func (r *downgradeTestAccountRepo) SetModelRateLimit(_ context.Context, accountID int64, scope string, resetAt time.Time, reason ...string) error {
	r.calls++
	r.accountID = accountID
	r.scope = scope
	r.until = resetAt
	if len(reason) > 0 {
		r.reason = reason[0]
	}
	return r.err
}
