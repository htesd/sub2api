package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// antistorm.21 审查修复的回归测试。

// 图片接口自建请求（强制 image_generation）不得改道 bps：调用方会把 URL 改回
// codex 端点，改道只会产出 URL/Host/请求体互相矛盾的请求。
func TestOpenAIBpsExcelIneligibleForImagesSelfBuiltRequest(t *testing.T) {
	account := openAIBpsExcelTestAccount(map[string]any{OpenAIBpsExcelEnabledExtraKey: true})
	c, _ := gin.CreateTestContext(nil)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	body := []byte(`{"model":"gpt-5.6-luna","stream":true,"tools":[{"type":"image_generation"}],"tool_choice":{"type":"image_generation"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"cat"}]}]}`)

	require.False(t, openAIBpsExcelRequestEligible(withOpenAIImagesSelfBuiltRequest(context.Background()), c, account, body))
	// 即使没有自建标记，对象形态的 tool_choice 也不改道。
	require.False(t, openAIBpsExcelRequestEligible(context.Background(), c, account, body))
}

func TestOpenAIBpsRequestToolsRelayable(t *testing.T) {
	account := openAIBpsExcelTestAccount(map[string]any{OpenAIBpsExcelEnabledExtraKey: true})
	responsesCtx := openAIBpsExcelResponsesContext()
	eligible := func(extra string) bool {
		return openAIBpsExcelRequestEligible(context.Background(), responsesCtx, account, openAIBpsExcelBody("gpt-6-astra", extra))
	}
	fn := `{"type":"function","name":"exec_command","parameters":{}}`

	// Codex CLI 常带的托管工具可丢弃。
	require.True(t, eligible(`"tools":[`+fn+`,{"type":"web_search"},{"type":"tool_search"},{"type":"image_generation"}],"tool_choice":"auto"`))
	// 目录外托管工具不能静默丢弃。
	for _, hosted := range []string{"mcp", "local_shell", "file_search", "code_interpreter", "computer_use_preview"} {
		require.False(t, eligible(`"tools":[`+fn+`,{"type":"`+hosted+`"}]`), hosted)
	}
	// 强制调用档无法经信封中继兑现。
	require.False(t, eligible(`"tools":[`+fn+`],"tool_choice":"required"`))
	require.False(t, eligible(`"tools":[`+fn+`],"tool_choice":{"type":"function","name":"exec_command"}`))
	// tool_choice:"none" 时工具声明无效，托管工具也不拦。
	require.True(t, eligible(`"tools":[{"type":"mcp"}],"tool_choice":"none"`))
}

// 客户端历史里的传输名调用（上一轮提取失败、原样下发的原生 run_officejs）
// 必须原样回放，不能再包一层信封。
func TestOpenAIBpsTranslateReplaysTransportCallVerbatim(t *testing.T) {
	specs := openAIBpsTestSpecs()
	rawArgs := `{"summary":"s","code":"{not json"}`
	history := []any{
		map[string]any{"type": "function_call", "call_id": "call_raw_transport", "name": "run_officejs", "arguments": rawArgs},
		map[string]any{"type": "function_call_output", "call_id": "call_raw_transport", "output": "unsupported call: run_officejs"},
	}
	translated, origins := openAIBpsTranslateInputItems(history, specs, "apikey:1")
	require.Len(t, translated, 2)
	call := translated[0].(map[string]any)
	require.Equal(t, "run_officejs", call["name"])
	require.Equal(t, rawArgs, call["arguments"], "原始参数必须逐字节保留")
	require.NotContains(t, call["arguments"], `\"name\":\"run_officejs\"`)
	require.Equal(t, openAIBpsToolTransportName, origins["call_raw_transport"])
	// 报错仍翻译成重试指引。
	require.Equal(t, openAIBpsTransportRetryGuidance, translated[1].(map[string]any)["output"])

	// functions. 前缀的传输名同理。
	prefixed := []any{map[string]any{"type": "function_call", "call_id": "call_pref_t", "name": "functions.run_officejs", "arguments": rawArgs}}
	translated, _ = openAIBpsTranslateInputItems(prefixed, specs, "apikey:1")
	require.Equal(t, "functions.run_officejs", translated[0].(map[string]any)["name"])
	require.Equal(t, rawArgs, translated[0].(map[string]any)["arguments"])
}

// 目录外的原生工具调用（被直通给客户端后回传）原样回放；目录内的仍按信封重合成。
func TestOpenAIBpsTranslateReplaysNonCatalogNativeVerbatim(t *testing.T) {
	specs := openAIBpsTestSpecs()
	history := []any{
		map[string]any{"type": "function_call", "call_id": "call_ls_native", "name": "list_skills", "arguments": `{}`},
		map[string]any{"type": "function_call", "call_id": "call_ns_miss", "name": "search", "namespace": "mcp", "arguments": `{"q":"x"}`},
		map[string]any{"type": "function_call", "call_id": "call_exec_miss", "name": "exec_command", "arguments": `{"cmd":"ls"}`},
	}
	translated, origins := openAIBpsTranslateInputItems(history, specs, "apikey:1")
	require.Len(t, translated, 3)
	require.Equal(t, "list_skills", translated[0].(map[string]any)["name"])
	require.Equal(t, "list_skills", origins["call_ls_native"])
	// 命名空间工具按 namespace.name 查目录，属于客户端工具 → 重合成。
	require.Equal(t, "run_officejs", translated[1].(map[string]any)["name"])
	require.Equal(t, "run_officejs", translated[2].(map[string]any)["name"])

	// 目录为空时无从区分，保持重合成旧行为。
	translated, _ = openAIBpsTranslateInputItems(history[2:], nil, "apikey:1")
	require.Equal(t, "run_officejs", translated[0].(map[string]any)["name"])
}

