package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const (
	openAICodexTicketExtraKeyPrefix = "codex_turn_ticket:"
	openAICodexAstraMinVersion      = "0.153.4"
	openAICodexTicketStatePrefix    = "gAAAAA"
	// openAICodexTicketFernetVersion 是 Fernet 令牌的版本字节；"gAAAAA" 前缀正是它
	// base64 之后的表现（0x80 → 'g'）。
	openAICodexTicketFernetVersion = 0x80

	// 采集限流默认值：32 并发、失败后 30s 起步指数退避、上限 15 分钟。
	defaultOpenAICodexTicketHarvestConcurrency = 32
	defaultOpenAICodexTicketBackoffBaseSeconds = 30
	defaultOpenAICodexTicketBackoffMaxSeconds  = 900
	openAICodexTicketDefaultModel              = "gpt-6-astra"
	openAICodexTicketDefaultSolModel           = "gpt-5.6-sol"
)

// ErrOpenAICodexTicketUnavailable 表示该号该模型没有可用的 292 门票，
// 且 fail_closed 禁止裸打业务请求。
var ErrOpenAICodexTicketUnavailable = errors.New("codex turn-state ticket unavailable")

type openAICodexTicket struct {
	AccountID  int64     `json:"account_id"`
	Model      string    `json:"model"`
	State      string    `json:"state"`
	Length     int       `json:"length"`
	CapturedAt time.Time `json:"captured_at"`
	// IssuedAt 是从 turn-state 的 Fernet 头部解出的签发时间；解析失败时为零值，
	// 此时有效期退回按 CapturedAt 计算。
	IssuedAt  time.Time `json:"issued_at,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	Attempts  int       `json:"attempts"`
}

// decodeOpenAICodexTicketIssuedAt 只解析 turn-state 的 Fernet 头部：raw[0] 是版本
// 字节（0x80），raw[1:9] 是大端 uint64 的签发时间戳（秒）。我们没有密钥，也不需要
// 明文，所以不做解密与 HMAC 校验——只要签发时间，用来替代"以本地捕获时刻起算 TTL"
// 这个假设，顺带能发现时钟偏差与结构异常的票。
func decodeOpenAICodexTicketIssuedAt(state string) (byte, time.Time, error) {
	state = strings.TrimSpace(state)
	if state == "" {
		return 0, time.Time{}, errors.New("empty turn-state")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(state, "="))
	if err != nil {
		return 0, time.Time{}, err
	}
	if len(raw) < 9 {
		return 0, time.Time{}, fmt.Errorf("turn-state too short: %d bytes", len(raw))
	}
	ts := binary.BigEndian.Uint64(raw[1:9])
	if ts == 0 || ts > uint64(math.MaxInt64) {
		return raw[0], time.Time{}, fmt.Errorf("turn-state timestamp out of range: %d", ts)
	}
	return raw[0], time.Unix(int64(ts), 0).UTC(), nil
}

// resolveOpenAICodexTicketIssuedAt 返回可信的签发时间。解析失败、版本字节不符或
// 时间戳落在合理窗口之外时回退到 now：宁可按本地时刻保守计时，也不要因为一个畸形
// 头部把票判成永不过期或一出生就过期。
func resolveOpenAICodexTicketIssuedAt(state string, now time.Time, ttl time.Duration, accountID int64, model string) time.Time {
	version, issuedAt, err := decodeOpenAICodexTicketIssuedAt(state)
	if err != nil {
		logger.L().Info("openai_codex_ticket issued_at decode failed",
			zap.Int64("account_id", accountID), zap.String("model", model), zap.Error(err))
		return now
	}
	if version != openAICodexTicketFernetVersion {
		logger.L().Info("openai_codex_ticket unexpected fernet version",
			zap.Int64("account_id", accountID), zap.String("model", model),
			zap.String("version", fmt.Sprintf("0x%02x", version)))
		return now
	}
	// 合理窗口：签发时间不应早于一个 TTL 之前，也不应显著晚于现在（容忍 2 分钟时钟偏差）。
	if issuedAt.Before(now.Add(-ttl)) || issuedAt.After(now.Add(2*time.Minute)) {
		logger.L().Info("openai_codex_ticket issued_at out of window",
			zap.Int64("account_id", accountID), zap.String("model", model),
			zap.Time("issued_at", issuedAt), zap.Duration("skew", now.Sub(issuedAt)))
		return now
	}
	return issuedAt
}

func openAICodexTicketKey(accountID int64, model string) string {
	return fmt.Sprintf("%d\x00%s", accountID, strings.TrimSpace(model))
}

func openAICodexTicketExtraKey(model string) string {
	return openAICodexTicketExtraKeyPrefix + strings.TrimSpace(model)
}

func normalizeOpenAICodexTicketModel(model string) string {
	return strings.TrimSpace(model)
}

func extractOpenAICodexTicketModel(body []byte) string {
	return normalizeOpenAICodexTicketModel(gjson.GetBytes(body, "model").String())
}

func (s *OpenAIGatewayService) openAICodexTicketConfig() config.OpenAICodexTicketConfig {
	cfg := config.OpenAICodexTicketConfig{}
	if s != nil && s.cfg != nil {
		cfg = s.cfg.Gateway.OpenAICodexTicket
	}
	if cfg.TargetLength <= 0 {
		cfg.TargetLength = 292
	}
	if cfg.TTLSeconds <= 0 {
		cfg.TTLSeconds = 3600
	}
	if cfg.RefreshBeforeSeconds <= 0 {
		cfg.RefreshBeforeSeconds = 600
	}
	if cfg.HarvestProbeIntervalSeconds <= 0 {
		cfg.HarvestProbeIntervalSeconds = 6
	}
	if cfg.HarvestAttemptTimeoutSeconds <= 0 {
		cfg.HarvestAttemptTimeoutSeconds = 25
	}
	if cfg.HarvestConcurrency <= 0 {
		cfg.HarvestConcurrency = defaultOpenAICodexTicketHarvestConcurrency
	}
	if cfg.HarvestBackoffBaseSeconds <= 0 {
		cfg.HarvestBackoffBaseSeconds = defaultOpenAICodexTicketBackoffBaseSeconds
	}
	if cfg.HarvestBackoffMaxSeconds <= 0 {
		cfg.HarvestBackoffMaxSeconds = defaultOpenAICodexTicketBackoffMaxSeconds
	}
	if cfg.HarvestBackoffMaxSeconds < cfg.HarvestBackoffBaseSeconds {
		cfg.HarvestBackoffMaxSeconds = cfg.HarvestBackoffBaseSeconds
	}
	if len(cfg.Models) == 0 {
		cfg.Models = []string{openAICodexTicketDefaultModel, openAICodexTicketDefaultSolModel}
	}
	return cfg
}

func (s *OpenAIGatewayService) openAICodexTicketGatedModel(model string) bool {
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" || !s.openAICodexTicketEnabled() {
		return false
	}
	for _, item := range s.openAICodexTicketConfig().Models {
		if normalizeOpenAICodexTicketModel(item) == model {
			return true
		}
	}
	return false
}

// OpenAICodexTicketStatus 是给管理端看的门票摘要，不含 state blob。
type OpenAICodexTicketStatus struct {
	Model            string     `json:"model"`
	Length           int        `json:"length,omitempty"`
	Ready            bool       `json:"ready"`
	RemainingSeconds int64      `json:"remaining_seconds"`
	Blocked          bool       `json:"blocked"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
}

