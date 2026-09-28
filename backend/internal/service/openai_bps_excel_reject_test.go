package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// bps 拒收（400/422）后同请求不再改道；其他状态码、未走 bps 的尝试不受影响。
func TestMarkOpenAIBpsExcelRejected(t *testing.T) {
	account := openAIBpsExcelTestAccount(map[string]any{OpenAIBpsExcelEnabledExtraKey: true})
	c := openAIBpsExcelResponsesContext()
	body := openAIBpsExcelBody("gpt-6-astra")
	require.True(t, openAIBpsExcelRequestEligible(context.Background(), c, account, body))

	// 未走 bps 的尝试：不标记。
	c.Set(openAIBpsExcelUsedContextKey, false)
	require.False(t, markOpenAIBpsExcelRejected(c, account, http.StatusUnprocessableEntity, nil))

	c.Set(openAIBpsExcelUsedContextKey, true)
	// 鉴权/限流/5xx 仍走原 failover。
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusBadGateway} {
		require.False(t, markOpenAIBpsExcelRejected(c, account, status, nil), status)
	}
	c.Set(openAIBpsExcelBodyContextKey, []byte(`{"model":"gpt-6-astra","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"f"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"secret words"}]}]}`))
	require.True(t, markOpenAIBpsExcelRejected(c, account, http.StatusUnprocessableEntity, []byte(`{"error":{"message":"422: Invalid request body."}}`)))
	require.False(t, openAIBpsExcelRequestEligible(context.Background(), c, account, body), "拒收后同请求不再改道")
	// 只回落一次。
	require.False(t, markOpenAIBpsExcelRejected(c, account, http.StatusUnprocessableEntity, nil))
}

func TestOpenAIBpsBodyShapeSummaryHasNoContent(t *testing.T) {
	summary := openAIBpsBodyShapeSummary([]byte(`{"model":"m","input":[
		{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"f","defer_loading":true}]}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"secret words"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"more"}]},
		{"type":"function_call","call_id":"c","name":"f","arguments":"{}"}]}`))
	require.NotContains(t, summary, "secret")
	require.Contains(t, summary, "message:user*2")
	require.Contains(t, summary, "function_call{arguments,call_id,name,type}")
	require.Contains(t, summary, "  function{defer_loading,name,type}")
	require.Equal(t, "body=unavailable", openAIBpsBodyShapeSummary(nil))
}

// 工具输出里的图片（Codex exec 的 image() 截图）让 bps 直接 422：这类请求留 codex 上游。
func TestOpenAIBpsIneligibleForImageInToolOutput(t *testing.T) {
	account := openAIBpsExcelTestAccount(map[string]any{OpenAIBpsExcelEnabledExtraKey: true})
	c := openAIBpsExcelResponsesContext()
	withOutput := func(outputType, output string) []byte {
		return []byte(`{"model":"gpt-6-astra","input":[` +
			`{"type":"additional_tools","role":"developer","tools":[{"type":"custom","name":"exec"}]},` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"shot"}]},` +
			`{"type":"custom_tool_call","call_id":"c1","name":"exec","input":"image(x)"},` +
			`{"type":"` + outputType + `","call_id":"c1","output":` + output + `}]}`)
	}
	img := `[{"type":"input_text","text":"ok"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]`
	require.False(t, openAIBpsExcelRequestEligible(context.Background(), c, account, withOutput("custom_tool_call_output", img)))
	require.False(t, openAIBpsExcelRequestEligible(context.Background(), c, account, withOutput("function_call_output", img)))
	require.True(t, openAIBpsExcelRequestEligible(context.Background(), c, account, withOutput("custom_tool_call_output", `[{"type":"input_text","text":"ok"}]`)))
	// 文本里提到 input_image 字样不算图片。
	require.True(t, openAIBpsExcelRequestEligible(context.Background(), c, account, withOutput("custom_tool_call_output", `"the input_image field"`)))
}