func TestOpenAIBpsNativeCallCacheScopedLRU(t *testing.T) {
	cache := newOpenAIBpsNativeCallCache(2)
	item := func(callID string) map[string]any {
		return map[string]any{"type": "function_call", "call_id": callID, "name": "run_officejs", "arguments": "{}"}
	}

	// 隔离：同 call_id 不同 scope 互不可见。
	cache.Remember("apikey:1", item("call_a"))
	_, hit := cache.Recall("apikey:2", "call_a")
	require.False(t, hit)
	_, hit = cache.Recall("apikey:1", "call_a")
	require.True(t, hit)

	// LRU：Recall 刷新位置，淘汰最久未用的条目。
	cache.Remember("apikey:1", item("call_b"))
	_, hit = cache.Recall("apikey:1", "call_a") // a 变最新
	require.True(t, hit)
	cache.Remember("apikey:1", item("call_c")) // 挤出 b
	_, hit = cache.Recall("apikey:1", "call_b")
	require.False(t, hit)
	_, hit = cache.Recall("apikey:1", "call_a")
	require.True(t, hit)
	_, hit = cache.Recall("apikey:1", "call_c")
	require.True(t, hit)

	// 重复 Remember 覆盖内容且不增加条目。
	updated := item("call_c")
	updated["arguments"] = `{"v":2}`
	cache.Remember("apikey:1", updated)
	got, hit := cache.Recall("apikey:1", "call_c")
	require.True(t, hit)
	require.Equal(t, `{"v":2}`, got["arguments"])
	require.Equal(t, 2, cache.lru.Len())
}

// 无参工具信封只写 name（或 arguments 为空串/null）→ 按空对象提取。
func TestOpenAIBpsExtractEnvelopeWithoutArguments(t *testing.T) {
	specs := openAIBpsClientToolSpecs(map[string]any{"tools": []any{
		map[string]any{"type": "function", "name": "list_files", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
		map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{
			"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}, "required": []any{"cmd"},
		}},
	}})
	for _, envelope := range []string{`{"name":"list_files"}`, `{"name":"list_files","arguments":null}`, `{"name":"list_files","arguments":""}`} {
		extracted, ok := openAIBpsExtractNativeToolCall(openAIBpsNativeTransportCall(envelope), specs)
		require.True(t, ok, envelope)
		require.JSONEq(t, `{}`, extracted["arguments"].(string))
	}
	// 有 required 字段的工具缺参数仍被 schema 拒绝。
	_, ok := openAIBpsExtractNativeToolCall(openAIBpsNativeTransportCall(`{"name":"exec_command"}`), specs)
	require.False(t, ok)
}

// 别名在包装层判定之前归一。
func TestOpenAIBpsEnvelopeAliasesBeforeUnwrap(t *testing.T) {
	specs := openAIBpsTestSpecs()
	// {"tool":"run_officejs","args":{"code":...}} 是传输名包装层，要继续拆。
	wrapped := `{"tool":"run_officejs","args":{"code":"{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"pwd\"}}"}}`
	extracted, ok := openAIBpsExtractNativeToolCall(openAIBpsNativeTransportCall(wrapped), specs)
	require.True(t, ok)
	require.Equal(t, "exec_command", extracted["name"])
	require.JSONEq(t, `{"cmd":"pwd"}`, extracted["arguments"].(string))

	// {"tool":"exec_command","code":"..."} 带名字，不是无名参数壳，不能被拆开。
	named := `{"tool":"exec_command","code":"{\"name\":\"apply_patch\",\"input\":\"x\"}","args":{"cmd":"ls"}}`
	extracted, ok = openAIBpsExtractNativeToolCall(openAIBpsNativeTransportCall(named), specs)
	require.True(t, ok)
	require.Equal(t, "exec_command", extracted["name"])
	require.JSONEq(t, `{"cmd":"ls"}`, extracted["arguments"].(string))
}

func openAIBpsRelayTransportItem(id, callID, envelope string) map[string]any {
	args, _ := json.Marshal(map[string]any{"summary": "s", "code": envelope})
	return map[string]any{"type": "function_call", "id": id, "call_id": callID, "name": "run_officejs", "arguments": string(args), "status": "completed"}
}

func findOpenAIBpsEmittedData(emitted []string, eventType string) []string {
	var out []string
	for i := 0; i+1 < len(emitted); i++ {
		if emitted[i] == "event: "+eventType {
			out = append(out, emitted[i+1])
		}
	}
	return out
}

