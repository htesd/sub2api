package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type fasterModelSettingRepoStub struct {
	SettingRepository
	values map[string]string
}

func (r *fasterModelSettingRepoStub) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}

func newFasterModelTestService(values map[string]string) *OpenAIGatewayService {
	return &OpenAIGatewayService{settingService: NewSettingService(&fasterModelSettingRepoStub{values: values}, nil)}
}

func newFasterModelTestContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c
}

func TestOpenAICodexDowngradePolicyObserving(t *testing.T) {
	require.False(t, OpenAICodexDowngradePolicy{}.Observing())
	require.True(t, OpenAICodexDowngradePolicy{Observe: true}.Observing())
	// 阻断一定同时观察。
	require.True(t, OpenAICodexDowngradePolicy{Enabled: true}.Observing())
}

func TestObserveOpenAIFasterModelHeaderRecordsAndOverwrites(t *testing.T) {
	observer := &upstreamResponseModelObserver{}
	header := http.Header{openAICodexSafetyBufferingModelHeader: []string{"  gpt-5.6-luna "}}
	observer.ObserveOpenAIFasterModelHeader(&Account{ID: 1}, "gpt-6-astra", header)
	require.Equal(t, "gpt-5.6-luna", observer.FasterModel())

	// 同一次转发内后一次上游响应没有该头时覆盖为空。
	observer.ObserveOpenAIFasterModelHeader(&Account{ID: 1}, "gpt-6-astra", http.Header{})
	require.Empty(t, observer.FasterModel())

	long := strings.Repeat("m", 500)
	observer.ObserveOpenAIFasterModelHeader(nil, "gpt-6-astra", http.Header{openAICodexSafetyBufferingModelHeader: []string{long}})
	require.LessOrEqual(t, len(observer.FasterModel()), 200)
	require.NotEmpty(t, observer.FasterModel())

	var nilObserver *upstreamResponseModelObserver
	require.NotPanics(t, func() { nilObserver.ObserveOpenAIFasterModelHeader(nil, "", nil) })
	require.Empty(t, nilObserver.FasterModel())
}

func TestObserveOpenAIUpstreamFasterModelRespectsSwitches(t *testing.T) {
	header := http.Header{openAICodexSafetyBufferingModelHeader: []string{"gpt-5.6-luna"}}
	cases := []struct {
		name   string
		values map[string]string
		want   string
	}{
		{name: "both off", values: map[string]string{}, want: ""},
		{name: "observe only", values: map[string]string{SettingKeyOpenAICodexDowngradeObserveEnabled: "true"}, want: "gpt-5.6-luna"},
		{name: "block implies observe", values: map[string]string{SettingKeyOpenAICodexDowngradeBlockEnabled: "true"}, want: "gpt-5.6-luna"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newFasterModelTestService(tc.values)
			c := newFasterModelTestContext()
			svc.observeOpenAIUpstreamFasterModel(context.Background(), c, &Account{ID: 9}, "gpt-6-astra", header)
			require.Equal(t, tc.want, observedUpstreamFasterModel(c))
		})
	}
}

func TestObserveOpenAIUpstreamFasterModelSurvivesLaterObserverLookup(t *testing.T) {
	svc := newFasterModelTestService(map[string]string{SettingKeyOpenAICodexDowngradeObserveEnabled: "true"})
	c := newFasterModelTestContext()
	// 还没有观察器的路径（如直接进入 passthrough）会补建，之后的响应处理复用同一个。
	svc.observeOpenAIUpstreamFasterModel(context.Background(), c, nil, "gpt-6-astra",
		http.Header{openAICodexSafetyBufferingModelHeader: []string{"gpt-5.6-luna"}})
	observer := upstreamResponseModelObserverFromContext(c)
	require.NotNil(t, observer)
	observer.ObserveOpenAI([]byte(`{"type":"response.completed","response":{"model":"gpt-6-astra"}}`), "response.completed")
	require.Equal(t, "gpt-5.6-luna", observedUpstreamFasterModel(c))
	require.Equal(t, "gpt-6-astra", observedUpstreamResponseModel(c))

	// 换号重试会重建观察器，上一个账号的标记不会带到下一个账号。
	beginUpstreamResponseModelObservation(c)
	require.Empty(t, observedUpstreamFasterModel(c))
}

func TestObserveOpenAIFasterModelHeaderRecordsTurnSignals(t *testing.T) {
	observer := &upstreamResponseModelObserver{}
	header := http.Header{}
	header.Set(openAICodexSafetyBufferingModelHeader, "gpt-6-luna")
	header.Set(openAICodexSafetyBufferingEnabledHeader, "true")
	header.Set(openAICodexTurnStateHeader, strings.Repeat("g", 780))
	observer.ObserveOpenAIFasterModelHeader(&Account{ID: 1}, "gpt-6-astra", header)
	require.Equal(t, 780, observer.TurnStateLen())
	require.NotNil(t, observer.SafetyBuffering())
	require.True(t, *observer.SafetyBuffering())

	// 下一次上游响应没有这些头：覆盖为"没有"，不能残留上一次的值。
	off := http.Header{}
	off.Set(openAICodexSafetyBufferingEnabledHeader, "False")
	observer.ObserveOpenAIFasterModelHeader(&Account{ID: 2}, "gpt-6-astra", off)
	require.Equal(t, 0, observer.TurnStateLen())
	require.NotNil(t, observer.SafetyBuffering())
	require.False(t, *observer.SafetyBuffering())

	observer.ObserveOpenAIFasterModelHeader(&Account{ID: 3}, "gpt-6-astra", http.Header{})
	require.Nil(t, observer.SafetyBuffering(), "missing header stays NULL")

	unknown := http.Header{}
	unknown.Set(openAICodexSafetyBufferingEnabledHeader, "maybe")
	observer.ObserveOpenAIFasterModelHeader(&Account{ID: 4}, "gpt-6-astra", unknown)
	require.Nil(t, observer.SafetyBuffering(), "unrecognized value is not guessed")

	var nilObserver *upstreamResponseModelObserver
	require.Nil(t, nilObserver.SafetyBuffering())
	require.Equal(t, 0, nilObserver.TurnStateLen())
}

func TestObservedTurnSignalsFlowFromContext(t *testing.T) {
	svc := newFasterModelTestService(map[string]string{SettingKeyOpenAICodexDowngradeObserveEnabled: "true"})
	c := newFasterModelTestContext()
	header := http.Header{}
	header.Set(openAICodexSafetyBufferingEnabledHeader, "true")
	header.Set(openAICodexTurnStateHeader, strings.Repeat("g", 292))
	svc.observeOpenAIUpstreamFasterModel(context.Background(), c, &Account{ID: 7}, "gpt-6-astra", header)
	require.Equal(t, 292, observedUpstreamTurnStateLen(c))
	require.NotNil(t, observedUpstreamSafetyBuffering(c))
	require.True(t, *observedUpstreamSafetyBuffering(c))

	require.Nil(t, optionalPositiveIntPtr(0))
	require.Equal(t, 292, *optionalPositiveIntPtr(292))
}
