package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// driveOpenAIBpsRelay 模拟 processSSELine 的钩子契约驱动中继：返回被吞的行数、
// 回流（合成/补发）的行、以及应走正常处理的行。
func driveOpenAIBpsRelay(r *openAIBpsToolStreamRelay, lines []string) (swallowed int, emitted []string, passed []string) {
	for _, line := range lines {
		fed := false
		handled := r.hookLine(line, func(emit string) {
			emitted = append(emitted, emit)
			fed = true
		})
		if handled {
			if !fed {
				swallowed++
			}
			continue
		}
		passed = append(passed, line)
	}
	return swallowed, emitted, passed
}

func openAIBpsSSELines(eventType string, payload map[string]any) []string {
	raw, _ := json.Marshal(payload)
	return []string{"event: " + eventType, "data: " + string(raw)}
}

func TestOpenAIBpsToolStreamRelayInterceptsTransportCall(t *testing.T) {
	specs := openAIBpsTestSpecs()
	relay := newOpenAIBpsToolStreamRelay(specs, "")

	nativeArgs, _ := json.Marshal(map[string]any{
		"summary": "Check weather", "extended_summary": "x", "destructive": false, "references": []any{},
		"code": `{"name":"exec_command","arguments":{"cmd":"curl wttr.in"}}`,
	})
	nativeItem := map[string]any{
		"type": "function_call", "id": "fc_up_1", "call_id": "call_up_1",
		"name": "run_officejs", "arguments": string(nativeArgs), "status": "completed",
	}

	var lines []string
	// 前置事件：response.created 直通。
	lines = append(lines, openAIBpsSSELines("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_1"}})...)
	lines = append(lines, "")
	// output_item.added（骨架）。
	skeleton := map[string]any{"type": "function_call", "id": "fc_up_1", "call_id": "call_up_1", "name": "run_officejs", "arguments": ""}
	lines = append(lines, openAIBpsSSELines("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": skeleton})...)
	lines = append(lines, "")
	// 参数增量 × 2 + done。
	lines = append(lines, openAIBpsSSELines("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc_up_1", "output_index": 0, "delta": `{"summary":"Che`})...)
	lines = append(lines, openAIBpsSSELines("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc_up_1", "output_index": 0, "delta": `ck weather"...`})...)
	lines = append(lines, openAIBpsSSELines("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "item_id": "fc_up_1", "output_index": 0, "arguments": string(nativeArgs)})...)
	// output_item.done（完整原生条目）。
	lines = append(lines, openAIBpsSSELines("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": nativeItem})...)
	lines = append(lines, "")
	// completed：output 里仍是原生条目。
	lines = append(lines, openAIBpsSSELines("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{"id": "resp_1", "status": "completed", "output": []any{nativeItem},
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 5}},
	})...)
	lines = append(lines, "")

	swallowed, emitted, passed := driveOpenAIBpsRelay(relay, lines)

	// response.created 的 event/data 行与空行直通。
	require.Contains(t, passed, `event: response.created`)
	require.Contains(t, passed, "")
	// 原生调用的 added/delta/done 全部被吞（不下行）。
	for _, line := range passed {
		require.NotContains(t, line, "run_officejs")
		require.NotContains(t, line, `" Che`)
	}
	require.Greater(t, swallowed, 0)

	// 合成事件：客户端形状的 added → args.done → item.done。
	var addedData, argsDoneData, itemDoneData, completedData string
	for i := 0; i+1 < len(emitted); i++ {
		switch emitted[i] {
		case "event: response.output_item.added":
			addedData = emitted[i+1]
		case "event: response.function_call_arguments.done":
			argsDoneData = emitted[i+1]
		case "event: response.output_item.done":
			itemDoneData = emitted[i+1]
		case "event: response.completed":
			completedData = emitted[i+1]
		}
	}
	require.NotEmpty(t, addedData)
	require.NotEmpty(t, argsDoneData)
	require.NotEmpty(t, itemDoneData)
	require.NotEmpty(t, completedData)

	added := gjsonGet(t, addedData, "")
	require.Equal(t, "exec_command", added.Get("item.name").String())
	require.Equal(t, "", added.Get("item.arguments").String())
	require.Equal(t, "fc_up_1", added.Get("item.id").String())

	argsDone := gjsonGet(t, argsDoneData, "")
	require.JSONEq(t, `{"cmd":"curl wttr.in"}`, argsDone.Get("arguments").String())

	itemDone := gjsonGet(t, itemDoneData, "")
	require.Equal(t, "exec_command", itemDone.Get("item.name").String())
	require.Equal(t, "call_up_1", itemDone.Get("item.call_id").String())
	require.Equal(t, "completed", itemDone.Get("item.status").String())

	completed := gjsonGet(t, completedData, "")
	require.Equal(t, "exec_command", completed.Get("response.output.0.name").String())
	// usage 等其余字段原样保留。
	require.Equal(t, int64(10), completed.Get("response.usage.input_tokens").Int())

	// 原生条目已缓存，供下一轮历史回放。
	_, hit := openAIBpsNativeCalls.Recall("", "call_up_1")
	require.True(t, hit)
}