// 流式缓冲判定与提取同一解析规则：functions. 前缀的同名原生调用也被拦截。
func TestOpenAIBpsStreamRelayResolvesPrefixedNames(t *testing.T) {
	relay := newOpenAIBpsToolStreamRelay(openAIBpsTestSpecs(), "apikey:9")
	native := map[string]any{"type": "function_call", "id": "fc_pref", "call_id": "call_pref", "name": "functions.exec_command", "arguments": `{"cmd":"pwd"}`}
	skeleton := map[string]any{"type": "function_call", "id": "fc_pref", "call_id": "call_pref", "name": "functions.exec_command", "arguments": ""}
	var lines []string
	lines = append(lines, openAIBpsSSELines("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": skeleton})...)
	lines = append(lines, openAIBpsSSELines("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": native})...)
	_, emitted, passed := driveOpenAIBpsRelay(relay, lines)
	for _, line := range passed {
		require.NotContains(t, line, "functions.exec_command")
	}
	done := findOpenAIBpsEmittedData(emitted, "response.output_item.done")
	require.Len(t, done, 1)
	require.Equal(t, "exec_command", gjson.Get(done[0][len("data: "):], "item.name").String())
}

// 非成功终态（incomplete/failed）的 output 同样改写；终态前仍在缓冲的条目按
// 终态里的完整条目补发事件；吞掉增量时回调进度。
func TestOpenAIBpsStreamRelayTerminalFlushAndRewrite(t *testing.T) {
	for _, terminal := range []string{"response.incomplete", "response.failed", "response.completed"} {
		t.Run(terminal, func(t *testing.T) {
			relay := newOpenAIBpsToolStreamRelay(openAIBpsTestSpecs(), "apikey:9")
			progress := 0
			relay.onProgress = func() { progress++ }
			callID := fmt.Sprintf("call_term_%s", terminal)
			native := openAIBpsRelayTransportItem("fc_term", callID, `{"name":"exec_command","arguments":{"cmd":"pwd"}}`)
			skeleton := map[string]any{"type": "function_call", "id": "fc_term", "call_id": callID, "name": "run_officejs", "arguments": ""}

			var lines []string
			lines = append(lines, openAIBpsSSELines("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": skeleton})...)
			lines = append(lines, openAIBpsSSELines("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc_term", "output_index": 0, "delta": "{"})...)
			// 上游没发 output_item.done 就直接终态。
			lines = append(lines, openAIBpsSSELines(terminal, map[string]any{
				"type":     terminal,
				"response": map[string]any{"id": "resp_t", "status": "x", "output": []any{native}},
			})...)
			_, emitted, passed := driveOpenAIBpsRelay(relay, lines)

			require.Equal(t, 1, progress)
			for _, line := range passed {
				require.NotContains(t, line, "run_officejs")
			}
			done := findOpenAIBpsEmittedData(emitted, "response.output_item.done")
			require.Len(t, done, 1, "缓冲条目必须在终态前补发")
			require.Equal(t, "exec_command", gjson.Get(done[0][len("data: "):], "item.name").String())
			term := findOpenAIBpsEmittedData(emitted, terminal)
			require.Len(t, term, 1)
			require.Equal(t, "exec_command", gjson.Get(term[0][len("data: "):], "response.output.0.name").String())
			// 补发的 item.done 位于终态之前。
			doneIdx, termIdx := -1, -1
			for i, line := range emitted {
				if line == "event: response.output_item.done" && doneIdx < 0 {
					doneIdx = i
				}
				if line == "event: "+terminal {
					termIdx = i
				}
			}
			require.Less(t, doneIdx, termIdx)
			// 原生条目进了本 scope 的回放缓存，其他 scope 不可见。
			_, hit := openAIBpsNativeCalls.Recall("apikey:9", callID)
			require.True(t, hit)
			_, hit = openAIBpsNativeCalls.Recall("apikey:10", callID)
			require.False(t, hit)
		})
	}
}

// 提取失败的原生条目也写入回放缓存（客户端拿到的就是原生条目）。
func TestOpenAIBpsStreamRelayRemembersFailedExtraction(t *testing.T) {
	relay := newOpenAIBpsToolStreamRelay(openAIBpsTestSpecs(), "apikey:11")
	native := openAIBpsRelayTransportItem("fc_bad2", "call_bad2", `{not json`)
	skeleton := map[string]any{"type": "function_call", "id": "fc_bad2", "call_id": "call_bad2", "name": "run_officejs", "arguments": ""}
	var lines []string
	lines = append(lines, openAIBpsSSELines("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": skeleton})...)
	lines = append(lines, openAIBpsSSELines("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": native})...)
	_, emitted, _ := driveOpenAIBpsRelay(relay, lines)
	done := findOpenAIBpsEmittedData(emitted, "response.output_item.done")
	require.Len(t, done, 1)
	require.Equal(t, "run_officejs", gjson.Get(done[0][len("data: "):], "item.name").String())
	remembered, hit := openAIBpsNativeCalls.Recall("apikey:11", "call_bad2")
	require.True(t, hit)
	require.Equal(t, native["arguments"], remembered["arguments"])
}
