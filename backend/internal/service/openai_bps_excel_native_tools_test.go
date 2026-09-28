package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// antistorm.22：客户端工具经 input[].additional_tools 原生声明（bps 原生支持，
// 2026-09-28 本地 codex 0.153.4/0.157.1 + opencode 实测），run_officejs 信封降为兜底。

// Codex ≥0.153 配 GPT-6 清单时的请求形状：顶层没有 tools，工具全在 additional_tools
// 载体里（code mode 只有 custom exec + 若干命名空间函数）。
func openAIBpsCodexCodeModeBody(extraInput ...string) []byte {
	input := `{"type":"additional_tools","id":"at_1","role":"developer","tools":[` +
		`{"type":"namespace","name":"functions","description":"","tools":[` +
		`{"type":"custom","name":"exec","description":"Run JavaScript","format":{"type":"grammar","syntax":"lark","definition":"start: /[\\s\\S]+/"}},` +
		`{"type":"function","name":"wait","parameters":{"type":"object","properties":{"ms":{"type":"number"}}}}]},` +
		`{"type":"namespace","name":"collaboration","description":"","tools":[` +
		`{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"task":{"type":"string"}},"required":["task"]}}]}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"cat a.txt"}]}`
	for _, extra := range extraInput {
		input += "," + extra
	}
	return []byte(`{"model":"gpt-6-astra","stream":true,"store":false,"tool_choice":"auto","parallel_tool_calls":false,` +
		`"reasoning":{"effort":"medium"},"prompt_cache_key":"sess-1","input":[` + input + `]}`)
}

func TestOpenAIBpsClientToolSpecsReadsAdditionalTools(t *testing.T) {
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(openAIBpsCodexCodeModeBody(), &decoded))
	specs := openAIBpsClientToolSpecs(decoded)
	require.Len(t, specs, 3)
	require.Equal(t, "custom", specs["functions.exec"].Type)
	require.Equal(t, "exec", specs["functions.exec"].Name)
	require.Equal(t, "collaboration", specs["collaboration.spawn_agent"].Namespace)
	// functions 命名空间在可见名里只列裸名（模型调用写的就是裸名）。
	require.Equal(t, []string{"collaboration.spawn_agent", "exec", "wait"}, openAIBpsClientToolNames(specs))
	// 裸名能解析到 functions 命名空间下的工具。
	spec, ok := openAIBpsResolveClientToolSpec("exec", specs)
	require.True(t, ok)
	require.Equal(t, "functions.exec", spec.Key)

	// tool_choice:"none" 时载体也不算。
	decoded["tool_choice"] = "none"
	require.Empty(t, openAIBpsClientToolSpecs(decoded))
}

// 客户端自带的 additional_tools 载体原样上行（不再在头部重复声明），序言针对
// code mode 提示嵌套工具要在 exec 的 JavaScript 里调；历史里的 exec 调用原样
// 保留（旧版重合成 run_officejs 信封，模型照着历史发信封导致死循环）。
func TestRewriteOpenAIBodyForBpsExcelCodexAdditionalTools(t *testing.T) {
	body := openAIBpsCodexCodeModeBody(
		`{"type":"custom_tool_call","id":"ctc_1","status":"completed","call_id":"call_exec_1","name":"exec","input":"text(1)"}`,
		`{"type":"custom_tool_call_output","id":"ctco_1","call_id":"call_exec_1","output":[{"type":"input_text","text":"1"}]}`,
	)
	rewritten, specs, err := rewriteOpenAIBodyForBpsExcel(body, "apikey:native-1")
	require.NoError(t, err)
	require.Len(t, specs, 3)
	require.False(t, gjson.GetBytes(rewritten, "tools").Exists())

	input := gjson.GetBytes(rewritten, "input").Array()
	// 序言 ×2 → 客户端载体 → user → exec 调用 → 输出。
	require.Len(t, input, 6)
	require.Equal(t, "developer", input[0].Get("role").String())
	prologue := input[0].Get("content.0.text").String()
	require.Contains(t, prologue, "call them directly")
	require.Contains(t, prologue, "inside the exec tool")
	require.Contains(t, prologue, "no update_plan tool")
	require.Equal(t, "additional_tools", input[2].Get("type").String())
	require.Equal(t, "at_1", input[2].Get("id").String(), "未过滤任何工具时载体逐字节保留")
	require.Equal(t, "custom_tool_call", input[4].Get("type").String())
	require.Equal(t, "exec", input[4].Get("name").String())
	require.Equal(t, "text(1)", input[4].Get("input").String())
	require.NotContains(t, string(rewritten), `"name":"run_officejs"`)
	// 全文只有一个 additional_tools 载体。
	count := 0
	for _, item := range input {
		if item.Get("type").String() == "additional_tools" {
			count++
		}
	}
	require.Equal(t, 1, count)
}

