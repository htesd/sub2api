package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func openAIBpsExcelTestAccount(extra map[string]any) *Account {
	return &Account{
		ID:       1,
		Name:     "bps-test",
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra:    extra,
	}
}

func TestOpenAIBpsExcelEnabled(t *testing.T) {
	require.False(t, openAIBpsExcelEnabled(nil))
	require.False(t, openAIBpsExcelEnabled(openAIBpsExcelTestAccount(nil)))
	require.False(t, openAIBpsExcelEnabled(openAIBpsExcelTestAccount(map[string]any{})))
	require.False(t, openAIBpsExcelEnabled(openAIBpsExcelTestAccount(map[string]any{OpenAIBpsExcelEnabledExtraKey: false})))
	require.True(t, openAIBpsExcelEnabled(openAIBpsExcelTestAccount(map[string]any{OpenAIBpsExcelEnabledExtraKey: true})))
	require.True(t, openAIBpsExcelEnabled(openAIBpsExcelTestAccount(map[string]any{OpenAIBpsExcelEnabledExtraKey: "true"})))
	require.True(t, openAIBpsExcelEnabled(openAIBpsExcelTestAccount(map[string]any{OpenAIBpsExcelEnabledExtraKey: "on"})))
	require.False(t, openAIBpsExcelEnabled(openAIBpsExcelTestAccount(map[string]any{OpenAIBpsExcelEnabledExtraKey: "false"})))
	require.False(t, openAIBpsExcelEnabled(openAIBpsExcelTestAccount(map[string]any{OpenAIBpsExcelEnabledExtraKey: 0})))
}

func TestOpenAIBpsExcelCanonicalModel(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"gpt-6-astra", "gpt-6-astra"},
		{"gpt-6", "gpt-6-astra"},
		{"gpt-6-astra-high", "gpt-6-astra"},
		{"gpt-6-astra-xhigh", "gpt-6-astra"},
		{"gpt-6-astra-2026-09-25", "gpt-6-astra"},
		{"openai/gpt-6-astra", "gpt-6-astra"},
		{"GPT-6-ASTRA", "gpt-6-astra"},
		{"gpt-5.6-sol", "gpt-5.6-sol"},
		{"gpt-5.6-sol-high", "gpt-5.6-sol"},
		{"gpt-5.6-terra", "gpt-5.6-terra"},
		{"gpt-5.6-luna", "gpt-5.6-luna"},
		// gpt-6-sol/luna 于 2026-09-26 在 bps 开放（前一天还是 403）。
		{"gpt-6-sol", "gpt-6-sol"},
		{"gpt-6-sol-high", "gpt-6-sol"},
		{"gpt-6-luna", "gpt-6-luna"},
		{"gpt-6-luna-xhigh", "gpt-6-luna"},
		// 不在 bps 目录。
		{"gpt-6-nova", ""},
		{"gpt-5.5", ""},
		{"gpt-5.4", ""},
		{"", ""},
	} {
		require.Equal(t, tc.want, openAIBpsExcelCanonicalModel(tc.in), "input %q", tc.in)
	}
}

func TestOpenAIBpsExcelReasoningEffort(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"low", "low"},
		{"medium", "medium"},
		{"high", "high"},
		{"xhigh", "xhigh"},
		{"max", "xhigh"},
		{"ultra", "xhigh"},
		{"none", "medium"},
		{"minimal", "medium"},
		{"", "medium"},
		{"garbage", "medium"},
		{" HIGH ", "high"},
	} {
		require.Equal(t, tc.want, openAIBpsExcelReasoningEffort(tc.in), "input %q", tc.in)
	}
}

func openAIBpsExcelBody(model string, extraFields ...string) []byte {
	var b strings.Builder
	b.WriteString(`{"model":` + openAIBpsExcelQuote(model) + `,"stream":true,"store":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]`)
	for _, f := range extraFields {
		b.WriteString(",")
		b.WriteString(f)
	}
	b.WriteString("}")
	return []byte(b.String())
}

func openAIBpsExcelQuote(s string) string {
	out, _ := json.Marshal(s)
	return string(out)
}

func openAIBpsExcelResponsesContext() *gin.Context {
	c, _ := gin.CreateTestContext(nil)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c
}

