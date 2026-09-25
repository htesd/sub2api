package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func openAIBpsTestSpecs() map[string]openAIBpsClientToolSpec {
	body := map[string]any{
		"tools": []any{
			map[string]any{
				"type": "function", "name": "exec_command", "description": "run a command",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"cmd": map[string]any{"type": "string"}},
					"required":   []any{"cmd"},
				},
			},
			map[string]any{"type": "custom", "name": "apply_patch"},
			map[string]any{
				"type": "namespace", "name": "mcp",
				"tools": []any{
					map[string]any{"type": "function", "name": "search", "parameters": map[string]any{"type": "object"}},
				},
			},
		},
	}
	return openAIBpsClientToolSpecs(body)
}

func TestOpenAIBpsClientToolSpecs(t *testing.T) {
	specs := openAIBpsTestSpecs()
	require.Len(t, specs, 3)
	require.Equal(t, "exec_command", specs["exec_command"].Name)
	require.Equal(t, "custom", specs["apply_patch"].Type)
	require.Equal(t, "mcp", specs["mcp.search"].Namespace)
	require.Equal(t, "search", specs["mcp.search"].Name)

	// tool_choice:"none" 视为无工具。
	none := map[string]any{
		"tool_choice": "none",
		"tools":       []any{map[string]any{"type": "function", "name": "x"}},
	}
	require.Empty(t, openAIBpsClientToolSpecs(none))
	require.Empty(t, openAIBpsClientToolSpecs(map[string]any{}))
	require.Empty(t, openAIBpsClientToolSpecs(nil))
}

func TestOpenAIBpsCatalogInstructionsAndReminder(t *testing.T) {
	specs := openAIBpsTestSpecs()
	instructions := openAIBpsCatalogInstructions(specs)
	require.Contains(t, instructions, "run_officejs")
	require.Contains(t, instructions, `"exec_command"`)
	require.Contains(t, instructions, `"mcp.search"`)
	require.Contains(t, instructions, `"namespace":"mcp"`)

	reminder := openAIBpsCatalogReminder(specs)
	require.Contains(t, reminder, "exec_command")
	require.Contains(t, reminder, "apply_patch")
	// custom 工具提示走 input。
	require.Contains(t, reminder, `"input":"RAW_INPUT"`)

	// 无工具 → 外部客户端提示，无 reminder。
	require.Equal(t, openAIBpsExternalClientInstructions, openAIBpsCatalogInstructions(nil))
	require.Empty(t, openAIBpsCatalogReminder(nil))
}

func TestOpenAIBpsRepairInvalidJSONBackslashes(t *testing.T) {
	// 非法转义的反斜杠被双写（修复后 JSON 可解析且内容保真）。
	require.Equal(t, `{"cmd":"ls \\( -name x"}`, openAIBpsRepairInvalidJSONBackslashes(`{"cmd":"ls \( -name x"}`))
	// 合法转义不动。
	require.Equal(t, `{"a":"b\nC:\"}`, openAIBpsRepairInvalidJSONBackslashes(`{"a":"b\nC:\"}`))
	require.Equal(t, `{"a":"A"}`, openAIBpsRepairInvalidJSONBackslashes(`{"a":"A"}`))
}

func TestOpenAIBpsDecodeTransportCode(t *testing.T) {
	// 直接是对象。
	obj := map[string]any{"name": "exec_command"}
	require.Equal(t, obj, openAIBpsDecodeTransportCode(obj))
	// JSON 文本。
	decoded := openAIBpsDecodeTransportCode(`{"name":"exec_command","arguments":{"cmd":"pwd"}}`)
	require.Equal(t, "exec_command", decoded["name"])
	// 反斜杠修复。
	decoded = openAIBpsDecodeTransportCode(`{"name":"exec_command","arguments":{"cmd":"ls \( foo"}}`)
	require.NotNil(t, decoded)
	require.Equal(t, `ls \( foo`, decoded["arguments"].(map[string]any)["cmd"])
	// 代码围栏/赋值包装：取第一个完整 JSON 对象。
	decoded = openAIBpsDecodeTransportCode("```json\n{\"name\":\"exec_command\"}\n```")
	require.Equal(t, "exec_command", decoded["name"])
	// 非 JSON → nil。
	require.Nil(t, openAIBpsDecodeTransportCode("not json at all"))
	require.Nil(t, openAIBpsDecodeTransportCode(42))
}