func OpenAICodexTicketStatuses(account *Account, cfg config.OpenAICodexTicketConfig, now time.Time) []OpenAICodexTicketStatus {
	if !cfg.Enabled || !isOpenAICodexTicketAccount(account) {
		return nil
	}
	models, targetLen := cfg.Models, cfg.TargetLength
	if len(models) == 0 {
		models = []string{openAICodexTicketDefaultModel, openAICodexTicketDefaultSolModel}
	}
	if targetLen <= 0 {
		targetLen = 292
	}
	out := make([]OpenAICodexTicketStatus, 0, len(models))
	for _, model := range models {
		model = normalizeOpenAICodexTicketModel(model)
		if model == "" {
			continue
		}
		status := OpenAICodexTicketStatus{Model: model}
		ticket := parseOpenAICodexTicketFromAny(0, model, nil)
		if account != nil && account.Extra != nil {
			ticket = parseOpenAICodexTicketFromAny(account.ID, model, account.Extra[openAICodexTicketExtraKey(model)])
		}
		if ticket.valid(now, targetLen) {
			status.Ready = true
			status.Length = ticket.Length
			remaining := int64(ticket.ExpiresAt.Sub(now) / time.Second)
			if remaining < 0 {
				remaining = 0
			}
			status.RemainingSeconds = remaining
			exp := ticket.ExpiresAt
			status.ExpiresAt = &exp
		}
		status.Blocked = cfg.FailClosed && !status.Ready
		out = append(out, status)
	}
	return out
}

