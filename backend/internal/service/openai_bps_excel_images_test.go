package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const openAIBpsTestImage = `{"type":"input_image","image_url":"data:image/png;base64,AAAA"}`

func openAIBpsImageBody(items ...string) []byte {
	body := `{"model":"gpt-6-astra","stream":true,"input":[` +
		`{"type":"additional_tools","role":"developer","tools":[{"type":"custom","name":"exec"}]}`
	for _, item := range items {
		body += "," + item
	}
	return []byte(body + `]}`)
}

// 资格：user 消息与工具输出里的 URL 图片可改道；改写处理不了的位置留 codex。
func TestOpenAIBpsImagePlacementEligibility(t *testing.T) {
	account := openAIBpsExcelTestAccount(map[string]any{OpenAIBpsExcelEnabledExtraKey: true})
	c := openAIBpsExcelResponsesContext()
	eligible := func(items ...string) bool {
		return openAIBpsExcelRequestEligible(context.Background(), c, account, openAIBpsImageBody(items...))
	}
	call := `{"type":"custom_tool_call","call_id":"c1","name":"exec","input":"image(x)"}`
	userText := `{"type":"message","role":"user","content":[{"type":"input_text","text":"shot"}]}`

	require.True(t, eligible(userText, call, `{"type":"custom_tool_call_output","call_id":"c1","output":[{"type":"input_text","text":"ok"},`+openAIBpsTestImage+`]}`))
	require.True(t, eligible(userText, call, `{"type":"function_call_output","call_id":"c1","output":[`+openAIBpsTestImage+`]}`))
	require.True(t, eligible(`{"type":"message","role":"user","content":[{"type":"input_text","text":"what"},`+openAIBpsTestImage+`]}`))
	// 无 type 的 {role,content} 消息同样按消息判定。
	require.True(t, eligible(`{"role":"user","content":[`+openAIBpsTestImage+`]}`))
	// 文本里提到 input_image 字样不算图片。
	require.True(t, eligible(userText, call, `{"type":"custom_tool_call_output","call_id":"c1","output":"the input_image field"}`))

	require.False(t, eligible(`{"type":"message","role":"developer","content":[`+openAIBpsTestImage+`]}`))
	require.False(t, eligible(`{"type":"message","role":"assistant","content":[`+openAIBpsTestImage+`]}`))
	// file_id 引用 bps 无从解析。
	require.False(t, eligible(`{"type":"message","role":"user","content":[{"type":"input_image","file_id":"file-1"}]}`))
	require.False(t, eligible(userText, call, `{"type":"custom_tool_call_output","call_id":"c1","output":[{"type":"input_image","file_id":"file-1"}]}`))
	// chat completions messages 里的图片没实测，留 codex。
	require.False(t, openAIBpsExcelRequestEligible(context.Background(), c, account,
		[]byte(`{"model":"gpt-6-astra","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)))
}

// 改写：bps 只在 function_call_output 里收图（2026-09-28 直连实测，其余位置 422）。
func TestRewriteOpenAIBodyForBpsExcelMovesImagesIntoFunctionOutputs(t *testing.T) {
	body := openAIBpsImageBody(
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"what color"},`+openAIBpsTestImage+`]}`,
		`{"type":"message","role":"user","content":[`+openAIBpsTestImage+`,`+openAIBpsTestImage+`]}`,
		`{"type":"custom_tool_call","call_id":"c1","name":"exec","input":"image(x)"}`,
		`{"type":"custom_tool_call_output","call_id":"c1","output":[{"type":"input_text","text":"ok"},`+openAIBpsTestImage+`]}`,
		`{"type":"custom_tool_call","call_id":"c2","name":"exec","input":"ls"}`,
		`{"type":"custom_tool_call_output","call_id":"c2","output":[{"type":"input_text","text":"a.txt"}]}`,
	)
	rewritten, _, err := rewriteOpenAIBodyForBpsExcel(body, "scope-images")
	require.NoError(t, err)

	// 不变量：图片只出现在 function_call_output 里。
	input := gjson.GetBytes(rewritten, "input").Array()
	images := 0
	for _, item := range input {
		itemType := item.Get("type").String()
		for _, key := range []string{"content", "output"} {
			item.Get(key).ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() == "input_image" {
					images++
					require.Equal(t, "function_call_output", itemType, "image left in %s", itemType)
				}
				return true
			})
		}
	}
	require.Equal(t, 4, images)

	find := func(pred func(gjson.Result) bool) []gjson.Result {
		var out []gjson.Result
		for _, item := range input {
			if pred(item) {
				out = append(out, item)
			}
		}
		return out
	}
	// user 消息：文本留在消息里，图片进合成 user_attachment 调用对，调用紧跟其输出。
	calls := find(func(i gjson.Result) bool { return i.Get("name").String() == openAIBpsAttachmentToolName })
	require.Len(t, calls, 2)
	require.NotEqual(t, calls[0].Get("call_id").String(), calls[1].Get("call_id").String())
	for idx, item := range input {
		if item.Get("name").String() != openAIBpsAttachmentToolName {
			continue
		}
		require.Equal(t, "function_call", item.Get("type").String())
		next := input[idx+1]
		require.Equal(t, "function_call_output", next.Get("type").String())
		require.Equal(t, item.Get("call_id").String(), next.Get("call_id").String())
		require.Equal(t, "message", input[idx-1].Get("type").String())
	}
	textMsgs := find(func(i gjson.Result) bool { return i.Get("role").String() == "user" })
	require.Len(t, textMsgs, 2)
	require.Equal(t, "what color", textMsgs[0].Get("content.0.text").String())
	require.Contains(t, textMsgs[1].Get("content.0.text").String(), "Attached 2 image(s)")

	// custom 输出：带图的换成 function_call_output，call_id 不变；不带图的原样。
	c1 := find(func(i gjson.Result) bool {
		return i.Get("call_id").String() == "c1" && i.Get("output").Exists()
	})
	require.Len(t, c1, 1)
	require.Equal(t, "function_call_output", c1[0].Get("type").String())
	c2 := find(func(i gjson.Result) bool {
		return i.Get("call_id").String() == "c2" && i.Get("output").Exists()
	})
	require.Equal(t, "custom_tool_call_output", c2[0].Get("type").String())

	// 逐字节稳定：同一请求重写两次结果一致（合成 call_id 确定性派生，prompt cache 前缀不断）。
	again, _, err := rewriteOpenAIBodyForBpsExcel(body, "scope-images")
	require.NoError(t, err)
	require.Equal(t, string(gjson.GetBytes(rewritten, "input").Raw), string(gjson.GetBytes(again, "input").Raw))

	// 下一轮追加历史后，前面的合成条目不变。
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(body, &decoded))
	decoded["input"] = append(decoded["input"].([]any), map[string]any{"type": "message", "role": "user", "content": "next"})
	grown, err := json.Marshal(decoded)
	require.NoError(t, err)
	next, _, err := rewriteOpenAIBodyForBpsExcel(grown, "scope-images")
	require.NoError(t, err)
	for i := range input {
		require.JSONEq(t, input[i].Raw, gjson.GetBytes(next, "input").Array()[i].Raw)
	}
}