func openAIBpsNativeTransportCall(envelope string) map[string]any {
	args, _ := json.Marshal(map[string]any{
		"summary": "s", "extended_summary": "es", "destructive": false, "references": []any{},
		"code": envelope,
	})
	return map[string]any{
		"type":      "function_call",
		"id":        "fc_native_1",
		"call_id":   "call_native_1",
		"name":      "run_officejs",
		"arguments": string(args),
	}
}

func TestOpenAIBpsTransportEnvelope(t *testing.T) {
	native := openAIBpsNativeTransportCall(`{"name":"exec_command","arguments":{"cmd":"pwd"}}`)
	envelope := openAIBpsTransportEnvelope(native)
	require.Equal(t, "exec_command", envelope["name"])

	// functions. 前缀别名同样识别。
	aliased := openAIBpsNativeTransportCall(`{"name":"exec_command","arguments":{}}`)
	aliased["name"] = "functions.run_officejs"
	require.NotNil(t, openAIBpsTransportEnvelope(aliased))

	// 嵌套一层传输信封 → 解出内层。
	inner, _ := json.Marshal(map[string]any{"name": "exec_command", "arguments": map[string]any{"cmd": "ls"}})
	nestedCode, _ := json.Marshal(map[string]any{"name": "run_officejs", "arguments": map[string]any{"code": string(inner)}})
	nested := openAIBpsNativeTransportCall(string(nestedCode))
	envelope = openAIBpsTransportEnvelope(nested)
	require.NotNil(t, envelope)
	require.Equal(t, "exec_command", envelope["name"])

	// 解到最后还是传输名 → 失败。
	deepCode := `{"name":"run_officejs","arguments":{"code":"{\"name\":\"functions.run_officejs\",\"arguments\":{\"code\":\"{\\\"name\\\":\\\"run_officejs\\\"}\"}}"}}`
	require.Nil(t, openAIBpsTransportEnvelope(openAIBpsNativeTransportCall(deepCode)))

	// 非 function_call / 非传输名 → nil。
	require.Nil(t, openAIBpsTransportEnvelope(map[string]any{"type": "message"}))
	require.Nil(t, openAIBpsTransportEnvelope(map[string]any{"type": "function_call", "name": "update_plan", "arguments": "{}"}))
}

func TestOpenAIBpsValueMatchesSchema(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"required":   []any{"cmd"},
		"properties": map[string]any{"cmd": map[string]any{"type": "string"}, "tty": map[string]any{"type": "boolean"}},
	}
	require.True(t, openAIBpsValueMatchesSchema(map[string]any{"cmd": "ls"}, schema))
	require.False(t, openAIBpsValueMatchesSchema(map[string]any{}, schema))
	require.False(t, openAIBpsValueMatchesSchema(map[string]any{"cmd": 1.0}, schema))
	require.True(t, openAIBpsValueMatchesSchema(map[string]any{"cmd": "ls", "tty": true}, schema))
	// 空 schema 放行。
	require.True(t, openAIBpsValueMatchesSchema(42.0, nil))
	// integer vs number。
	require.True(t, openAIBpsValueMatchesSchema(3.0, map[string]any{"type": "integer"}))
	require.False(t, openAIBpsValueMatchesSchema(3.5, map[string]any{"type": "integer"}))
	require.True(t, openAIBpsValueMatchesSchema(3.5, map[string]any{"type": "number"}))
	// type 列表。
	require.True(t, openAIBpsValueMatchesSchema("x", map[string]any{"type": []any{"string", "null"}}))
	require.True(t, openAIBpsValueMatchesSchema(nil, map[string]any{"type": []any{"string", "null"}}))
	// enum。
	require.True(t, openAIBpsValueMatchesSchema("a", map[string]any{"enum": []any{"a", "b"}}))
	require.False(t, openAIBpsValueMatchesSchema("c", map[string]any{"enum": []any{"a", "b"}}))
	// additionalProperties:false。
	strict := map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}, "additionalProperties": false}
	require.False(t, openAIBpsValueMatchesSchema(map[string]any{"a": "x", "b": 1.0}, strict))
}