func (s *OpenAIGatewayService) openAICodexTicketEnabled() bool {
	return s.openAICodexTicketEnabledContext(context.Background())
}

func (s *OpenAIGatewayService) openAICodexTicketEnabledContext(ctx context.Context) bool {
	if s == nil {
		return false
	}
	fallback := s.cfg != nil && s.cfg.Gateway.OpenAICodexTicket.Enabled
	if s.settingService != nil {
		return s.settingService.GetOpenAICodexTicketEnabled(ctx, fallback)
	}
	return fallback
}

func (s *OpenAIGatewayService) openAICodexTicketHarvestProxyURL() string {
	return s.openAICodexTicketHarvestProxyURLContext(context.Background())
}

func (s *OpenAIGatewayService) openAICodexTicketHarvestProxyURLContext(ctx context.Context) string {
	if s.settingService != nil {
		if proxy := s.settingService.GetOpenAICodexTicketHarvestProxyURL(ctx); proxy != "" {
			return proxy
		}
	}
	return strings.TrimSpace(s.openAICodexTicketConfig().HarvestProxyURL)
}

func (t *openAICodexTicket) valid(now time.Time, targetLen int) bool {
	if t == nil {
		return false
	}
	state := strings.TrimSpace(t.State)
	if len(state) != targetLen || t.Length != targetLen || !strings.HasPrefix(state, openAICodexTicketStatePrefix) {
		return false
	}
	if t.ExpiresAt.IsZero() || !now.Before(t.ExpiresAt) {
		return false
	}
	return true
}

func (t *openAICodexTicket) needsRefresh(now time.Time, refreshBefore time.Duration) bool {
	if t == nil || t.ExpiresAt.IsZero() {
		return true
	}
	return !t.ExpiresAt.After(now.Add(refreshBefore))
}

func (s *OpenAIGatewayService) lookupOpenAICodexTicket(account *Account, model string) *openAICodexTicket {
	if s == nil || account == nil || account.ID <= 0 {
		return nil
	}
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" {
		return nil
	}
	key := openAICodexTicketKey(account.ID, model)
	targetLen := 292
	if s != nil {
		targetLen = s.openAICodexTicketConfig().TargetLength
	}
	now := time.Now()
	var mem *openAICodexTicket
	if raw, ok := s.openaiCodexTickets.Load(key); ok {
		mem, _ = raw.(*openAICodexTicket)
	}
	var extra *openAICodexTicket
	if account.Extra != nil {
		extra = parseOpenAICodexTicketFromAny(account.ID, model, account.Extra[openAICodexTicketExtraKey(model)])
	}
	if extra.valid(now, targetLen) && (mem == nil || extra.CapturedAt.After(mem.CapturedAt)) {
		s.openaiCodexTickets.Store(key, extra)
		return extra
	}
	if mem.valid(now, targetLen) {
		return mem
	}
	if extra != nil {
		s.openaiCodexTickets.Store(key, extra)
		return extra
	}
	if mem != nil {
		s.openaiCodexTickets.Delete(key)
	}
	return nil
}

