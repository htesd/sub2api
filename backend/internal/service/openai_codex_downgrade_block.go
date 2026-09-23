package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// 降智阻断：上游对 gpt-6 请求启用 safety buffering 后会改用更快的模型（响应里
// response.model 变成 gpt-5.6-luna，并额外回一个 x-codex-turn-state 头）。这种响应
// HTTP 200、SSE 结构完整，既有的失败判据都发现不了它，客户端会静默拿到降级结果。
//
// 实测（2026-09-21）：账号一旦进入该状态，改请求形状（tools/reasoning effort/
// service_tier/originator）、换出口、注入合格票都无法恢复；而未进入该状态的账号用
// 最简请求也能拿到目标模型。所以判据只能是"这一轮实际回了哪个模型"，处置只能是
// 冷却该账号的该模型并换号。
const (
	openAICodexDowngradeDefaultMinutes = 30
	openAICodexDowngradeMaxMinutes     = 24 * 60
	openAICodexDowngradeDefaultModel   = "gpt-6-astra"
	openAICodexDowngradeErrorCode      = "openai_model_downgraded"
	openAICodexDowngradeReason         = "openai_model_downgraded"
	openAICodexDowngradeCacheTTL       = 5 * time.Second
	// 上游只在降级轮返回该响应头，可在读 body 之前作为旁证记录，但不单独作为判据。
	openAICodexSafetyBufferingModelHeader = "X-Codex-Safety-Buffering-Faster-Model"
)

// OpenAICodexDowngradePolicy 是降智阻断的生效配置。
type OpenAICodexDowngradePolicy struct {
	Enabled  bool
	Cooldown time.Duration
	Models   []string
}

// NormalizeOpenAICodexDowngradeMinutes 约束冷却时长，0/负值回落默认值。
func NormalizeOpenAICodexDowngradeMinutes(minutes int) int {
	if minutes <= 0 {
		return openAICodexDowngradeDefaultMinutes
	}
	if minutes > openAICodexDowngradeMaxMinutes {
		return openAICodexDowngradeMaxMinutes
	}
	return minutes
}