func openAIBpsExcelMessagesContext() *gin.Context {
	c, _ := gin.CreateTestContext(nil)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c
}

func TestOpenAIBpsExcelRequestEligible(t *testing.T) {
	enabled := openAIBpsExcelTestAccount(map[string]any{OpenAIBpsExcelEnabledExtraKey: true})
	disabled := openAIBpsExcelTestAccount(nil)
	c, _ := gin.CreateTestContext(nil)

	require.True(t, openAIBpsExcelRequestEligible(context.Background(), c, enabled, openAIBpsExcelBody("gpt-6-astra")))
	require.True(t, openAIBpsExcelRequestEligible(context.Background(), c, enabled, openAIBpsExcelBody("gpt-5.6-sol")))
	// 开关关闭 / 账号类型不符 / nil。
	require.False(t, openAIBpsExcelRequestEligible(context.Background(), c, disabled, openAIBpsExcelBody("gpt-6-astra")))
	require.False(t, openAIBpsExcelRequestEligible(context.Background(), c, nil, openAIBpsExcelBody("gpt-6-astra")))
	apiKeyAccount := openAIBpsExcelTestAccount(map[string]any{OpenAIBpsExcelEnabledExtraKey: true})
	apiKeyAccount.Type = AccountTypeAPIKey
	require.False(t, openAIBpsExcelRequestEligible(context.Background(), c, apiKeyAccount, openAIBpsExcelBody("gpt-6-astra")))
	// 模型目录外。
	require.False(t, openAIBpsExcelRequestEligible(context.Background(), c, enabled, openAIBpsExcelBody("gpt-6-nova")))
	require.False(t, openAIBpsExcelRequestEligible(context.Background(), c, enabled, openAIBpsExcelBody("gpt-5.5")))
	// gpt-6-sol/luna 已在 bps 目录（2026-09-26 复测开放）。
	require.True(t, openAIBpsExcelRequestEligible(context.Background(), c, enabled, openAIBpsExcelBody("gpt-6-sol")))
	require.True(t, openAIBpsExcelRequestEligible(context.Background(), c, enabled, openAIBpsExcelBody("gpt-6-luna")))
	// 容量记账账号不再整体排除：bps 流量不产生 codex 会话，准入簿记保守成立。
	capacityAccount := openAIBpsExcelTestAccount(map[string]any{
		OpenAIBpsExcelEnabledExtraKey: true,
		"codex_fingerprint_mode":      "capacity",
	})
	require.True(t, openAIBpsExcelRequestEligible(context.Background(), c, capacityAccount, openAIBpsExcelBody("gpt-6-astra")))
	// 客户端 function/custom 工具：原生 responses 入站放行（additional_tools 原生声明）；
	// 无入站请求信息或 messages 入站（没挂 update_plan 归一/信封兜底）仍留 codex 上游。
	responsesCtx := openAIBpsExcelResponsesContext()
	require.True(t, openAIBpsExcelRequestEligible(context.Background(), responsesCtx, enabled, openAIBpsExcelBody("gpt-6-astra",
		`"tools":[{"type":"function","name":"exec_command","description":"d","parameters":{}}]`)))
	require.True(t, openAIBpsExcelRequestEligible(context.Background(), responsesCtx, enabled, openAIBpsExcelBody("gpt-6-astra",
		`"tools":[{"type":"custom","name":"apply_patch"}]`)))
	require.False(t, openAIBpsExcelRequestEligible(context.Background(), c, enabled, openAIBpsExcelBody("gpt-6-astra",
		`"tools":[{"type":"function","name":"exec_command","description":"d","parameters":{}}]`)))
	require.False(t, openAIBpsExcelRequestEligible(context.Background(), openAIBpsExcelMessagesContext(), enabled, openAIBpsExcelBody("gpt-6-astra",
		`"tools":[{"type":"function","name":"exec_command","description":"d","parameters":{}}]`)))
	// tool_choice:"none" 视为无工具：无入站信息也可改道。
	require.True(t, openAIBpsExcelRequestEligible(context.Background(), c, enabled, openAIBpsExcelBody("gpt-6-astra",
		`"tools":[{"type":"function","name":"exec_command","parameters":{}}],"tool_choice":"none"`)))
	// 仅图像生成工具（分组桥接注入的形状）→ 仍可改道。
	require.True(t, openAIBpsExcelRequestEligible(context.Background(), c, enabled, openAIBpsExcelBody("gpt-6-astra",
		`"tools":[{"type":"image_generation","action":"","model":""}]`)))
	// 图像生成 + function 混合：responses 入站放行（function 原生声明，图像工具丢弃）。
	require.True(t, openAIBpsExcelRequestEligible(context.Background(), responsesCtx, enabled, openAIBpsExcelBody("gpt-6-astra",
		`"tools":[{"type":"image_generation"},{"type":"function","name":"x"}]`)))
	// user 消息图片 → 可改道（改写成合成 user_attachment 调用对，见 images 测试）。
	require.True(t, openAIBpsExcelRequestEligible(context.Background(), c, enabled, []byte(`{"model":"gpt-6-astra","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`)))
	// 改写处理不了的图片位置（assistant 消息）→ codex 上游。
	require.False(t, openAIBpsExcelRequestEligible(context.Background(), c, enabled, []byte(`{"model":"gpt-6-astra","input":[{"type":"message","role":"assistant","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`)))
	// 结构化输出 → codex 上游。
	require.False(t, openAIBpsExcelRequestEligible(context.Background(), c, enabled, openAIBpsExcelBody("gpt-6-astra",
		`"text":{"format":{"type":"json_schema","name":"s","schema":{}}}`)))
	// service_tier（Fast/Flex）→ codex 上游。
	require.False(t, openAIBpsExcelRequestEligible(context.Background(), c, enabled, openAIBpsExcelBody("gpt-6-astra",
		`"service_tier":"priority"`)))
}