func parseOpenAICodexTicketFromAny(accountID int64, model string, raw any) *openAICodexTicket {
	if raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var ticket openAICodexTicket
	if err := json.Unmarshal(b, &ticket); err != nil {
		return nil
	}
	ticket.AccountID = accountID
	if strings.TrimSpace(model) != "" {
		ticket.Model = model
	}
	ticket.State = strings.TrimSpace(ticket.State)
	if ticket.Length == 0 {
		ticket.Length = len(ticket.State)
	}
	if ticket.State == "" {
		return nil
	}
	return &ticket
}

func (s *OpenAIGatewayService) storeOpenAICodexTicket(ctx context.Context, account *Account, ticket *openAICodexTicket) {
	if s == nil || account == nil || ticket == nil || account.ID <= 0 {
		return
	}
	model := normalizeOpenAICodexTicketModel(ticket.Model)
	ticket.Model = model
	ticket.AccountID = account.ID
	s.openaiCodexTickets.Store(openAICodexTicketKey(account.ID, model), ticket)
	if s.accountRepo == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		openAICodexTicketExtraKey(model): ticket,
	}); err != nil {
		logger.L().Warn("openai_codex_ticket persist failed",
			zap.Int64("account_id", account.ID),
			zap.String("model", model),
			zap.Error(err),
		)
	}
}

// applyOpenAICodexTicket 在出站请求上覆盖 x-codex-turn-state。
// 请求路径只注入已捕获的有效门票，不现场打票；无票则返回
// ErrOpenAICodexTicketUnavailable。打票由后台 harvester 完成。
func (s *OpenAIGatewayService) applyOpenAICodexTicket(ctx context.Context, account *Account, model string, h http.Header) error {
	if s == nil || h == nil || !isOpenAICodexTicketAccount(account) || !s.openAICodexTicketEnabledContext(ctx) {
		return nil
	}
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" || !s.openAICodexTicketGatedModel(model) {
		return nil
	}
	cfg := s.openAICodexTicketConfig()
	ticket := s.lookupOpenAICodexTicket(account, model)
	if ticket.valid(time.Now(), cfg.TargetLength) {
		h.Set(openAICodexTurnStateHeader, ticket.State)
		return nil
	}
	if !cfg.FailClosed {
		return nil
	}
	return ErrOpenAICodexTicketUnavailable
}

// openAICodexTicketOutboundModel 预测本请求真正出站的模型名，也就是
// applyOpenAICodexTicket 注入时读到的 body.model。
//
// 调度门控与注入必须按同一个模型名判定门票。普通请求下二者同源：Forward 的
// upstreamModel 与本函数都走 resolveOpenAIAccountUpstreamModelForRequest，且
// Forward 会把 body.model 改写成该值后才注入。但 /responses/compact 例外——
// Forward 会把出站模型进一步改写为 compact 映射或 gateway.openai_compact_model
// （默认非空），此时若门控仍按客户端原始模型判定，就会把「实际出站是非门控
// 模型、根本不需要票」的 compact 请求整片误拦成不可调度。
func (s *OpenAIGatewayService) openAICodexTicketOutboundModel(account *Account, requestedModel string, requireCompact bool) string {
	model := strings.TrimSpace(requestedModel)
	if account == nil || model == "" {
		return model
	}
	if !account.IsOpenAI() {
		return canonicalOpenAIAccountSchedulingModel(account, model)
	}
	_, upstreamModel := resolveOpenAIForwardMappedModels(account, model, requireCompact)
	if requireCompact {
		// 与 Forward 同序：compact 兜底模型优先于普通/compact 映射结果。
		if compactModel := strings.TrimSpace(s.resolveOpenAICompactFallbackModel(account, model)); compactModel != "" {
			upstreamModel = compactModel
		}
	}
	if upstreamModel = strings.TrimSpace(upstreamModel); upstreamModel != "" {
		return upstreamModel
	}
	return model
}