// NormalizeOpenAICodexDowngradeModels 支持逗号与换行分隔；空输入回落默认模型。
// 与打票名单一致地做精确匹配，不支持通配符，避免误伤其他模型。
func NormalizeOpenAICodexDowngradeModels(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t'
	})
	seen := map[string]struct{}{}
	models := make([]string, 0, len(fields))
	for _, field := range fields {
		model := strings.ToLower(strings.TrimSpace(field))
		if model == "" {
			continue
		}
		if _, exists := seen[model]; exists {
			continue
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	if len(models) == 0 {
		return []string{openAICodexDowngradeDefaultModel}
	}
	sort.Strings(models)
	return models
}

type cachedOpenAICodexDowngradePolicy struct {
	policy    OpenAICodexDowngradePolicy
	expiresAt int64
}

// ResolveOpenAICodexDowngradePolicy 读取后台设置（5 秒缓存），缺键时回落默认值：
// 关闭、30 分钟、仅 gpt-6-astra。
func (s *SettingService) ResolveOpenAICodexDowngradePolicy(ctx context.Context) OpenAICodexDowngradePolicy {
	fallback := OpenAICodexDowngradePolicy{
		Enabled:  false,
		Cooldown: openAICodexDowngradeDefaultMinutes * time.Minute,
		Models:   NormalizeOpenAICodexDowngradeModels(""),
	}
	if s == nil || s.settingRepo == nil {
		return fallback
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if cached, ok := s.openAICodexDowngradePolicyCache.Load().(*cachedOpenAICodexDowngradePolicy); ok && cached != nil {
		if time.Now().UnixNano() < cached.expiresAt {
			return cached.policy
		}
	}
	if ctx.Err() != nil {
		return fallback
	}
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	values, err := s.settingRepo.GetMultiple(dbCtx, []string{
		SettingKeyOpenAICodexDowngradeBlockEnabled,
		SettingKeyOpenAICodexDowngradeBlockMinutes,
		SettingKeyOpenAICodexDowngradeBlockModels,
	})
	if err != nil && !errors.Is(err, ErrSettingNotFound) {
		return fallback
	}
	policy := fallback
	if raw, ok := values[SettingKeyOpenAICodexDowngradeBlockEnabled]; ok {
		policy.Enabled = strings.TrimSpace(raw) == "true"
	}
	if raw, ok := values[SettingKeyOpenAICodexDowngradeBlockMinutes]; ok {
		if minutes, convErr := strconv.Atoi(strings.TrimSpace(raw)); convErr == nil {
			policy.Cooldown = time.Duration(NormalizeOpenAICodexDowngradeMinutes(minutes)) * time.Minute
		}
	}
	if raw, ok := values[SettingKeyOpenAICodexDowngradeBlockModels]; ok {
		policy.Models = NormalizeOpenAICodexDowngradeModels(raw)
	}
	s.openAICodexDowngradePolicyCache.Store(&cachedOpenAICodexDowngradePolicy{
		policy:    policy,
		expiresAt: time.Now().Add(openAICodexDowngradeCacheTTL).UnixNano(),
	})
	return policy
}

// InvalidateOpenAICodexDowngradePolicyCache 在保存设置后立即生效。
func (s *SettingService) InvalidateOpenAICodexDowngradePolicyCache() {
	if s == nil {
		return
	}
	s.openAICodexDowngradePolicyCache.Store((*cachedOpenAICodexDowngradePolicy)(nil))
}

func (s *OpenAIGatewayService) openAICodexDowngradePolicy(ctx context.Context) OpenAICodexDowngradePolicy {
	if s == nil || s.settingService == nil {
		return OpenAICodexDowngradePolicy{Cooldown: openAICodexDowngradeDefaultMinutes * time.Minute, Models: NormalizeOpenAICodexDowngradeModels("")}
	}
	return s.settingService.ResolveOpenAICodexDowngradePolicy(ctx)
}

func openAICodexDowngradeModelWatched(models []string, sentModel string) bool {
	sent := strings.ToLower(strings.TrimSpace(sentModel))
	if sent == "" {
		return false
	}
	for _, model := range models {
		if model == sent {
			return true
		}
	}
	return false
}

// openAICodexDowngradeObservedModel 从单个 SSE data 帧里读出上游声明的模型；
// 只认 response.model / model 两个位置，与票据判据保持一致。
func openAICodexDowngradeObservedModel(payload []byte) string {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return ""
	}
	for _, field := range gjson.GetManyBytes(payload, "response.model", "model") {
		if field.Exists() && field.Type == gjson.String {
			if model := strings.ToLower(strings.TrimSpace(field.String())); model != "" {
				return model
			}
		}
	}
	return ""
}

// openAICodexDowngraded 判断"送出模型属于名单且上游实际回了别的模型"。
func openAICodexDowngraded(sentModel, observedModel string) bool {
	sent := strings.ToLower(strings.TrimSpace(sentModel))
	observed := strings.ToLower(strings.TrimSpace(observedModel))
	return sent != "" && observed != "" && sent != observed
}

// noteOpenAICodexDowngrade 冷却该 (账号, 模型) 并返回换号用的 failover 错误。
// 进程内冷却立即生效；持久化冷却让其他实例与重启后也排除该账号。
func (s *OpenAIGatewayService) noteOpenAICodexDowngrade(ctx context.Context, c *gin.Context, account *Account,
	sentModel, observedModel, upstreamRequestID string, policy OpenAICodexDowngradePolicy, upstreamHeaders http.Header,
) *UpstreamFailoverError {
	if s == nil || account == nil {
		return nil
	}
	now := time.Now()
	cooldown := policy.Cooldown
	if cooldown <= 0 {
		cooldown = openAICodexDowngradeDefaultMinutes * time.Minute
	}
	until := now.Add(cooldown)
	canonicalModel := canonicalOpenAIAccountSchedulingModel(account, sentModel)
	if strings.TrimSpace(canonicalModel) == "" {
		canonicalModel = strings.TrimSpace(sentModel)
	}
	s.recordOpenAIAccountModelTransientFailure(account, canonicalModel, now)
	if s.accountRepo != nil && strings.TrimSpace(canonicalModel) != "" {
		// 判定发生在响应处理中途，请求 ctx 可能随客户端断开被取消；冷却必须写下去。
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := s.accountRepo.SetModelRateLimit(writeCtx, account.ID, canonicalModel, until, openAICodexDowngradeReason); err != nil {
			logger.L().Warn("openai_codex_downgrade cooldown persist failed",
				zap.Int64("account_id", account.ID), zap.String("model", canonicalModel), zap.Error(err))
		}
	}
	fasterModel := ""
	if upstreamHeaders != nil {
		fasterModel = strings.TrimSpace(upstreamHeaders.Get(openAICodexSafetyBufferingModelHeader))
	}
	logger.L().Warn("openai_codex_downgrade blocked",
		zap.Int64("account_id", account.ID),
		zap.String("account", account.Name),
		zap.String("sent_model", sentModel),
		zap.String("response_model", observedModel),
		zap.String("cooldown_model", canonicalModel),
		zap.String("faster_model_header", fasterModel),
		zap.Duration("cooldown", cooldown),
		zap.String("upstream_request_id", upstreamRequestID))
	return newOpenAICodexDowngradeFailoverError(c, account, sentModel, observedModel, upstreamRequestID)
}

func openAICodexDowngradeMessage(sentModel, observedModel string) string {
	return fmt.Sprintf("OpenAI upstream served %s instead of %s (safety buffering downgrade)",
		strings.TrimSpace(observedModel), strings.TrimSpace(sentModel))
}

// OpenAICodexDowngradeClientMessage 是所有账号都被降级后返回给客户端的文案。
func OpenAICodexDowngradeClientMessage() string {
	return "当前请求被 OpenAI 降级到更快的模型（safety buffering），已暂停相关账号，请重试以切换账号"
}

func openAICodexDowngradeErrorBody(sentModel, observedModel string) []byte {
	body, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    "upstream_error",
			"code":    openAICodexDowngradeErrorCode,
			"message": openAICodexDowngradeMessage(sentModel, observedModel),
		},
	})
	if err != nil {
		return []byte(`{"error":{"type":"upstream_error","code":"openai_model_downgraded","message":"OpenAI upstream downgraded the requested model"}}`)
	}
	return body
}

