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