// outboundModel 必须是真正会发给上游的模型名（openAICodexTicketOutboundModel），
// 不是客户端原始模型：注入侧读的是出站 body.model，两侧口径必须一致。
func (s *OpenAIGatewayService) openAICodexTicketBlocksAccount(account *Account, outboundModel string) bool {
	if s == nil || !isOpenAICodexTicketAccount(account) || !s.openAICodexTicketEnabled() {
		return false
	}
	cfg := s.openAICodexTicketConfig()
	if !cfg.FailClosed {
		return false
	}
	model := normalizeOpenAICodexTicketModel(outboundModel)
	if !s.openAICodexTicketGatedModel(model) {
		return false
	}
	ticket := s.lookupOpenAICodexTicket(account, model)
	return !ticket.valid(time.Now(), cfg.TargetLength)
}

func (s *OpenAIGatewayService) fireOpenAICodexTicketProbe(ctx context.Context, account *Account, token, model, proxyURL string, attemptTimeout time.Duration) (state string, status int, err error) {
	attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
	defer cancel()

	body := []byte(`{"model":` + jsonString(model) + `,"store":false,"stream":true,"instructions":"Reply with exactly: pong","input":[{"role":"user","content":[{"type":"input_text","text":"ping"}]}]}`)
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, chatgptCodexURL, bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAIHarvest))
	req.Close = true
	req.Host = "chatgpt.com"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("session_id", uuid.NewString())
	if err := resolveAndSetOpenAIChatGPTAccountHeaders(attemptCtx, s.accountRepo, req.Header, account); err != nil {
		return "", 0, err
	}
	applyOpenAICodexTicketHarvestIdentity(req.Header, model)

	// Synthetic probes must use the dedicated no-reuse transport even when the
	// production account is bound to a plugin. This also avoids reading pluginManager
	// while handlers are still wiring it during gateway construction.
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return "", 0, err
	}
	if resp == nil {
		return "", 0, errors.New("nil upstream response")
	}
	// Only the response header is needed; no connection will be reused.
	defer func() {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	return extractOpenAICodexTurnState(resp.Header), resp.StatusCode, nil
}

func jsonString(v string) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `""`
	}
	return string(b)
}

func applyOpenAICodexTicketHarvestIdentity(h http.Header, model string) {
	ensureCodexIdentityHeaders(h)
	enforceCodexIdentityHeaders(h)
	version := strings.TrimSpace(h.Get("version"))
	if needsOpenAICodexAstraVersion(model) && (version == "" || CompareVersions(version, openAICodexAstraMinVersion) < 0) {
		h.Set("version", openAICodexAstraMinVersion)
		h.Set("user-agent", buildCodexCLIUserAgent(openAICodexAstraMinVersion))
		h.Set("originator", openai.CodexDefaultOriginator)
	}
}

func needsOpenAICodexAstraVersion(model string) bool {
	m := strings.ToLower(normalizeOpenAICodexTicketModel(model))
	return strings.Contains(m, "gpt-6") || strings.Contains(m, "astra")
}

func (s *OpenAIGatewayService) StartOpenAICodexTicketHarvester() {
	if s == nil {
		return
	}
	s.openaiCodexTicketLifecycleMu.Lock()
	defer s.openaiCodexTicketLifecycleMu.Unlock()
	if s.openaiCodexTicketStopped || s.openaiCodexTicketDone != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.openaiCodexTicketCancel = cancel
	s.openaiCodexTicketDone = done
	go func() {
		defer close(done)
		s.openAICodexTicketHarvestLoop(ctx)
	}()
	logger.L().Info("openai_codex_ticket harvester started",
		zap.Int("ttl_seconds", s.openAICodexTicketConfig().TTLSeconds),
		zap.Int("target_length", s.openAICodexTicketConfig().TargetLength),
		zap.Strings("models", s.openAICodexTicketConfig().Models),
	)
}

