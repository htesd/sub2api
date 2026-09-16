package service

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCodexCapacityChatIdentityAndStrictContinuation(t *testing.T) {
	parse := func(path, body, header, value string) *codexCapacityRequest {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", path, nil)
		c.Request.Header.Set(header, value)
		return parseCodexCapacityInput(c, []byte(body))
	}
	for _, header := range []string{"session-id", "session_id", "conversation_id", "X-Session-Id", "X-Conversation-ID", "thread-id"} {
		t.Run(header, func(t *testing.T) {
			r := parse("/v1/chat/completions", `{}`, header, "conversation")
			require.True(t, r.supported)
			require.Equal(t, "conversation", r.input.Root)
			require.Equal(t, "conversation", r.input.Thread)
		})
	}
	// Conversation headers identify the session; an independent explicit cache
	// key must not collapse different conversations into one capacity binding.
	r := parse("/v1/chat/completions", `{"prompt_cache_key":"shared-cache"}`, "X-Session-Id", "conversation")
	require.Equal(t, "conversation", r.input.Root)
	require.Equal(t, "shared-cache", r.originalCache)
	child := parse("/v1/chat/completions", `{"client_metadata":{"thread_id":"child","x-codex-parent-thread-id":"parent"}}`, "X-Session-Id", "conversation")
	require.Equal(t, "conversation", child.input.Root)
	require.Equal(t, "child", child.input.Thread)
	require.False(t, child.input.Mountable)
	a := parse("/v1/chat/completions", `{"messages":[{"role":"user","content":"same"}]}`, "", "")
	b := parse("/v1/chat/completions", `{"messages":[{"role":"user","content":"same"}]}`, "", "")
	require.NotEmpty(t, a.input.Root)
	require.NotEqual(t, a.input.Root, b.input.Root)
	require.True(t, a.input.Ephemeral)
	// 无标识请求在所有入口都合成独立身份（仅用于投影隔离），并标记为 ephemeral：
	// 准入开启时由 plan 明确要求客户端补稳定标识。
	for _, tc := range []struct{ path, body string }{
		{"/v1/responses", `{}`},
		{"/v1/responses/compact", `{}`},
		{"/v1/chat/completions", `{"client_metadata":{"x-openai-subagent":"true"}}`},
	} {
		r := parse(tc.path, tc.body, "", "")
		require.NotEmpty(t, r.input.Root, tc.body)
		require.Equal(t, r.input.Root, r.input.Thread, tc.body)
		require.True(t, r.input.Ephemeral, tc.body)
	}
	// 续接与显式父线程不合成身份：这两种请求必须绑定到真实存在的会话。
	for _, tc := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"previous_response_id":"resp_old"}`},
		{"/v1/chat/completions", `{"client_metadata":{"x-codex-parent-thread-id":"parent"}}`},
	} {
		r := parse(tc.path, tc.body, "", "")
		require.Empty(t, r.input.Root, tc.body)
		require.False(t, r.input.Ephemeral, tc.body)
	}
}

// 准入关闭（默认）时只做身份投影：合成身份可用、不占根配额、用完即回收；
// 准入开启时同一请求必须明确要求客户端提供稳定标识。
func TestCodexCapacityProjectionOnlyWithoutAdmission(t *testing.T) {
	now := time.Now()
	r := newCodexCapacityRegistry()
	p := defaultCodexCapacityPolicy()
	require.False(t, p.admissionEnabled())
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		in := capacityInput(1, fmt.Sprint("eph", i), fmt.Sprint("eph", i))
		in.Ephemeral = true
		l, err := r.reserve("account", p, in, false, now)
		require.NoError(t, err)
		require.False(t, l.Assignment.Synthetic, "投影层不挂合成子会话")
		require.False(t, seen[l.Assignment.SessionID])
		seen[l.Assignment.SessionID] = true
		l.sent(now)
		l.release()
	}
	require.Empty(t, r.accounts["account"].Bindings, "合成身份释放后不留绑定")

	admission := capacityAdmissionPolicy()
	in := capacityInput(1, "eph", "eph")
	in.Ephemeral = true
	_, err := r.reserve("account", admission, in, false, now)
	require.Equal(t, "session_identity_required", AsCodexSessionCapacityError(err).Code)
	require.Equal(t, 400, AsCodexSessionCapacityError(err).Status)
	require.Zero(t, AsCodexSessionCapacityError(err).RetryAfter, "400 不给重试提示")
	require.NotEqual(t, "session_identity_required", AsCodexSessionCapacityError(err).ClientMessage())
}

// 根计数与宿主资格必须同域：一个 api key 用满上限不能让另一个 key 既拿不到
// 新根也找不到宿主。
func TestCodexCapacityRootBudgetIsScopedPerAPIKey(t *testing.T) {
	now := time.Now()
	r := newCodexCapacityRegistry()
	p := capacityAdmissionPolicy()
	p.MaxRootSessions = 1
	first := exposeCapacity(t, r, p, capacityInput(1, "root-a", "root-a"), now)
	require.NotEmpty(t, first.Assignment.SessionID)
	_, err := r.reserve("account", p, capacityInput(1, "root-b", "root-b"), false, now)
	require.Equal(t, "session_roots_full", AsCodexSessionCapacityError(err).Code)
	other, err := r.reserve("account", p, capacityInput(2, "root-b", "root-b"), false, now)
	require.NoError(t, err, "另一个 api key 有独立的根预算")
	other.release()
}

// 绑定表是内存上界而不是策略上限：达到上界淘汰最旧的空闲绑定，不能硬拒到重启。
func TestCodexCapacityEvictsIdleBindingsInsteadOfRejecting(t *testing.T) {
	now := time.Now()
	r := newCodexCapacityRegistry()
	p := capacityAdmissionPolicy()
	p.MaxRootSessions = 100000
	a := &codexCapacityAccount{Roots: map[string]codexCapacityRoot{}, Bindings: map[codexCapacityBindingKey]*codexCapacityBinding{}}
	for i := 0; i < codexCapacityMaxBindings; i++ {
		a.Bindings[codexCapacityBindingKey{1, fmt.Sprint("thread", i)}] = &codexCapacityBinding{LastSent: now.Add(-time.Duration(i) * time.Second)}
	}
	r.accounts["account"] = a
	l, err := r.reserve("account", p, capacityInput(1, "fresh", "fresh"), false, now)
	require.NoError(t, err)
	require.Len(t, a.Bindings, codexCapacityMaxBindings)
	l.release()
}
