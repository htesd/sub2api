//go:build unit

package handler

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type chatCapacityCapture struct {
	service.HTTPUpstream
	bodies         []string
	headers        []http.Header
	paths          []string
	accountIDs     []int64
	failures       int
	failureStatus  int
	responseHeader http.Header
}

func (u *chatCapacityCapture) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	u.bodies = append(u.bodies, string(body))
	u.headers = append(u.headers, req.Header.Clone())
	u.paths = append(u.paths, req.URL.Path)
	u.accountIDs = append(u.accountIDs, accountID)
	if u.failures > 0 {
		u.failures--
		if u.failureStatus == 429 {
			return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"1"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow down"}}`))}, nil
		}
		return &http.Response{StatusCode: 520, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("upstream unavailable"))}, nil
	}
	events := `data: {"type":"response.created","response":{"id":"resp_chat","model":"gpt-5.5","status":"in_progress","output":[]}}

data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"OK"}

data: {"type":"response.completed","response":{"id":"resp_chat","model":"gpt-5.5","status":"completed","output":[{"type":"message","id":"msg_chat","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}}

data: [DONE]

`
	headers := u.responseHeader.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", "text/event-stream")
	return &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(events))}, nil
}

func configureChatCapacityAccounts(accounts []service.Account) {
	for i := range accounts {
		accounts[i].Credentials["chatgpt_account_id"] = fmt.Sprintf("chat-capacity-account-%d", i)
		accounts[i].Extra = map[string]any{
			"codex_fingerprint_mode": "capacity",
			"codex_fingerprint_seed": "00000000-0000-4000-8000-000000000001",
			"codex_session_capacity": map[string]any{"max_root_sessions": 1, "subagent_fallback_enabled": false},
		}
	}
	// A single eligible account makes the capacity boundary deterministic.
	accounts[1].Schedulable = false
}

func chatCapacityContext(t *testing.T, body, session string) (*gin.Context, *httptest.ResponseRecorder) {
	c, rec := newOpenAIResponsesFailoverTestContext(t, nil)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	if session != "" {
		c.Request.Header.Set("X-Session-Id", session)
	}
	return c, rec
}

func TestChatCapacityConvertsBothResponseModes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			upstream := &chatCapacityCapture{}
			h := newOpenAIResponsesFailoverTestHandler(t, upstream, configureChatCapacityAccounts)
			body := fmt.Sprintf(`{"model":"gpt-5.5","stream":%t,"messages":[{"role":"user","content":"hello"}]}`, stream)
			c, rec := chatCapacityContext(t, body, "conversation-one")
			h.ChatCompletions(c)
			require.Equal(t, 200, rec.Code, rec.Body.String())
			require.Len(t, upstream.bodies, 1)
			require.Equal(t, "/backend-api/codex/responses", upstream.paths[0])
			sent := gjson.Parse(upstream.bodies[0])
			require.False(t, sent.Get("messages").Exists())
			require.Contains(t, sent.Get("input").Raw, "hello")
			require.True(t, sent.Get("stream").Bool())
			session := sent.Get("client_metadata.session_id").String()
			require.NotEmpty(t, session)
			require.Equal(t, session, upstream.headers[0].Get("session_id"), "cache injection must not overwrite admitted session")
			require.Equal(t, sent.Get("client_metadata.thread_id").String(), upstream.headers[0].Get("thread-id"))
			require.Equal(t, session, sent.Get("prompt_cache_key").String())
			installation := gjson.Get(sent.Get("client_metadata.x-codex-turn-metadata").String(), "installation_id").String()
			require.NotEmpty(t, installation)
			require.Equal(t, installation, upstream.headers[0].Get("x-codex-installation-id"))
			if stream {
				require.Contains(t, rec.Body.String(), `"object":"chat.completion.chunk"`)
				require.Contains(t, rec.Body.String(), `"content":"OK"`)
				require.Contains(t, rec.Body.String(), "data: [DONE]")
			} else {
				require.Equal(t, "chat.completion", gjson.GetBytes(rec.Body.Bytes(), "object").String())
				require.Equal(t, "OK", gjson.GetBytes(rec.Body.Bytes(), "choices.0.message.content").String())
				require.Equal(t, "stop", gjson.GetBytes(rec.Body.Bytes(), "choices.0.finish_reason").String())
			}
			// Follow-up requests carrying the same conversation identity reuse capacity.
			next, nextRec := chatCapacityContext(t, body, "conversation-one")
			h.ChatCompletions(next)
			require.Equal(t, 200, nextRec.Code, nextRec.Body.String())
			require.Len(t, upstream.bodies, 2)
			require.Equal(t, session, upstream.headers[1].Get("session_id"))
		})
	}
}