func (s *OpenAIGatewayService) StopOpenAICodexTicketHarvester() {
	if s == nil {
		return
	}
	s.openaiCodexTicketLifecycleMu.Lock()
	s.openaiCodexTicketStopped = true
	cancel, done := s.openaiCodexTicketCancel, s.openaiCodexTicketDone
	s.openaiCodexTicketLifecycleMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (s *OpenAIGatewayService) openAICodexTicketHarvestLoop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.refreshOpenAICodexTickets(ctx)
			timer.Reset(time.Duration(s.openAICodexTicketConfig().HarvestProbeIntervalSeconds) * time.Second)
		}
	}
}

// refreshOpenAICodexTickets probes each account/model with a missing or soon-to-expire
// ticket once. The loop waits for all probes, then waits the configured interval
// before starting the next cycle.
func (s *OpenAIGatewayService) refreshOpenAICodexTickets(ctx context.Context) {
	if s == nil || s.accountRepo == nil || ctx.Err() != nil || !s.openAICodexTicketEnabledContext(ctx) {
		return
	}
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		logger.L().Warn("openai_codex_ticket list accounts failed", zap.Error(err))
		return
	}
	cfg := s.openAICodexTicketConfig()
	now := time.Now()
	refreshBefore := time.Duration(cfg.RefreshBeforeSeconds) * time.Second
	var wg sync.WaitGroup
	// sem 给本轮探测封顶。上游原实现按 (账号, 模型) 无限起 goroutine，几千账号
	// 会把单个采集代理瞬间打满，且每发都可能占满 attempt_timeout。
	sem := make(chan struct{}, cfg.HarvestConcurrency)
	probed := 0
	skipped := 0
	for i := range accounts {
		account := accounts[i]
		if account.Status != StatusActive || !isOpenAICodexTicketAccount(&account) {
			continue
		}
		for _, model := range cfg.Models {
			model := normalizeOpenAICodexTicketModel(model)
			if model == "" {
				continue
			}
			// 已有一张有效且未临近过期的票 → 本周期不打，省得白刷。
			if t := s.lookupOpenAICodexTicket(&account, model); t.valid(now, cfg.TargetLength) && !t.needsRefresh(now, refreshBefore) {
				continue
			}
			// 连续失败的 (账号, 模型) 在退避窗口内跳过：拿不到 292 往往是出口 IP
			// 当前不在干净窗口期，按 probe_interval 死磕只会加速烧号烧 IP。
			if !s.openAICodexTicketProbeAllowed(openAICodexTicketKey(account.ID, model), now) {
				skipped++
				continue
			}
			acc := account
			// Token/header helpers may update account metadata; each model owns its maps.
			acc.Extra = maps.Clone(account.Extra)
			acc.Credentials = maps.Clone(account.Credentials)
			probed++
			wg.Add(1)
			go func(acc Account, model string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				if ctx.Err() != nil {
					return
				}
				s.probeOnceOpenAICodexTicket(ctx, &acc, model)
			}(acc, model)
		}
	}
	wg.Wait()
	if probed > 0 || skipped > 0 {
		logger.L().Info("openai_codex_ticket probe cycle",
			zap.Int("probed", probed), zap.Int("backoff_skipped", skipped),
			zap.Int("concurrency", cfg.HarvestConcurrency))
	}
}