// IsOpenAICodexDowngradeErrorBody 供错误分类与测试识别该 failover 来源。
func IsOpenAICodexDowngradeErrorBody(body []byte) bool {
	return strings.TrimSpace(gjson.GetBytes(body, "error.code").String()) == openAICodexDowngradeErrorCode
}

// newOpenAICodexDowngradeFailoverError 构造换号错误：账号相关、不在同号重试、
// 耗尽后给客户端 429，让客户端重试时落到别的账号。
func newOpenAICodexDowngradeFailoverError(c *gin.Context, account *Account, sentModel, observedModel, upstreamRequestID string) *UpstreamFailoverError {
	message := openAICodexDowngradeMessage(sentModel, observedModel)
	accountID := int64(0)
	accountName := ""
	platform := PlatformOpenAI
	if account != nil {
		accountID = account.ID
		accountName = account.Name
		platform = account.Platform
	}
	setOpsUpstreamError(c, http.StatusBadGateway, message, openAICodexDowngradeErrorCode)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:            opsUpstreamProxyID(account),
		ProxyName:          opsUpstreamProxyName(account),
		Platform:           platform,
		AccountID:          accountID,
		AccountName:        accountName,
		UpstreamStatusCode: http.StatusBadGateway,
		UpstreamRequestID:  upstreamRequestID,
		Kind:               "failover",
		Message:            message,
	})
	headers := http.Header{}
	if trimmed := strings.TrimSpace(upstreamRequestID); trimmed != "" {
		headers.Set("x-request-id", trimmed)
	}
	return &UpstreamFailoverError{
		StatusCode:      http.StatusBadGateway,
		ResponseBody:    openAICodexDowngradeErrorBody(sentModel, observedModel),
		ResponseHeaders: headers,
		Scope:           GatewayFailureScopeAccount,
		// 同一账号重试只会再拿一次降级响应，直接换号。
		ClientStatusCode: http.StatusTooManyRequests,
		ClientMessage:    OpenAICodexDowngradeClientMessage(),
	}
}

// openAICodexDowngradeStreamCheck 在首个携带模型声明的 SSE 帧上做判定。
// 调用点必须保证客户端尚未收到语义字节，否则不得换号。
func (s *OpenAIGatewayService) openAICodexDowngradeStreamCheck(ctx context.Context, c *gin.Context, account *Account,
	sentModel string, payload []byte, upstreamRequestID string, upstreamHeaders http.Header, policy OpenAICodexDowngradePolicy,
) *UpstreamFailoverError {
	if !policy.Enabled || account == nil || account.Platform != PlatformOpenAI {
		return nil
	}
	if !openAICodexDowngradeModelWatched(policy.Models, sentModel) {
		return nil
	}
	observed := openAICodexDowngradeObservedModel(payload)
	if !openAICodexDowngraded(sentModel, observed) {
		return nil
	}
	return s.noteOpenAICodexDowngrade(ctx, c, account, sentModel, observed, upstreamRequestID, policy, upstreamHeaders)
}

// openAICodexDowngradeNonStreamCheck 处理非流式/整体读入的响应体：既支持 SSE 帧，
// 也支持单个 JSON 响应对象。
func (s *OpenAIGatewayService) openAICodexDowngradeNonStreamCheck(ctx context.Context, c *gin.Context, account *Account,
	sentModel string, body []byte, upstreamRequestID string, upstreamHeaders http.Header, policy OpenAICodexDowngradePolicy,
) *UpstreamFailoverError {
	if !policy.Enabled || account == nil || account.Platform != PlatformOpenAI {
		return nil
	}
	if !openAICodexDowngradeModelWatched(policy.Models, sentModel) {
		return nil
	}
	observed := ""
	if bodyHasSSEFraming(body) {
		for _, frame := range strings.Split(string(body), "\n") {
			data, ok := extractOpenAISSEDataLine(frame)
			if !ok {
				continue
			}
			if model := openAICodexDowngradeObservedModel([]byte(data)); model != "" {
				observed = model
				break
			}
		}
	} else {
		observed = openAICodexDowngradeObservedModel(body)
	}
	if !openAICodexDowngraded(sentModel, observed) {
		return nil
	}
	return s.noteOpenAICodexDowngrade(ctx, c, account, sentModel, observed, upstreamRequestID, policy, upstreamHeaders)
}