func TestChatCapacityStatelessRequestsCountIndependentlyAndReturn429(t *testing.T) {
	upstream := &chatCapacityCapture{}
	h := newOpenAIResponsesFailoverTestHandler(t, upstream, configureChatCapacityAccounts)
	body := `{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`
	c, first := chatCapacityContext(t, body, "")
	h.ChatCompletions(c)
	require.Equal(t, 200, first.Code, first.Body.String())
	c, second := chatCapacityContext(t, body, "")
	h.ChatCompletions(c)
	require.Equal(t, 429, second.Code, second.Body.String())
	require.Contains(t, second.Body.String(), "session_roots_full")
	require.NotEmpty(t, second.Header().Get("Retry-After"))
	require.Len(t, upstream.bodies, 1, "capacity denial must not call upstream")
}

func TestChatCapacityPreservesToolHistoryAndImages(t *testing.T) {
	upstream := &chatCapacityCapture{}
	h := newOpenAIResponsesFailoverTestHandler(t, upstream, configureChatCapacityAccounts)
	body := `{"model":"gpt-5.5","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"https://example.com/image.png"}}]},{"role":"assistant","tool_calls":[{"id":"call_one","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"上海\"}"}}]},{"role":"tool","tool_call_id":"call_one","content":"sunny"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`
	c, rec := chatCapacityContext(t, body, "tools-conversation")
	h.ChatCompletions(c)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Len(t, upstream.bodies, 1)
	sent := gjson.Parse(upstream.bodies[0])
	require.Contains(t, sent.Get("input").Raw, "https://example.com/image.png")
	require.Equal(t, "get_weather", sent.Get(`input.#(type=="function_call").name`).String())
	require.JSONEq(t, `{"city":"上海"}`, sent.Get(`input.#(type=="function_call").arguments`).String())
	callID := sent.Get(`input.#(type=="function_call").call_id`).String()
	require.NotEmpty(t, callID)
	require.Equal(t, callID, sent.Get(`input.#(type=="function_call_output").call_id`).String())
	require.Equal(t, "sunny", sent.Get(`input.#(type=="function_call_output").output`).String())
	require.Equal(t, "get_weather", sent.Get("tools.0.name").String())
}

func TestChatCapacityRejectsHTTPContinuationBeforeConversion(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-5.5","previous_response_id":"resp_old","messages":[{"role":"user","content":"continue"}]}`,
		`{"model":"gpt-5.5","previous_response_id":"resp_old","input":"continue"}`,
	} {
		upstream := &chatCapacityCapture{}
		h := newOpenAIResponsesFailoverTestHandler(t, upstream, configureChatCapacityAccounts)
		c, rec := chatCapacityContext(t, body, "conversation")
		h.ChatCompletions(c)
		require.Equal(t, 400, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), "session_continuation_requires_websocket")
		require.Empty(t, rec.Header().Get("Retry-After"))
		require.Empty(t, upstream.bodies, "previous_response_id must not be silently dropped")
	}
}