// 顶层 tools（opencode / 旧 Codex）挪进 input 头部的 additional_tools 载体：
// 只留 function/custom/namespace，托管工具丢弃；顶层 tools 字段不上行。
func TestRewriteOpenAIBodyForBpsExcelMovesTopLevelTools(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","stream":true,"tool_choice":"auto",
		"tools":[
			{"type":"function","name":"bash","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}},
			{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}},
			{"type":"web_search"},
			{"type":"namespace","name":"mcp","tools":[{"type":"function","name":"search"},{"type":"image_generation"}]}
		],
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"function_call","call_id":"call_b1","name":"bash","arguments":"{\"command\":\"ls\"}"},
			{"type":"function_call_output","call_id":"call_b1","output":"a.txt"}
		]}`)
	rewritten, specs, err := rewriteOpenAIBodyForBpsExcel(body, "apikey:native-2")
	require.NoError(t, err)
	require.Len(t, specs, 3)
	require.False(t, gjson.GetBytes(rewritten, "tools").Exists())
	require.False(t, gjson.GetBytes(rewritten, "tool_choice").Exists())

	input := gjson.GetBytes(rewritten, "input").Array()
	require.Equal(t, "additional_tools", input[0].Get("type").String())
	require.Equal(t, "developer", input[0].Get("role").String())
	declared := input[0].Get("tools").Array()
	require.Len(t, declared, 3, "web_search 丢弃")
	require.Equal(t, "bash", declared[0].Get("name").String())
	require.Equal(t, "apply_patch", declared[1].Get("name").String())
	require.Equal(t, "grammar", declared[1].Get("format.type").String())
	require.Equal(t, "mcp", declared[2].Get("name").String())
	require.Len(t, declared[2].Get("tools").Array(), 1, "命名空间里的托管工具同样丢弃")

	prologue := input[1].Get("content.0.text").String()
	require.Contains(t, prologue, "Client tools: apply_patch, bash, mcp.search.")
	require.NotContains(t, prologue, "inside the exec tool")
	// 历史调用原样（不重合成信封）。
	require.Equal(t, "function_call", input[4].Get("type").String())
	require.Equal(t, "bash", input[4].Get("name").String())
	require.Equal(t, `{"command":"ls"}`, input[4].Get("arguments").String())
	require.NotContains(t, string(rewritten), "run_officejs\",\"arguments")

	// 确定性：同一请求体重写两次逐字节一致（prompt cache 前缀稳定）。
	again, _, err := rewriteOpenAIBodyForBpsExcel(body, "apikey:native-2")
	require.NoError(t, err)
	require.Equal(t, string(rewritten), string(again))
}

// tool_choice:"none"：不声明任何工具，客户端自带载体也不上行。
func TestRewriteOpenAIBodyForBpsExcelToolChoiceNoneDropsDeclarations(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","tool_choice":"none","tools":[{"type":"function","name":"bash"}],"input":[
		{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"wait"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	rewritten, specs, err := rewriteOpenAIBodyForBpsExcel(body, "")
	require.NoError(t, err)
	require.Empty(t, specs)
	require.NotContains(t, string(rewritten), "additional_tools")
	require.Contains(t, gjson.GetBytes(rewritten, "input.0.content.0.text").String(), "external OpenAI Responses API client")
}

// additional_tools 载体里的托管工具与顶层同一规则：可丢弃的放行，其余留 codex。
func TestOpenAIBpsRequestToolsRelayableAdditionalTools(t *testing.T) {
	account := openAIBpsExcelTestAccount(map[string]any{OpenAIBpsExcelEnabledExtraKey: true})
	responsesCtx := openAIBpsExcelResponsesContext()
	body := func(tools string) []byte {
		return []byte(`{"model":"gpt-6-astra","input":[{"type":"additional_tools","role":"developer","tools":[` + tools +
			`]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	}
	require.True(t, openAIBpsExcelRequestEligible(context.Background(), responsesCtx, account, body(`{"type":"function","name":"wait"},{"type":"web_search"}`)))
	require.False(t, openAIBpsExcelRequestEligible(context.Background(), responsesCtx, account, body(`{"type":"function","name":"wait"},{"type":"mcp"}`)))
	// 载体里有客户端工具时同样要求 responses 入站。
	require.False(t, openAIBpsExcelRequestEligible(context.Background(), openAIBpsExcelMessagesContext(), account, body(`{"type":"function","name":"wait"}`)))
	require.True(t, openAIBpsExcelRequestEligible(context.Background(), responsesCtx, account, openAIBpsCodexCodeModeBody()))
}

// 客户端工具的直接调用（原生声明后的常态）不经拦截器，逐行直通；只有信封与
// 同名 update_plan 受管。
func TestOpenAIBpsStreamRelayPassesDirectClientCalls(t *testing.T) {
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(openAIBpsCodexCodeModeBody(), &decoded))
	relay := newOpenAIBpsToolStreamRelay(openAIBpsClientToolSpecs(decoded), "apikey:native-3")

	item := map[string]any{"type": "custom_tool_call", "id": "ctc_up_9", "call_id": "call_up_9", "name": "exec", "input": "", "status": "in_progress"}
	done := map[string]any{"type": "custom_tool_call", "id": "ctc_up_9", "call_id": "call_up_9", "name": "exec", "input": "text(2)", "status": "completed"}
	var lines []string
	lines = append(lines, openAIBpsSSELines("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})...)
	lines = append(lines, openAIBpsSSELines("response.custom_tool_call_input.delta", map[string]any{"type": "response.custom_tool_call_input.delta", "item_id": "ctc_up_9", "output_index": 0, "delta": "text(2)"})...)
	lines = append(lines, openAIBpsSSELines("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": done})...)
	lines = append(lines, openAIBpsSSELines("response.completed", map[string]any{"type": "response.completed",
		"response": map[string]any{"id": "resp_9", "status": "completed", "output": []any{done}}})...)

	_, emitted, passed := driveOpenAIBpsRelay(relay, lines)
	// data 行全部走正常处理、逐字节不变；受管事件族的 event: 行先暂存，data 行
	// 裁决放行后原样回流——回流的只能是这些 event: 行，不能有合成事件。
	for i := 1; i < len(lines); i += 2 {
		require.Contains(t, passed, lines[i])
	}
	require.Len(t, emitted, len(lines)/2)
	for i, line := range emitted {
		require.Equal(t, lines[2*i], line)
	}

	// completed 改写同一规则：直接调用不动。
	payload, _ := json.Marshal(map[string]any{"output": []any{done}})
	_, changed := openAIBpsRewriteCompletedToolCalls(payload, relay.specs, nil, "apikey:native-3")
	require.False(t, changed)
}

// 模型仍发 run_officejs 信封时（兜底），custom 工具按裸名还原；functions 默认
// 命名空间不写进调用（旧版写 spec.Key 会把 exec 改名成 functions.exec）。
func TestOpenAIBpsExtractEnvelopeCustomToolInFunctionsNamespace(t *testing.T) {
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(openAIBpsCodexCodeModeBody(), &decoded))
	specs := openAIBpsClientToolSpecs(decoded)

	native := openAIBpsNativeTransportCall(`{"name":"exec","input":"text(3)"}`)
	extracted, reason := openAIBpsExtractNativeToolCallReason(native, specs)
	require.Empty(t, reason)
	require.Equal(t, "custom_tool_call", extracted["type"])
	require.Equal(t, "exec", extracted["name"])
	require.NotContains(t, extracted, "namespace")
	require.Equal(t, "text(3)", extracted["input"])

	// 其他动态命名空间必须带 namespace。
	nsCall := openAIBpsNativeTransportCall(`{"name":"collaboration.spawn_agent","arguments":{"task":"x"}}`)
	extracted, reason = openAIBpsExtractNativeToolCallReason(nsCall, specs)
	require.Empty(t, reason)
	require.Equal(t, "spawn_agent", extracted["name"])
	require.Equal(t, "collaboration", extracted["namespace"])
}