func TestOpenAIBpsToolStreamRelayPassthrough(t *testing.T) {
	specs := openAIBpsTestSpecs()
	relay := newOpenAIBpsToolStreamRelay(specs, "")

	var lines []string
	// 消息条目：output_item.added 是受管事件族但内容为 message → 直通。
	lines = append(lines, openAIBpsSSELines("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 0,
		"item": map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "content": []any{}},
	})...)
	lines = append(lines, openAIBpsSSELines("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": "msg_1", "delta": "he"})...)
	// 目录外原生工具调用（如 list_skills）→ 直通，客户端报 unsupported call 后自愈。
	lines = append(lines, openAIBpsSSELines("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 1,
		"item": map[string]any{"type": "function_call", "id": "fc_x", "call_id": "call_x", "name": "list_skills", "arguments": ""},
	})...)
	lines = append(lines, openAIBpsSSELines("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc_x", "delta": "{}"})...)
	lines = append(lines, openAIBpsSSELines("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": 1,
		"item": map[string]any{"type": "function_call", "id": "fc_x", "call_id": "call_x", "name": "list_skills", "arguments": "{}", "status": "completed"},
	})...)

	_, _, passed := driveOpenAIBpsRelay(relay, lines)
	joined := strings.Join(passed, "\n")
	require.Contains(t, joined, "response.output_text.delta")
	require.Contains(t, joined, "list_skills")
}

func TestOpenAIBpsToolStreamRelayMalformedEnvelopePassthrough(t *testing.T) {
	specs := openAIBpsTestSpecs()
	relay := newOpenAIBpsToolStreamRelay(specs, "")

	nativeItem := map[string]any{
		"type": "function_call", "id": "fc_bad", "call_id": "call_bad",
		"name": "run_officejs", "arguments": `{"code":"{not json"}`, "status": "completed",
	}
	var lines []string
	lines = append(lines, openAIBpsSSELines("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 0,
		"item": map[string]any{"type": "function_call", "id": "fc_bad", "call_id": "call_bad", "name": "run_officejs", "arguments": ""},
	})...)
	lines = append(lines, openAIBpsSSELines("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": nativeItem})...)

	_, emitted, passed := driveOpenAIBpsRelay(relay, lines)
	require.Empty(t, passed)
	// 提取失败 → 原生形状补发（客户端报 unsupported call 后请求侧给重试指引）。
	joined := strings.Join(emitted, "\n")
	require.Contains(t, joined, "run_officejs")
	require.Contains(t, joined, `{not json`)
}

func TestOpenAIBpsToolStreamRelayCustomToolCall(t *testing.T) {
	specs := openAIBpsTestSpecs()
	relay := newOpenAIBpsToolStreamRelay(specs, "")

	patch := "*** Begin Patch\n*** Update File: a.go\n*** End Patch"
	envelope, _ := json.Marshal(map[string]any{"name": "apply_patch", "input": patch})
	nativeArgs, _ := json.Marshal(map[string]any{
		"summary": "patch", "extended_summary": "x", "destructive": false, "references": []any{},
		"code": string(envelope),
	})
	nativeItem := map[string]any{
		"type": "function_call", "id": "fc_pt", "call_id": "call_pt",
		"name": "run_officejs", "arguments": string(nativeArgs), "status": "completed",
	}
	var lines []string
	lines = append(lines, openAIBpsSSELines("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 0,
		"item": map[string]any{"type": "function_call", "id": "fc_pt", "call_id": "call_pt", "name": "run_officejs", "arguments": ""},
	})...)
	lines = append(lines, openAIBpsSSELines("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": nativeItem})...)

	_, emitted, _ := driveOpenAIBpsRelay(relay, lines)
	joined := strings.Join(emitted, "\n")
	require.Contains(t, joined, "response.custom_tool_call_input.done")
	var doneData string
	for i := 0; i+1 < len(emitted); i++ {
		if emitted[i] == "event: response.output_item.done" {
			doneData = emitted[i+1]
		}
	}
	require.NotEmpty(t, doneData)
	item := gjsonGet(t, doneData, "item")
	require.Equal(t, "custom_tool_call", item.Get("type").String())
	require.Equal(t, "apply_patch", item.Get("name").String())
	require.Equal(t, patch, item.Get("input").String())
}

// gjsonGet 解析一行 "data: {...}" 的载荷并返回其中的 path 视图（空 path 为整体）。
func gjsonGet(t *testing.T, dataLine string, path string) gjson.Result {
	t.Helper()
	require.True(t, strings.HasPrefix(dataLine, "data: "), "line must be a data line: %q", dataLine)
	parsed := gjson.Parse(strings.TrimPrefix(dataLine, "data: "))
	if path == "" {
		return parsed
	}
	return parsed.Get(path)
}
