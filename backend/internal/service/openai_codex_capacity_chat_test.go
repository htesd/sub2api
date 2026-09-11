package service

import (
	"net/http/httptest"
	"testing"

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
	for _, tc := range []struct{ path, body string }{
		{"/v1/responses", `{}`},
		{"/v1/responses/compact", `{}`},
		{"/v1/chat/completions", `{"previous_response_id":"resp_old"}`},
		{"/v1/chat/completions", `{"client_metadata":{"x-codex-parent-thread-id":"parent"}}`},
		{"/v1/chat/completions", `{"client_metadata":{"x-openai-subagent":"true"}}`},
	} {
		r := parse(tc.path, tc.body, "", "")
		require.Empty(t, r.input.Root, tc.body)
	}
}
