package service

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// 降智观察：上游对某一轮启用 safety buffering 时会在响应头
// X-Codex-Safety-Buffering-Faster-Model 里写出实际换成的模型。它和 response.model
// 对比是两条独立的证据——上游可能在 response.model 里仍报原模型名。
// 开了"降智观察"或"降智阻断"（阻断一定同时观察）时记录下来，写进
// usage_logs.upstream_faster_model；只记录，不改变任何转发、换号或计费行为。
//
// 同一观察点另外记两条旁证（antistorm.28）：
//   - X-Codex-Safety-Buffering-Enabled（true/false）→ usage_logs.upstream_safety_buffering
//   - x-codex-turn-state 的长度 → usage_logs.upstream_turn_state_len
//     （09-23 前 292/332 是未降级档、312/356 是降级档，之后恒为 780；记下来才能回答
//     "不带降级头的请求拿到的是什么票"）。

// openAICodexSafetyBufferingEnabledHeader 是上游是否对本轮启用 safety buffering 的响应头。
const openAICodexSafetyBufferingEnabledHeader = "X-Codex-Safety-Buffering-Enabled"

// ObserveOpenAIFasterModelHeader 在每次拿到上游 HTTP 响应后调用。每次都覆盖写入
// （没有该头就写空），同一次转发内重试时留下的是最后一次上游响应的值。
func (o *upstreamResponseModelObserver) ObserveOpenAIFasterModelHeader(account *Account, sentModel string, header http.Header) {
	if o == nil {
		return
	}
	fasterModel := ""
	if header != nil {
		fasterModel = normalizeObservedUpstreamResponseModel(header.Get(openAICodexSafetyBufferingModelHeader))
	}
	o.fasterModel = fasterModel
	o.safetyBuffering = parseOpenAICodexSafetyBufferingEnabled(header)
	o.turnStateLen = len(extractOpenAICodexTurnState(header))
	if fasterModel == "" {
		return
	}
	fields := []zap.Field{
		zap.String("sent_model", strings.TrimSpace(sentModel)),
		zap.String("faster_model", fasterModel),
		zap.String("upstream_request_id", strings.TrimSpace(header.Get("x-request-id"))),
	}
	if account != nil {
		fields = append(fields, zap.Int64("account_id", account.ID))
	}
	logger.L().Info("openai_upstream_faster_model_observed", fields...)
}

// parseOpenAICodexSafetyBufferingEnabled 把 X-Codex-Safety-Buffering-Enabled 解析成三态：
// 头缺失或取值无法识别时返回 nil（不臆测），true/false 原样返回。
func parseOpenAICodexSafetyBufferingEnabled(header http.Header) *bool {
	if header == nil {
		return nil
	}
	var v bool
	switch strings.ToLower(strings.TrimSpace(header.Get(openAICodexSafetyBufferingEnabledHeader))) {
	case "true", "1":
		v = true
	case "false", "0":
		v = false
	default:
		return nil
	}
	return &v
}

// observeOpenAIUpstreamFasterModel 是挂在 gin 上下文观察器上的版本。换号重试时观察器
// 会随新的转发尝试重建。还没有观察器的路径（如直接进入 passthrough）在这里补建，
// 后续响应处理只在观察器为空时才新建，不会把这里的记录丢掉。
func (s *OpenAIGatewayService) observeOpenAIUpstreamFasterModel(ctx context.Context, c *gin.Context, account *Account, sentModel string, header http.Header) {
	if c == nil || !s.openAICodexDowngradePolicy(ctx).Observing() {
		return
	}
	observer := upstreamResponseModelObserverFromContext(c)
	if observer == nil {
		observer = beginUpstreamResponseModelObservation(c)
	}
	observer.ObserveOpenAIFasterModelHeader(account, sentModel, header)
}

// FasterModel 返回本次尝试观察到的上游降级标记，没有则为空。
func (o *upstreamResponseModelObserver) FasterModel() string {
	if o == nil {
		return ""
	}
	return o.fasterModel
}

// SafetyBuffering 返回本次尝试观察到的 X-Codex-Safety-Buffering-Enabled；nil 表示没有该头。
func (o *upstreamResponseModelObserver) SafetyBuffering() *bool {
	if o == nil || o.safetyBuffering == nil {
		return nil
	}
	v := *o.safetyBuffering
	return &v
}

// TurnStateLen 返回本次尝试观察到的 x-codex-turn-state 长度；0 表示没有该头。
func (o *upstreamResponseModelObserver) TurnStateLen() int {
	if o == nil {
		return 0
	}
	return o.turnStateLen
}

func observedUpstreamFasterModel(c *gin.Context) string {
	return upstreamResponseModelObserverFromContext(c).FasterModel()
}

func observedUpstreamSafetyBuffering(c *gin.Context) *bool {
	return upstreamResponseModelObserverFromContext(c).SafetyBuffering()
}

func observedUpstreamTurnStateLen(c *gin.Context) int {
	return upstreamResponseModelObserverFromContext(c).TurnStateLen()
}