func TestChatCapacityPreservesNativeChildMetadata(t *testing.T) {
	upstream := &chatCapacityCapture{}
	h := newOpenAIResponsesFailoverTestHandler(t, upstream, configureChatCapacityAccounts)
	parent, parentRec := chatCapacityContext(t, `{"model":"gpt-5.5","messages":[{"role":"user","content":"parent"}]}`, "parent")
	h.ChatCompletions(parent)
	require.Equal(t, 200, parentRec.Code, parentRec.Body.String())
	body := `{"model":"gpt-5.5","messages":[{"role":"user","content":"child"}],"client_metadata":{"thread_id":"child","x-codex-parent-thread-id":"parent","x-openai-subagent":"collab_spawn","x-codex-turn-metadata":"{\"subagent_kind\":\"thread_spawn\",\"thread_source\":\"subagent\",\"turn_id\":\"child-turn\"}"}}`
	c, rec := chatCapacityContext(t, body, "parent")
	h.ChatCompletions(c)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Len(t, upstream.bodies, 2)
	sent := gjson.Parse(upstream.bodies[1])
	require.Equal(t, upstream.headers[0].Get("session_id"), upstream.headers[1].Get("session_id"))
	require.Equal(t, upstream.headers[0].Get("thread-id"), upstream.headers[1].Get("x-codex-parent-thread-id"))
	require.NotEqual(t, upstream.headers[0].Get("thread-id"), upstream.headers[1].Get("thread-id"))
	require.Equal(t, "collab_spawn", sent.Get("client_metadata.x-openai-subagent").String())
	require.Equal(t, "collab_spawn", upstream.headers[1].Get("x-openai-subagent"))
	turn := gjson.Parse(sent.Get("client_metadata.x-codex-turn-metadata").String())
	require.Equal(t, "thread_spawn", turn.Get("subagent_kind").String())
	require.Equal(t, "subagent", turn.Get("thread_source").String())
	require.NotEmpty(t, turn.Get("turn_id").String())
	require.NotEqual(t, "child-turn", turn.Get("turn_id").String())
	require.Equal(t, turn.Raw, upstream.headers[1].Get("x-codex-turn-metadata"))
}

func TestChatCapacityContextDoesNotEnableUnselectedMode(t *testing.T) {
	for _, apiKey := range []bool{false, true} {
		t.Run(fmt.Sprint(apiKey), func(t *testing.T) {
			upstream := &chatCapacityCapture{}
			h := newOpenAIResponsesFailoverTestHandler(t, upstream, func(accounts []service.Account) {
				accounts[1].Schedulable = false
				if apiKey {
					accounts[0].Type = service.AccountTypeAPIKey
					accounts[0].Credentials = map[string]any{"api_key": "test-key", "base_url": "https://api.openai.com"}
				}
			})
			// More than the default five roots must still pass for accounts without capacity enabled.
			for i := 0; i < 6; i++ {
				c, rec := chatCapacityContext(t, `{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`, "")
				h.ChatCompletions(c)
				require.Equal(t, 200, rec.Code, rec.Body.String())
			}
			require.Len(t, upstream.bodies, 6)
		})
	}
}

func TestChatCapacityStatelessRetryAndFailover(t *testing.T) {
	for _, same := range []bool{true, false} {
		t.Run(fmt.Sprint(same), func(t *testing.T) {
			upstream := &chatCapacityCapture{failures: 1}
			if same {
				upstream.failureStatus = 429
			}
			h := newOpenAIResponsesFailoverTestHandler(t, upstream, func(accounts []service.Account) {
				configureChatCapacityAccounts(accounts)
				if !same {
					accounts[1].Schedulable = true
				}
			})
			body := `{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`
			c, rec := chatCapacityContext(t, body, "")
			h.ChatCompletions(c)
			require.Equal(t, 200, rec.Code, rec.Body.String())
			require.Len(t, upstream.bodies, 2)
			if same {
				require.Equal(t, []int64{1, 1}, upstream.accountIDs)
				require.Equal(t, upstream.headers[0].Get("session_id"), upstream.headers[1].Get("session_id"))
			} else {
				require.Equal(t, []int64{1, 2}, upstream.accountIDs)
				require.NotEqual(t, upstream.headers[0].Get("session_id"), upstream.headers[1].Get("session_id"))
			}
			for i, sent := range upstream.bodies {
				require.Equal(t, upstream.headers[i].Get("session_id"), gjson.Get(sent, "client_metadata.session_id").String())
				require.Equal(t, upstream.headers[i].Get("thread-id"), gjson.Get(sent, "client_metadata.thread_id").String())
			}
			// Failed sends retain exposure; retries must not create extra roots.
			c, denied := chatCapacityContext(t, body, "")
			h.ChatCompletions(c)
			require.Equal(t, 429, denied.Code, denied.Body.String())
			require.Len(t, upstream.bodies, 2)
		})
	}
}