func TestRewriteOpenAIBodyForBpsExcel(t *testing.T) {
	body := []byte(`{
		"model": "gpt-6-astra",
		"stream": true,
		"store": true,
		"instructions": "be brief",
		"reasoning": {"effort": "max", "summary": "auto"},
		"service_tier": "priority",
		"previous_response_id": "resp_123",
		"tools": [{"type":"image_generation"}],
		"tool_choice": "auto",
		"parallel_tool_calls": true,
		"max_output_tokens": 4096,
		"prompt_cache_key": "conv-abc",
		"metadata": {"origin": "unit-test", "n": 3},
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"run it"}]},
			{"type":"function_call_output","call_id":"c1","output":"ok"}
		]
	}`)
	rewritten, specs, err := rewriteOpenAIBodyForBpsExcel(body, "")
	require.NoError(t, err)
	// image_generation 不是客户端 function/custom 工具，不进中继目录。
	require.Empty(t, specs)

	var out map[string]any
	require.NoError(t, json.Unmarshal(rewritten, &out))

	require.Equal(t, "gpt-6-astra", out["model"])
	require.Equal(t, "explicit", out["model_selection"])
	require.Equal(t, true, out["stream"])
	require.Equal(t, false, out["store"])
	require.Equal(t, "be brief", out["instructions"])
	// max 档收敛到 xhigh；reasoning 对象不进入白名单。
	require.Equal(t, "xhigh", out["reasoning_effort"])
	require.NotContains(t, out, "reasoning")
	require.Equal(t, "conv-abc", out["prompt_cache_key"])

	// 白名单外字段一律丢弃。
	for _, dropped := range []string{"service_tier", "previous_response_id", "tools", "tool_choice", "parallel_tool_calls", "max_output_tokens"} {
		require.NotContains(t, out, dropped, "field %s must be dropped", dropped)
	}

	// input = 序言（无客户端工具时的外部客户端提示）+ 翻译后的原历史。
	input, ok := out["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 5)
	prologue, ok := input[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "developer", prologue["role"])
	prologueText := prologue["content"].([]any)[0].(map[string]any)["text"].(string)
	require.Contains(t, prologueText, "There is no live Excel workbook in this session")
	// 原历史里的 function_call_output 仍在，id 归一到 call_id 派生值。
	tail, ok := input[4].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "function_call_output", tail["type"])
	require.Equal(t, "c1", tail["call_id"])
	require.Equal(t, openAIBpsFunctionItemID("c1"), tail["id"])

	// context_management 默认压缩阈值。
	cm, ok := out["context_management"].([]any)
	require.True(t, ok)
	require.Len(t, cm, 1)
	require.Equal(t, "compaction", cm[0].(map[string]any)["type"])

	// metadata：客户端标量保留，task/turn 派生，agent_iteration 计入尾随工具输出。
	metadata, ok := out["metadata"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "unit-test", metadata["origin"])
	require.Equal(t, "3", metadata["n"])
	require.Equal(t, "2", metadata["agent_iteration"])
	taskID, _ := metadata["task_id"].(string)
	turnID, _ := metadata["turn_id"].(string)
	require.Len(t, taskID, 36)
	require.Len(t, turnID, 36)

	// 确定性：同一请求体重写两次，派生标识逐字一致。
	again, _, err := rewriteOpenAIBodyForBpsExcel(body, "")
	require.NoError(t, err)
	var out2 map[string]any
	require.NoError(t, json.Unmarshal(again, &out2))
	require.Equal(t, metadata["task_id"], out2["metadata"].(map[string]any)["task_id"])
	require.Equal(t, metadata["turn_id"], out2["metadata"].(map[string]any)["turn_id"])

	// 追加新一轮用户消息后 turn_id 变化、task_id 不变。
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(body, &decoded))
	decoded["input"] = append(decoded["input"].([]any), map[string]any{
		"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": "next turn"}},
	})
	nextBody, _ := json.Marshal(decoded)
	nextRewritten, _, err := rewriteOpenAIBodyForBpsExcel(nextBody, "")
	require.NoError(t, err)
	var out3 map[string]any
	require.NoError(t, json.Unmarshal(nextRewritten, &out3))
	m3 := out3["metadata"].(map[string]any)
	require.Equal(t, metadata["task_id"], m3["task_id"])
	require.NotEqual(t, metadata["turn_id"], m3["turn_id"])
	require.Equal(t, "1", m3["agent_iteration"])
}