func TestOpenAIBpsExtractNativeToolCall(t *testing.T) {
	specs := openAIBpsTestSpecs()

	// 传输信封 → 客户端 function_call，id/call_id 沿用原生条目。
	native := openAIBpsNativeTransportCall(`{"name":"exec_command","arguments":{"cmd":"pwd"}}`)
	extracted, ok := openAIBpsExtractNativeToolCall(native, specs)
	require.True(t, ok)
	require.Equal(t, "function_call", extracted["type"])
	require.Equal(t, "exec_command", extracted["name"])
	require.Equal(t, "call_native_1", extracted["call_id"])
	require.Equal(t, "fc_native_1", extracted["id"])
	require.JSONEq(t, `{"cmd":"pwd"}`, extracted["arguments"].(string))
	// 原生条目已进回放缓存。
	remembered, hit := openAIBpsNativeCalls.Recall("call_native_1")
	require.True(t, hit)
	require.Equal(t, "run_officejs", remembered["name"])

	// custom 工具走 input。
	customNative := openAIBpsNativeTransportCall(`{"name":"apply_patch","input":"*** Begin Patch\n*** End Patch"}`)
	extracted, ok = openAIBpsExtractNativeToolCall(customNative, specs)
	require.True(t, ok)
	require.Equal(t, "custom_tool_call", extracted["type"])
	require.Equal(t, "apply_patch", extracted["name"])
	require.Equal(t, "*** Begin Patch\n*** End Patch", extracted["input"])
	require.True(t, strings.HasPrefix(extracted["id"].(string), "ctc_call_"))

	// 命名空间工具按 key 还原，并带 namespace 字段。
	nsNative := openAIBpsNativeTransportCall(`{"name":"mcp.search","arguments":{}}`)
	extracted, ok = openAIBpsExtractNativeToolCall(nsNative, specs)
	require.True(t, ok)
	require.Equal(t, "search", extracted["name"])
	require.Equal(t, "mcp", extracted["namespace"])

	// 同名原生调用（update_plan）：无信封，参数归一到 Codex 形状。
	planSpecs := map[string]openAIBpsClientToolSpec{
		"update_plan": {Key: "update_plan", Name: "update_plan", Type: "function", Spec: map[string]any{}},
	}
	nativePlanArgs, _ := json.Marshal(map[string]any{
		"summary": "s",
		"plan":    []any{map[string]any{"id": "step1", "description": "do it", "status": "in_progress", "result": ""}},
	})
	nativePlan := map[string]any{
		"type": "function_call", "id": "fc_plan", "call_id": "call_plan",
		"name": "update_plan", "arguments": string(nativePlanArgs),
	}
	extracted, ok = openAIBpsExtractNativeToolCall(nativePlan, planSpecs)
	require.True(t, ok)
	require.JSONEq(t, `{"plan":[{"step":"do it","status":"in_progress"}],"explanation":"s"}`, extracted["arguments"].(string))

	// 信封畸形 → 不提取（原生条目直通，客户端报错后请求侧给重试指引）。
	malformed := openAIBpsNativeTransportCall(`{not json`)
	_, ok = openAIBpsExtractNativeToolCall(malformed, specs)
	require.False(t, ok)
	// 名字不在目录 → 不提取。
	unknown := openAIBpsNativeTransportCall(`{"name":"not_a_tool","arguments":{}}`)
	_, ok = openAIBpsExtractNativeToolCall(unknown, specs)
	require.False(t, ok)
	// 参数违反目录 schema → 不提取。
	badArgs := openAIBpsNativeTransportCall(`{"name":"exec_command","arguments":{"cmd":123}}`)
	_, ok = openAIBpsExtractNativeToolCall(badArgs, specs)
	require.False(t, ok)
	// 缺 cmd（required）→ 不提取。
	missing := openAIBpsNativeTransportCall(`{"name":"exec_command","arguments":{}}`)
	_, ok = openAIBpsExtractNativeToolCall(missing, specs)
	require.False(t, ok)
}