// probeOnceOpenAICodexTicket 走打票代理打一发。命中合格 292（HTTP 200、长度==target、
// gAAAAA 前缀）就落库；否则记 Info miss，交给下个周期重试。同一 key 并发去重，避免上一发还没
// 回来又叠一发。
func (s *OpenAIGatewayService) probeOnceOpenAICodexTicket(ctx context.Context, account *Account, model string) {
	if s == nil || !isOpenAICodexTicketAccount(account) || ctx.Err() != nil || !s.openAICodexTicketEnabledContext(ctx) {
		return
	}
	cfg := s.openAICodexTicketConfig()
	proxyURL := s.openAICodexTicketHarvestProxyURLContext(ctx)
	if proxyURL == "" || s.httpUpstream == nil || ctx.Err() != nil {
		return
	}
	key := openAICodexTicketKey(account.ID, model)
	_, _, _ = s.openaiCodexTicketFlight.Do(key, func() (any, error) {
		token, _, err := s.GetAccessToken(ctx, account)
		if err != nil || strings.TrimSpace(token) == "" {
			s.noteOpenAICodexTicketProbeFailure(key, cfg, time.Now())
			logger.L().Info("openai_codex_ticket probe miss",
				zap.Int64("account_id", account.ID), zap.String("model", model),
				zap.String("reason", "token"), zap.Error(err))
			return nil, nil
		}
		state, status, perr := s.fireOpenAICodexTicketProbe(ctx, account, token, model, proxyURL, time.Duration(cfg.HarvestAttemptTimeoutSeconds)*time.Second)
		if perr != nil {
			s.noteOpenAICodexTicketProbeFailure(key, cfg, time.Now())
			logger.L().Info("openai_codex_ticket probe miss",
				zap.Int64("account_id", account.ID), zap.String("model", model),
				zap.String("reason", "error"), zap.Error(perr))
			return nil, nil
		}
		if status != http.StatusOK || state == "" || len(state) != cfg.TargetLength || !strings.HasPrefix(state, openAICodexTicketStatePrefix) {
			s.noteOpenAICodexTicketProbeFailure(key, cfg, time.Now())
			// len 是关键诊断位：312 说明当前出口 IP 已被降智，不是网络问题。
			logger.L().Info("openai_codex_ticket probe miss",
				zap.Int64("account_id", account.ID), zap.String("model", model),
				zap.Int("http", status), zap.Int("len", len(state)))
			return nil, nil
		}
		now := time.Now()
		ttl := time.Duration(cfg.TTLSeconds) * time.Second
		// 有效期锚定在票据自述的签发时间上，而不是本地捕获时刻：上游签发即开始计时，
		// 按捕获时刻起算会把探测往返与排队延迟白送给我们，导致票据实际已死后仍被注入
		// （表现为上游的 "Encrypted content could not be ..."）。
		issuedAt := resolveOpenAICodexTicketIssuedAt(state, now, ttl, account.ID, model)
		ticket := &openAICodexTicket{
			AccountID:  account.ID,
			Model:      model,
			State:      state,
			Length:     len(state),
			CapturedAt: now,
			IssuedAt:   issuedAt,
			ExpiresAt:  issuedAt.Add(ttl),
			Attempts:   1,
		}
		s.storeOpenAICodexTicket(ctx, account, ticket)
		s.clearOpenAICodexTicketBackoff(key)
		logger.L().Info("openai_codex_ticket harvested",
			zap.Int64("account_id", account.ID), zap.String("model", model),
			zap.Int("length", ticket.Length), zap.String("mode", "continuous"),
			zap.Time("issued_at", issuedAt), zap.Duration("capture_lag", now.Sub(issuedAt)))
		return nil, nil
	})
}

// openAICodexTicketBackoff 记录某个 (账号, 模型) 的连续失败与下次允许探测时刻。
type openAICodexTicketBackoff struct {
	failures    int
	nextAttempt time.Time
}

// openAICodexTicketProbeAllowed 判断退避窗口是否已过。
func (s *OpenAIGatewayService) openAICodexTicketProbeAllowed(key string, now time.Time) bool {
	if s == nil {
		return false
	}
	raw, ok := s.openaiCodexTicketBackoff.Load(key)
	if !ok {
		return true
	}
	state, ok := raw.(*openAICodexTicketBackoff)
	if !ok || state == nil {
		return true
	}
	return !now.Before(state.nextAttempt)
}

