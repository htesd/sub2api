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

func observedUpstreamFasterModel(c *gin.Context) string {
	return upstreamResponseModelObserverFromContext(c).FasterModel()
}