func TestOpenAIBpsTranslateInputItems(t *testing.T) {
	// 缓存命中：逐字节回放原生条目。
	native := openAIBpsNativeTransportCall(`{"name":"exec_command","arguments":{"cmd":"ls"}}`)
	openAIBpsNativeCalls.Remember(native)
	history := []any{
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}},
			"internal_chat_message_metadata_passthrough": map[string]any{"turn_id": "x"}},
		map[string]any{"type": "function_call", "call_id": "call_native_1", "name": "exec_command", "arguments": `{"cmd":"ls"}`},
		map[string]any{"type": "function_call_output", "call_id": "call_native_1", "output": ""},
	}
	translated, origins := openAIBpsTranslateInputItems(history)
	require.Len(t, translated, 3)
	// 客户端私有元数据被剥掉。
	require.NotContains(t, translated[0].(map[string]any), "internal_chat_message_metadata_passthrough")
	// 原生条目逐字节回放。
	require.Equal(t, "run_officejs", translated[1].(map[string]any)["name"])
	require.Equal(t, native["arguments"], translated[1].(map[string]any)["arguments"])
	// 空输出占位成功；origin 来自回放的原生条目名。
	require.Equal(t, "(tool call succeeded with no output)", translated[2].(map[string]any)["output"])
	require.Equal(t, "run_officejs", origins["call_native_1"])

	// 缓存未命中：按信封重合成 run_officejs 包装。
	miss := []any{
		map[string]any{"type": "function_call", "call_id": "call_miss", "name": "exec_command", "arguments": `{"cmd":"pwd"}`},
		map[string]any{"type": "function_call_output", "call_id": "call_miss", "output": "done"},
	}
	translated, origins = openAIBpsTranslateInputItems(miss)
	require.Len(t, translated, 2)
	wrapped := translated[0].(map[string]any)
	require.Equal(t, "run_officejs", wrapped["name"])
	var wrapArgs map[string]any
	require.NoError(t, json.Unmarshal([]byte(wrapped["arguments"].(string)), &wrapArgs))
	require.JSONEq(t, `{"name":"exec_command","arguments":{"cmd":"pwd"}}`, wrapArgs["code"].(string))
	require.Equal(t, openAIBpsFunctionItemID("call_miss"), translated[1].(map[string]any)["id"])
	require.Equal(t, "run_officejs", origins["call_miss"])

	// custom 工具调用包装用 input。
	customMiss := []any{
		map[string]any{"type": "custom_tool_call", "call_id": "call_cm", "name": "apply_patch", "input": "patch-body"},
		map[string]any{"type": "custom_tool_call_output", "call_id": "call_cm", "output": "applied"},
	}
	translated, _ = openAIBpsTranslateInputItems(customMiss)
	wrapped = translated[0].(map[string]any)
	require.NoError(t, json.Unmarshal([]byte(wrapped["arguments"].(string)), &wrapArgs))
	require.JSONEq(t, `{"name":"apply_patch","input":"patch-body"}`, wrapArgs["code"].(string))
	// 中继来源的 custom 输出改回 function_call_output。
	require.Equal(t, "function_call_output", translated[1].(map[string]any)["type"])

	// update_plan：调用参数还原原生 schema，输出还原 {"status":"ok"}。
	planHistory := []any{
		map[string]any{"type": "function_call", "call_id": "call_p", "name": "update_plan", "arguments": `{"plan":[{"step":"a","status":"done"}],"explanation":"e"}`},
		map[string]any{"type": "function_call_output", "call_id": "call_p", "output": "Plan updated"},
	}
	translated, origins = openAIBpsTranslateInputItems(planHistory)
	require.Equal(t, "update_plan", origins["call_p"])
	var planCallArgs map[string]any
	require.NoError(t, json.Unmarshal([]byte(translated[0].(map[string]any)["arguments"].(string)), &planCallArgs))
	require.Equal(t, "e", planCallArgs["summary"])
	require.Equal(t, "step1", planCallArgs["plan"].([]any)[0].(map[string]any)["id"])
	require.Equal(t, "a", planCallArgs["plan"].([]any)[0].(map[string]any)["description"])
	require.JSONEq(t, `{"status":"ok"}`, translated[1].(map[string]any)["output"].(string))

	// 客户端对未识别 run_officejs 的报错 → 协议重试指引。
	unsupported := []any{
		map[string]any{"type": "function_call", "call_id": "call_un", "name": "run_officejs", "arguments": `{}`},
		map[string]any{"type": "function_call_output", "call_id": "call_un", "output": "Unsupported call: run_officejs(...)"},
	}
	translated, _ = openAIBpsTranslateInputItems(unsupported)
	require.Equal(t, openAIBpsTransportRetryGuidance, translated[1].(map[string]any)["output"])

	// reasoning：裸条目丢弃，encrypted_content 保留且只留白名单字段。
	withReasoning := []any{
		map[string]any{"type": "reasoning", "summary": []any{map[string]any{"text": "s"}}, "content": "secret"},
		map[string]any{"type": "reasoning", "summary": []any{map[string]any{"text": "s"}}, "encrypted_content": "enc-1", "extra": "x"},
		map[string]any{"type": "item_reference", "id": "ref-1"},
	}
	translated, _ = openAIBpsTranslateInputItems(withReasoning)
	require.Len(t, translated, 1)
	require.Equal(t, "enc-1", translated[0].(map[string]any)["encrypted_content"])
	require.NotContains(t, translated[0].(map[string]any), "extra")

	// 字符串输入包装成 user 消息。
	translated, _ = openAIBpsTranslateInputItems("ping")
	require.Len(t, translated, 1)
	require.Equal(t, "user", translated[0].(map[string]any)["role"])
}