// noteOpenAICodexTicketProbeFailure 累计失败并按 base*2^(n-1) 计算下次探测时刻，
// 加正向抖动避免数千账号在同一时刻齐步重试。
func (s *OpenAIGatewayService) noteOpenAICodexTicketProbeFailure(key string, cfg config.OpenAICodexTicketConfig, now time.Time) {
	if s == nil {
		return
	}
	failures := 1
	if raw, ok := s.openaiCodexTicketBackoff.Load(key); ok {
		if state, ok := raw.(*openAICodexTicketBackoff); ok && state != nil {
			failures = state.failures + 1
		}
	}
	base := time.Duration(cfg.HarvestBackoffBaseSeconds) * time.Second
	maxDelay := time.Duration(cfg.HarvestBackoffMaxSeconds) * time.Second
	delay := base
	for i := 1; i < failures && delay < maxDelay; i++ {
		delay *= 2
	}
	if delay > maxDelay {
		delay = maxDelay
	}
	if jitter := delay / 5; jitter > 0 {
		delay += time.Duration(rand.Int64N(int64(jitter)))
	}
	s.openaiCodexTicketBackoff.Store(key, &openAICodexTicketBackoff{
		failures:    failures,
		nextAttempt: now.Add(delay),
	})
}

// clearOpenAICodexTicketBackoff 打票成功后清空退避状态。
func (s *OpenAIGatewayService) clearOpenAICodexTicketBackoff(key string) {
	if s == nil {
		return
	}
	s.openaiCodexTicketBackoff.Delete(key)
}

// IsOpenAICodexTicketExtraKey identifies server-managed ticket material.
func IsOpenAICodexTicketExtraKey(key string) bool {
	return strings.HasPrefix(key, openAICodexTicketExtraKeyPrefix)
}

// MergeOpenAICodexTicketExtra preserves only persisted tickets, never summaries or
// blobs supplied by an account edit. The repository repeats this under the row
// lock so a concurrent harvest cannot be overwritten by a stale admin snapshot.
func MergeOpenAICodexTicketExtra(extra, current map[string]any) map[string]any {
	result := maps.Clone(extra)
	for key := range result {
		if IsOpenAICodexTicketExtraKey(key) {
			delete(result, key)
		}
	}
	for key, value := range current {
		if IsOpenAICodexTicketExtraKey(key) {
			if result == nil {
				result = make(map[string]any)
			}
			result[key] = value
		}
	}
	return result
}

// ValidateOpenAICodexTicketHarvestProxyURL validates only syntax, without making
// a network request or including credentials in validation errors.
func ValidateOpenAICodexTicketHarvestProxyURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return errors.New("harvest proxy must be an HTTP(S) or SOCKS5(h) URL with a host and no path, query or fragment")
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return errors.New("harvest proxy scheme must be http, https, socks5 or socks5h")
	}
	if port := parsed.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("harvest proxy port must be between 1 and 65535")
		}
	}
	return nil
}

// MaskProxyURL never returns a stored proxy password, even for invalid legacy data.
func MaskProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || ValidateOpenAICodexTicketHarvestProxyURL(raw) != nil {
		return ""
	}
	parsed, _ := url.Parse(raw)
	if parsed.User != nil {
		if _, ok := parsed.User.Password(); ok {
			parsed.User = url.UserPassword(parsed.User.Username(), "***")
		}
	}
	return parsed.String()
}

// IsMaskedProxyURL recognizes the exact password placeholder emitted by the API.
func IsMaskedProxyURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return false
	}
	password, ok := parsed.User.Password()
	return ok && password == "***"
}

// Credential shadows do not own tickets. Keep their existing forwarding policy
// instead of imposing a gate for a key the harvester never populates.
func isOpenAICodexTicketAccount(account *Account) bool {
	return account != nil && account.IsOpenAIOAuthLike() && !account.IsShadow()
}

// IsOpenAICodexTicketPrivateExtraKey also covers the retired account-level proxy
// override, whose credentials may remain in older account records.
func IsOpenAICodexTicketPrivateExtraKey(key string) bool {
	return IsOpenAICodexTicketExtraKey(key) || key == "codex_harvest_proxy_url"
}

// RedactOpenAICodexTicketExtra strips ephemeral ticket material from exports
// without changing the source account or unrelated backup fields.
func RedactOpenAICodexTicketExtra(extra map[string]any) map[string]any {
	redacted := maps.Clone(extra)
	for key := range redacted {
		if IsOpenAICodexTicketPrivateExtraKey(key) {
			delete(redacted, key)
		}
	}
	return redacted
}