func TestRewriteOpenAIBodyForBpsExcelEffortFallback(t *testing.T) {
	// 顶层 reasoning_effort 兼容写法同样被认（antistorm.12 的 none 档口径）。
	body := []byte(`{"model":"gpt-5.6-sol","reasoning_effort":"high","input":"ping"}`)
	rewritten, _, err := rewriteOpenAIBodyForBpsExcel(body, "")
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rewritten, &out))
	require.Equal(t, "gpt-5.6-sol", out["model"])
	require.Equal(t, "high", out["reasoning_effort"])
	// 字符串输入包装成 user 消息条目，前面是外部客户端提示序言。
	input, ok := out["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 2)
	ping, ok := input[1].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "user", ping["role"])

	// 无 effort 信息默认 medium。
	body = []byte(`{"model":"gpt-5.6-terra","input":"ping"}`)
	rewritten, _, err = rewriteOpenAIBodyForBpsExcel(body, "")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(rewritten, &out))
	require.Equal(t, "medium", out["reasoning_effort"])
}

func TestApplyOpenAIBpsExcelHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("chatgpt-account-id", "acct-1")
	applyOpenAIBpsExcelHeaders(h, nil, true)
	require.Equal(t, "acct-1", h.Get("x-openai-account-id"))
	require.Equal(t, "chatgpt", h.Get("x-basispoints-auth-mode"))
	require.Equal(t, "basispoints-excel-plugin", h.Get("x-openai-internal-basispoints-client-product"))
	require.Equal(t, "excel", h.Get("x-openai-internal-basispoints-client-editor"))
	require.Equal(t, "https://bps.openai.com", h.Get("origin"))
	require.Equal(t, "text/event-stream", h.Get("accept"))
	require.Equal(t, "application/json", h.Get("content-type"))
	// 2026-10 client 校验对齐：WebView2 UA、browser-ua 族、固定 accept-encoding。
	require.Contains(t, h.Get("user-agent"), "Edg/140.0.0.0")
	require.Equal(t, "Edge", h.Get("x-openai-internal-basispoints-browser-name"))
	require.Equal(t, `"Windows"`, h.Get("x-openai-internal-basispoints-browser-ua-platform"))
	require.Equal(t, "?0", h.Get("x-openai-internal-basispoints-browser-ua-mobile"))
	require.Equal(t, "identity", h.Get("accept-encoding"))
	// 无账号凭据时不发 account-user-id，也不发捕获头专有的 runtime-version。
	require.Empty(t, h.Get("x-openai-account-user-id"))
	require.Empty(t, h.Get("x-stainless-runtime-version"))

	h2 := http.Header{}
	applyOpenAIBpsExcelHeaders(h2, nil, false)
	require.Equal(t, "application/json", h2.Get("accept"))
	require.Empty(t, h2.Get("x-openai-account-id"))
}