func TestOpenAIBpsRewriteCompletedToolCalls(t *testing.T) {
	specs := openAIBpsTestSpecs()
	native := openAIBpsNativeTransportCall(`{"name":"exec_command","arguments":{"cmd":"ls -la"}}`)
	native["status"] = "completed"

	// completed 事件外形。
	event := map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":     "resp_1",
			"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{}}, native},
		},
	}
	raw, _ := json.Marshal(event)
	rewritten, changed := openAIBpsRewriteCompletedToolCalls(raw, specs, nil)
	require.True(t, changed)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(rewritten, &decoded))
	output := decoded["response"].(map[string]any)["output"].([]any)
	require.Len(t, output, 2)
	replaced := output[1].(map[string]any)
	require.Equal(t, "exec_command", replaced["name"])
	require.Equal(t, "completed", replaced["status"])
	require.JSONEq(t, `{"cmd":"ls -la"}`, replaced["arguments"].(string))

	// 裸响应外形（非流式折叠后的响应体）。
	bare, _ := json.Marshal(map[string]any{"id": "resp_2", "output": []any{native}})
	rewritten, changed = openAIBpsRewriteCompletedToolCalls(bare, specs, nil)
	require.True(t, changed)
	require.NoError(t, json.Unmarshal(rewritten, &decoded))
	require.Equal(t, "exec_command", decoded["output"].([]any)[1-1].(map[string]any)["name"])

	// 无工具调用 / 无目录 → 不动。
	messageOnly, _ := json.Marshal(map[string]any{"output": []any{map[string]any{"type": "message"}}})
	_, changed = openAIBpsRewriteCompletedToolCalls(messageOnly, specs, nil)
	require.False(t, changed)
	_, changed = openAIBpsRewriteCompletedToolCalls(raw, nil, nil)
	require.False(t, changed)
}