// 客户端对 bps 原生工具（客户端没有的）回的 unsupported call 改写成指引，
// 列出可直接调用的客户端工具；update_plan 仍回 {"status":"ok"}。
func TestOpenAIBpsUnsupportedNativeCallGuidance(t *testing.T) {
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(openAIBpsCodexCodeModeBody(), &decoded))
	specs := openAIBpsClientToolSpecs(decoded)
	history := []any{
		map[string]any{"type": "function_call", "call_id": "call_ro", "name": "run_officejs", "arguments": `{"code":"x"}`},
		map[string]any{"type": "function_call_output", "call_id": "call_ro", "output": "unsupported call: run_officejs"},
		map[string]any{"type": "function_call", "call_id": "call_up", "name": "update_plan", "arguments": `{"summary":"s","plan":[]}`},
		map[string]any{"type": "function_call_output", "call_id": "call_up", "output": "unsupported call: update_plan"},
	}
	translated, _ := openAIBpsTranslateInputItems(history, specs, "apikey:native-4")
	guidance := translated[1].(map[string]any)["output"].(string)
	require.Contains(t, guidance, "run_officejs was not executed")
	require.Contains(t, guidance, "Call one of the client tools directly instead: collaboration.spawn_agent, exec, wait.")
	require.NotContains(t, guidance, "Retry once")
	require.JSONEq(t, `{"status":"ok"}`, translated[3].(map[string]any)["output"].(string))
}