// bpsTestJWT 拼一个只用于测试的伪 JWT（签名无效，仅 payload 可读）。
func bpsTestJWT(t *testing.T, payload map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString(raw) + ".sig"
}

// TestApplyOpenAIBpsExcelHeadersAccountUserID 验证 x-openai-account-user-id 的
// 取值链：JWT claims（含 https://api.openai.com/auth 命名空间）现取优先，
// 捕获头仅在 JWT 缺失时兜底；凭据里的 chatgpt_user_id（无 __ 后缀形态）不参与。
func TestApplyOpenAIBpsExcelHeadersAccountUserID(t *testing.T) {
	full := "user-abc123__95354fb3-6cc3-4244-834c-d1b22ad3ecf1"
	account := openAIBpsExcelTestAccount(nil)
	account.Credentials = map[string]any{
		"access_token": bpsTestJWT(t, map[string]any{
			"https://api.openai.com/auth": map[string]any{"chatgpt_account_user_id": full},
		}),
		"chatgpt_user_id": "user-abc123",
	}
	h := http.Header{}
	applyOpenAIBpsExcelHeaders(h, account, true)
	require.Equal(t, full, h.Get("x-openai-account-user-id"))

	// 顶层 claims 形态也认。
	account.Credentials["access_token"] = bpsTestJWT(t, map[string]any{"chatgpt_account_user_id": "user-top"})
	h = http.Header{}
	applyOpenAIBpsExcelHeaders(h, account, true)
	require.Equal(t, "user-top", h.Get("x-openai-account-user-id"))

	// JWT 无 claim 时捕获头兜底；JWT 有值时压过捕获值。
	account.Credentials["access_token"] = "not-a-jwt"
	account.Extra = map[string]any{OpenAIBpsExcelCapturedHeadersExtraKey: map[string]any{
		"X-OpenAI-Account-User-ID": "user-captured",
	}}
	h = http.Header{}
	applyOpenAIBpsExcelHeaders(h, account, true)
	require.Equal(t, "user-captured", h.Get("x-openai-account-user-id"))

	account.Credentials["access_token"] = bpsTestJWT(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_user_id": full},
	})
	h = http.Header{}
	applyOpenAIBpsExcelHeaders(h, account, true)
	require.Equal(t, full, h.Get("x-openai-account-user-id"))
}

// TestApplyOpenAIBpsExcelHeadersCaptured 验证账号级捕获头：白名单内覆盖同名默认头、
// 白名单外（身份/协议/Cookie）一律丢弃、控制字符与超长值丢弃、数组保序。
func TestApplyOpenAIBpsExcelHeadersCaptured(t *testing.T) {
	longValue := strings.Repeat("a", openAIBpsMaxCapturedHeaderValue+1)
	account := openAIBpsExcelTestAccount(map[string]any{
		OpenAIBpsExcelCapturedHeadersExtraKey: map[string]any{
			"User-Agent": "Mozilla/5.0 real-capture",
			"X-OpenAI-Internal-Basispoints-Browser-Name": "Chrome",
			"X-Stainless-Runtime-Version":                "v22.17.0",
			"X-Stainless-OS":                             []any{"macOS", "macOS-dup", ""},
			"Authorization":                              "Bearer evil",
			"X-OpenAI-Account-ID":                        "acct-evil",
			"X-Basispoints-Auth-Mode":                    "apikey",
			"Cookie":                                     "session=evil",
			"Origin":                                     "https://evil.example",
			"X-Not-In-List":                              "nope",
			"X-Stainless-Arch":                           "bad\nvalue",
			"X-Stainless-Lang":                           longValue,
		},
	})
	h := http.Header{}
	h.Set("chatgpt-account-id", "acct-real")
	applyOpenAIBpsExcelHeaders(h, account, true)

	require.Equal(t, "Mozilla/5.0 real-capture", h.Get("user-agent"))
	require.Equal(t, "Chrome", h.Get("x-openai-internal-basispoints-browser-name"))
	require.Equal(t, "v22.17.0", h.Get("x-stainless-runtime-version"))
	require.Equal(t, []string{"macOS", "macOS-dup"}, h.Values("X-Stainless-OS"))
	// 身份头与协议头不受捕获影响。
	require.Equal(t, "acct-real", h.Get("x-openai-account-id"))
	require.Equal(t, "chatgpt", h.Get("x-basispoints-auth-mode"))
	require.Empty(t, h.Get("cookie"))
	require.Equal(t, "https://bps.openai.com", h.Get("origin"))
	require.Empty(t, h.Get("x-not-in-list"))
	// 控制字符/超长值被丢弃后回落到静态默认值。
	require.Equal(t, "unknown", h.Get("x-stainless-arch"))
	require.Equal(t, "js", h.Get("x-stainless-lang"))
}