func TestChatCapacitySyntheticOverflow(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			upstream := &chatCapacityCapture{}
			h := newOpenAIResponsesFailoverTestHandler(t, upstream, func(accounts []service.Account) {
				configureChatCapacityAccounts(accounts)
				accounts[0].Extra["codex_session_capacity"].(map[string]any)["subagent_fallback_enabled"] = true
			})
			body := fmt.Sprintf(`{"model":"gpt-5.5","stream":%t,"messages":[{"role":"user","content":"hello"}]}`, stream)
			for _, session := range []string{"host", "overflow"} {
				c, rec := chatCapacityContext(t, body, session)
				h.ChatCompletions(c)
				require.Equal(t, 200, rec.Code, rec.Body.String())
				require.Contains(t, rec.Body.String(), `"choices"`)
			}
			require.Len(t, upstream.bodies, 2)
			host, child := upstream.headers[0], upstream.headers[1]
			sent := gjson.Parse(upstream.bodies[1])
			require.Equal(t, host.Get("session_id"), child.Get("session_id"))
			require.Equal(t, host.Get("thread-id"), child.Get("x-codex-parent-thread-id"))
			require.NotEqual(t, host.Get("thread-id"), child.Get("thread-id"))
			require.Equal(t, child.Get("session_id"), sent.Get("client_metadata.session_id").String())
			require.Equal(t, child.Get("thread-id"), sent.Get("client_metadata.thread_id").String())
			require.Equal(t, child.Get("x-codex-parent-thread-id"), sent.Get("client_metadata.x-codex-parent-thread-id").String())
			require.Equal(t, "collab_spawn", child.Get("x-openai-subagent"))
		})
	}
}

func TestChatCapacityTurnStateDelayedEchoAfterFailover(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			upstream := &chatCapacityCapture{responseHeader: http.Header{"X-Codex-Turn-State": {"emitted-by-first"}}}
			h := newOpenAIResponsesFailoverTestHandler(t, upstream, func(accounts []service.Account) {
				configureChatCapacityAccounts(accounts)
				accounts[1].Schedulable = true
			})
			body := fmt.Sprintf(`{"model":"gpt-5.5","stream":%t,"messages":[{"role":"user","content":"hello"}]}`, stream)
			c, rec := chatCapacityContext(t, body, "conversation")
			h.ChatCompletions(c)
			require.Equal(t, 200, rec.Code, rec.Body.String())
			require.Equal(t, "emitted-by-first", rec.Header().Get("x-codex-turn-state"))
			upstream.failures = 1
			upstream.responseHeader = nil
			c, rec = chatCapacityContext(t, body, "conversation")
			h.ChatCompletions(c)
			require.Equal(t, 200, rec.Code, rec.Body.String())
			require.Equal(t, []int64{1, 1, 2}, upstream.accountIDs)
			require.Empty(t, rec.Header().Get("x-codex-turn-state"))
			// Echo A's token only after affinity has already moved to B.
			c, rec = chatCapacityContext(t, body[:len(body)-1]+`,"client_metadata":{"x-codex-turn-state":"emitted-by-first"}}`, "conversation")
			c.Request.Header.Set("x-codex-turn-state", "emitted-by-first")
			h.ChatCompletions(c)
			require.Equal(t, 200, rec.Code, rec.Body.String())
			require.Equal(t, []int64{1, 1, 2, 2}, upstream.accountIDs)
			require.Empty(t, upstream.headers[3].Get("x-codex-turn-state"))
			require.False(t, gjson.Get(upstream.bodies[3], "client_metadata.x-codex-turn-state").Exists())
		})
	}
}