// TestBuildUpstreamRequestBpsExcel 走 buildUpstreamRequest 全函数验证 bps 改道后的
// 真实出站请求：URL/Host/头部最小集/请求体白名单，且不带任何 codex 客户端身份头。
func TestBuildUpstreamRequestBpsExcel(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:       7,
		Name:     "bps-e2e",
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"chatgpt_account_id": "acct-e2e",
		},
		Extra: map[string]any{OpenAIBpsExcelEnabledExtraKey: true},
	}
	body := []byte(`{"model":"gpt-6-astra","stream":true,"store":false,"reasoning":{"effort":"high"},"previous_response_id":"resp_old","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	req, err := svc.buildUpstreamRequest(context.Background(), c, account, body, "test-token", true, "", true)
	require.NoError(t, err)
	require.Equal(t, "https://bps.openai.com/basispoints/api/responses", req.URL.String())
	require.Equal(t, "bps.openai.com", req.Host)

	require.Equal(t, "Bearer test-token", req.Header.Get("authorization"))
	require.Equal(t, "acct-e2e", req.Header.Get("chatgpt-account-id"))
	require.Equal(t, "acct-e2e", req.Header.Get("x-openai-account-id"))
	require.Equal(t, "chatgpt", req.Header.Get("x-basispoints-auth-mode"))
	require.Equal(t, "https://bps.openai.com", req.Header.Get("origin"))
	require.Equal(t, "text/event-stream", req.Header.Get("accept"))
	require.Equal(t, "identity", req.Header.Get("accept-encoding"))
	require.Contains(t, req.Header.Get("user-agent"), "Edg/140.0.0.0")
	// 该账号无 access_token 凭据：不发 account-user-id。
	require.Empty(t, req.Header.Get("x-openai-account-user-id"))
	// codex 客户端身份链一律不出现。
	for _, absent := range []string{"originator", "x-codex-turn-state", "version", "session_id", "OpenAI-Beta"} {
		require.Empty(t, req.Header.Get(absent), "header %s must not be sent to bps", absent)
	}
	require.True(t, openAIBpsExcelUsed(c))

	var out map[string]any
	require.NoError(t, json.NewDecoder(req.Body).Decode(&out))
	require.Equal(t, "gpt-6-astra", out["model"])
	require.Equal(t, "explicit", out["model_selection"])
	require.Equal(t, "high", out["reasoning_effort"])
	require.Equal(t, false, out["store"])
	require.NotContains(t, out, "previous_response_id")
	require.NotContains(t, out, "reasoning")
}

// TestBuildUpstreamRequestBpsExcelIneligible 资格不满足时请求体与端点保持 codex 原样。
func TestBuildUpstreamRequestBpsExcelIneligible(t *testing.T) {
	account := &Account{
		ID:       8,
		Name:     "bps-ineligible",
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{OpenAIBpsExcelEnabledExtraKey: true},
	}
	// gpt-6-nova 不在 bps 目录内 → 资格不通过（codex 路径的后续装饰需要完整服务
	// 依赖，这里只验证资格判定本身，不调用 buildUpstreamRequest 走到底）。
	require.False(t, openAIBpsExcelRequestEligible(context.Background(), nil, account, []byte(`{"model":"gpt-6-nova","input":"ping"}`)))
	require.False(t, openAIBpsExcelRequestEligible(context.Background(), nil, account, []byte(`{"model":"gpt-6-astra","tools":[{"type":"function","name":"x"}],"input":"ping"}`)))
}
