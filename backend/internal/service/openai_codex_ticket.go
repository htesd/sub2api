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

	"github.com/gin-gonic/gin"

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
	// 只给近 24 小时跑过流量的账号打票；设为负数可关闭该过滤。
	defaultOpenAICodexTicketActiveWindowSeconds = 86400
	// 出口 IP 返回非合格长度后冷却 10 分钟再参与轮换。
	defaultOpenAICodexTicketProxyBurnCooldownSeconds = 600

	// turn-state 的长度由套餐决定，不是全局常量。实测（2026-09-18，111 个活跃
	// OAuth 账号）：
	//   pro      → 292（10 个 AES 块）
	//   business → 332（12 个 AES 块，Team/Business Premium）
	// 上游把 target_length 写死成 292，导致 Business 账号永远拿不到票——它们
	// 返回 200 且 state 结构合法，只是长度天然是 332。
	openAICodexTicketLengthPro      = 292
	openAICodexTicketLengthBusiness = 332

	// openAICodexTicketPlanExtraKey 账号级套餐覆盖（pro / team）。留空时按
	// credentials.plan_type 推断。
	openAICodexTicketPlanExtraKey    = "codex_ticket_plan"
	openAICodexTicketDefaultModel    = "gpt-6-astra"
	openAICodexTicketDefaultSolModel = "gpt-5.6-sol"

	// openAICodexTicketEnabledExtraKey 账号级总开关。缺省=继承全局；
	// false=该号完全不参与（既不采集也不注入）。
	openAICodexTicketEnabledExtraKey = "codex_ticket_enabled"
	// openAICodexTicketActiveProbeExtraKey 账号级主动探测开关，默认 false。
	// 主动探测会用该号凭据从与业务出口不同的 IP 发合成请求，风险显著高于被动
	// 采集，因此必须逐号显式 opt-in。
	openAICodexTicketActiveProbeExtraKey = "codex_ticket_active_probe"
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

// openAICodexTicketTargetLength 返回该账号的合格 turn-state 长度。
//
// 优先账号级 extra 覆盖（codex_ticket_plan = pro|team），其次按 credentials
// 的 plan_type 推断，最后回落到配置里的 target_length。
//
// 注意：长度只是筛选器，不能证明模型身份或回答质量。真正的降智判据是上游实际
// 响应模型（usage_logs.upstream_response_model），长度只用来挑出"这一档套餐里
// 结构最短的那个 state"。
func openAICodexTicketTargetLength(account *Account, cfg config.OpenAICodexTicketConfig) int {
	fallback := cfg.TargetLength
	if fallback <= 0 {
		fallback = openAICodexTicketLengthPro
	}
	if account == nil {
		return fallback
	}
	if account.Extra != nil {
		if raw, ok := account.Extra[openAICodexTicketPlanExtraKey].(string); ok {
			switch strings.ToLower(strings.TrimSpace(raw)) {
			case "pro":
				return openAICodexTicketLengthPro
			case "team", "business":
				return openAICodexTicketLengthBusiness
			}
		}
	}
	switch normalizeOpenAICodexTicketPlan(account) {
	case "pro":
		return openAICodexTicketLengthPro
	case "business":
		return openAICodexTicketLengthBusiness
	}
	return fallback
}

// normalizeOpenAICodexTicketPlan 把 credentials.plan_type 归一到 pro / business /
// unknown。实测取值：pro、self_serve_business_prolite、
// self_serve_business_usage_based、free。
func normalizeOpenAICodexTicketPlan(account *Account) string {
	if account == nil || account.Credentials == nil {
		return "unknown"
	}
	raw, _ := account.Credentials["plan_type"].(string)
	plan := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case plan == "":
		return "unknown"
	case plan == "pro":
		return "pro"
	case strings.Contains(plan, "business"), strings.Contains(plan, "team"):
		return "business"
	case plan == "free":
		return "free"
	}
	return "unknown"
}

// openAICodexTicketAccountEnabled 账号级总开关：缺省继承全局，显式 false 则该号
// 完全不参与票据机制——既不被动采集，也不注入。
//
// 关掉它才是真正的隔离。只关主动探测是不够的：被动采集本身不新增任何上游请求，
// 但**注入**存在重放与上下文不匹配的风险面（同一张票跨会话/并发回合复用，且其自述
// 签发时间早于当前请求），而注入的票可以来自被动采集。
//
// 注意：注入与封号的因果关系尚未验证——回合凭证本来就可能被正常复用，单凭时间戳
// 较早不足以断言异常。这里的取舍是"无法证伪就提供关掉的能力"。
func openAICodexTicketAccountEnabled(account *Account) bool {
	if account == nil {
		return false
	}
	if account.Extra == nil {
		return true
	}
	switch v := account.Extra[openAICodexTicketEnabledExtraKey].(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "false", "0", "off", "no":
			return false
		case "true", "1", "on", "yes":
			return true
		}
	}
	return true
}

// openAICodexTicketAccountActiveProbeEnabled 账号级主动探测开关，默认关闭。
// 需要同时满足全局 harvest_loop_enabled 才会真正被探测。
func openAICodexTicketAccountActiveProbeEnabled(account *Account) bool {
	if account == nil || account.Extra == nil {
		return false
	}
	switch v := account.Extra[openAICodexTicketActiveProbeExtraKey].(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1", "on", "yes":
			return true
		}
	}
	return false
}

// openAICodexTicketPlanKnown 判断该账号的合格长度是否有确定依据。
// 必须与 openAICodexTicketTargetLength 同口径：账号级 extra.codex_ticket_plan
// 覆盖也算"已知"，否则一个显式标了 team 的账号会被当成未知套餐，长度判定用 332
// 而烧号判定却跳过，两套口径撕裂。
func openAICodexTicketPlanKnown(account *Account) bool {
	if account == nil {
		return false
	}
	if account.Extra != nil {
		if raw, ok := account.Extra[openAICodexTicketPlanExtraKey].(string); ok {
			switch strings.ToLower(strings.TrimSpace(raw)) {
			case "pro", "team", "business":
				return true
			}
		}
	}
	switch normalizeOpenAICodexTicketPlan(account) {
	case "pro", "business":
		return true
	}
	return false
}

// openAICodexTicketPlanEligible 判断该套餐是否可能拿到票。free 账号实测一律被
// 上游 400 拒绝（无 astra/sol 访问权限），探测它们只是白送失败信号。
func openAICodexTicketPlanEligible(account *Account) bool {
	return normalizeOpenAICodexTicketPlan(account) != "free"
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
	return NormalizeOpenAICodexTicketConfig(cfg)
}

// NormalizeOpenAICodexTicketConfig 补齐默认值。
//
// 必须导出并在所有消费点共用：运行时与管理端展示若各自拿原始配置，
// 归一化差异会直接变成"运行时放行但 UI 显示已暂停"这类矛盾。典型的坑是
// harvest_active_window_seconds——原始值 0 会被 openAICodexTicketAccountRecentlyUsed
// 当成"不限制"，而运行时其实把它归一成了 86400。
func NormalizeOpenAICodexTicketConfig(cfg config.OpenAICodexTicketConfig) config.OpenAICodexTicketConfig {
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
	if cfg.HarvestActiveWindowSeconds == 0 {
		cfg.HarvestActiveWindowSeconds = defaultOpenAICodexTicketActiveWindowSeconds
	}
	if cfg.HarvestProxyBurnCooldownSeconds <= 0 {
		cfg.HarvestProxyBurnCooldownSeconds = defaultOpenAICodexTicketProxyBurnCooldownSeconds
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
	// AccountDisabled 表示该账号已通过 codex_ticket_enabled 退出票据机制：
	// 库存里的票不会被注入。与 Blocked 是两回事——Blocked 是"没票且拒发"，
	// 这个是"有票也不用"。
	AccountDisabled bool `json:"account_disabled,omitempty"`
}

func OpenAICodexTicketStatuses(account *Account, cfg config.OpenAICodexTicketConfig, now time.Time) []OpenAICodexTicketStatus {
	if !cfg.Enabled || !isOpenAICodexTicketAccount(account) {
		return nil
	}
	// 与运行时同一套归一化，否则默认配置（未填 harvest_active_window_seconds）下
	// 展示层会把 0 当成"不限制"，而运行时按 86400 判定，两边对 idle 账号给出
	// 相反的结论。
	cfg = NormalizeOpenAICodexTicketConfig(cfg)
	models, targetLen := cfg.Models, openAICodexTicketTargetLength(account, cfg)
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
		// 与运行时同源：被动采集模式、无票源、idle、free 套餐等情况下运行时会
		// 放行，管理端不能还显示"已暂停"。proxyConfigured 用配置值近似——纯函数
		// 拿不到后台设置，宁可显示未阻断（与运行时的放行方向一致）。
		// 账号级关闭时票据不会被注入，Ready 只代表"库存里有一张结构有效的票"。
		// 不把它清成 false（库存事实要如实展示），但显式标出不可用，避免管理员
		// 误以为该号正在使用票据。
		if !openAICodexTicketAccountEnabled(account) {
			status.AccountDisabled = true
		}
		status.Blocked = !status.Ready && openAICodexTicketFailClosedEffective(
			account, cfg, len(SplitOpenAICodexTicketHarvestProxyPool(cfg.HarvestProxyURL)) > 0, now)
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
	// 按账号套餐取合格长度。用 pro 的 292 去校验 business 的 332 票会把有效票
	// 判为无效，在 mem-only 场景（UpdateExtra 失败）下还会顺手把它删掉。
	targetLen := openAICodexTicketTargetLength(account, s.openAICodexTicketConfig())
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
	// 账号级关闭 = 不注入。这是保护特定账号（如易封的 pro 号）的唯一有效开关。
	if !openAICodexTicketAccountEnabled(account) {
		return nil
	}
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" || !s.openAICodexTicketGatedModel(model) {
		return nil
	}
	cfg := s.openAICodexTicketConfig()
	ticket := s.lookupOpenAICodexTicket(account, model)
	if ticket.valid(time.Now(), openAICodexTicketTargetLength(account, cfg)) {
		h.Set(openAICodexTurnStateHeader, ticket.State)
		return nil
	}
	if !s.openAICodexTicketFailClosedActive(account, cfg) {
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

// openAICodexTicketFailClosedEffective 判断 fail_closed 对该账号是否真的该生效。
//
// fail_closed 的含义是"没有合格票就拒发，宁可失败也不要降智"。它唯一的成立前提是
// **这个账号确实会被一个可用的票源探测到**。任何时候前提不成立，fail_closed 就退化
// 成自举死锁：
//
//	没有票 → 拒发请求 → 没有成功回合 → last_used_at 不更新 → 永远拿不到票
//
// 死锁有四个入口，全部要挡住：
//  1. 只有被动采集（harvest_loop_enabled=false）：票只能来自成功回合，拒发即死锁
//  2. 主动循环开着但没配采集代理：probeOnce 直接返回，循环永远产不出票
//  3. 账号在活跃窗口外或从未使用：主动循环按 idle 跳过它，而拒发让它永远无法变活跃
//  4. free 套餐：上游一律 400，无论探测多少次都拿不到票
//
// 降智是可恢复的，永久拒发不是。所以这四种情况一律放行。
func openAICodexTicketFailClosedEffective(account *Account, cfg config.OpenAICodexTicketConfig, proxyConfigured bool, now time.Time) bool {
	if !cfg.FailClosed {
		return false
	}
	// 入口 1：该账号的票只能来自被动采集。需要全局主动探测总闸 + 该号显式 opt-in
	// 都成立，它才真的会被探测到。死锁是按账号成立的，所以这里必须按账号判。
	if !cfg.HarvestLoopEnabled || !openAICodexTicketAccountActiveProbeEnabled(account) {
		return false
	}
	// 账号级关闭时不参与票据机制，自然也不该被 fail_closed 拦。
	if !openAICodexTicketAccountEnabled(account) {
		return false
	}
	// 入口 2：没有可用票源。这里只能判"是否配置"，代理运行时不可用无法在此判定，
	// 但至少不能把空代理当成独立来源。
	if !proxyConfigured {
		return false
	}
	// 入口 4：free 套餐永远拿不到票。
	if !openAICodexTicketPlanEligible(account) {
		return false
	}
	// 入口 3：主动循环不会探测它。判定必须与 refreshOpenAICodexTickets 的准入同源。
	if !openAICodexTicketAccountRecentlyUsed(account, cfg, now) {
		return false
	}
	return true
}

// openAICodexTicketFailClosedActive 是 openAICodexTicketFailClosedEffective 的
// 服务侧封装，补上"采集代理是否配置"这一维。
func (s *OpenAIGatewayService) openAICodexTicketFailClosedActive(account *Account, cfg config.OpenAICodexTicketConfig) bool {
	proxyConfigured := len(SplitOpenAICodexTicketHarvestProxyPool(s.openAICodexTicketHarvestProxyURL())) > 0
	return openAICodexTicketFailClosedEffective(account, cfg, proxyConfigured, time.Now())
}

// outboundModel 必须是真正会发给上游的模型名（openAICodexTicketOutboundModel），
// 不是客户端原始模型：注入侧读的是出站 body.model，两侧口径必须一致。
func (s *OpenAIGatewayService) openAICodexTicketBlocksAccount(account *Account, outboundModel string) bool {
	if s == nil || !isOpenAICodexTicketAccount(account) || !s.openAICodexTicketEnabled() {
		return false
	}
	cfg := s.openAICodexTicketConfig()
	if !s.openAICodexTicketFailClosedActive(account, cfg) {
		return false
	}
	model := normalizeOpenAICodexTicketModel(outboundModel)
	if !s.openAICodexTicketGatedModel(model) {
		return false
	}
	ticket := s.lookupOpenAICodexTicket(account, model)
	return !ticket.valid(time.Now(), openAICodexTicketTargetLength(account, cfg))
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
	cfg := s.openAICodexTicketConfig()
	// 后台主动探测默认不启动。主动探测会用账号凭据从一个与业务出口完全不同的
	// IP（通常是机房 IP）发请求，在上游看来同一个账号周期性地跨 ASN 跳变，这本身
	// 就是可识别的关联特征。票据的主路径应该是从真实业务响应里被动捕获（同一出口、
	// 零额外请求），主动探测只作为兜底，且必须显式开启或手动触发。
	if !cfg.HarvestLoopEnabled {
		logger.L().Info("openai_codex_ticket harvester not started",
			zap.String("reason", "harvest_loop_enabled=false"),
			zap.String("note", "被动采集仍生效；主动探测需显式开启或手动触发"))
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
		zap.Int("ttl_seconds", cfg.TTLSeconds),
		zap.Int("default_target_length", cfg.TargetLength),
		zap.Int("active_window_seconds", cfg.HarvestActiveWindowSeconds),
		zap.Int("concurrency", cfg.HarvestConcurrency),
		zap.Strings("models", cfg.Models),
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
	idle := 0
	unschedulable := 0
	ineligible := 0
	optedOut := 0
	for i := range accounts {
		account := accounts[i]
		if account.Status != StatusActive || !isOpenAICodexTicketAccount(&account) {
			continue
		}
		// 尊重调度状态。上游只看 status==active，于是管理员手动"停止调度"的号、
		// 限流中的号、过载中的号、临时不可调度的号统统照打——这些号本来就不该
		// 承接流量，给它们打票既浪费干净出口 IP 的窗口期，又在给一个已经被暂停
		// 的账号持续制造上游请求。
		if !account.IsSchedulable() {
			unschedulable++
			continue
		}
		// free 套餐实测一律被上游 400 拒绝（无 astra/sol 权限），探测纯属白送失败。
		if !openAICodexTicketPlanEligible(&account) {
			ineligible++
			continue
		}
		// 账号级两道开关：总开关关掉即完全隔离；主动探测默认关闭，必须逐号 opt-in。
		// 主动探测用该号凭据从与业务出口不同的 IP 发合成请求，风险远高于被动采集，
		// 所以全局 harvest_loop_enabled 打开也只碰显式勾选的号。
		if !openAICodexTicketAccountEnabled(&account) || !openAICodexTicketAccountActiveProbeEnabled(&account) {
			optedOut++
			continue
		}
		for _, model := range cfg.Models {
			model := normalizeOpenAICodexTicketModel(model)
			if model == "" {
				continue
			}
			// 已有一张有效且未临近过期的票 → 本周期不打，省得白刷。
			if t := s.lookupOpenAICodexTicket(&account, model); t.valid(now, openAICodexTicketTargetLength(&account, cfg)) && !t.needsRefresh(now, refreshBefore) {
				continue
			}
			// 连续失败的 (账号, 模型) 在退避窗口内跳过：拿不到 292 往往是出口 IP
			// 当前不在干净窗口期，按 probe_interval 死磕只会加速烧号烧 IP。
			if !s.openAICodexTicketProbeAllowed(openAICodexTicketKey(account.ID, model), now) {
				skipped++
				continue
			}
			// 只给实际在跑流量的账号打票。票据是稀缺资源（每张都要消耗一发真实请求和
			// 一次干净出口 IP 的窗口期），给几千个闲置号预打票是纯浪费。冷启动代价是
			// 某个久未使用的号被调度到时第一发没有票，但它一旦发过请求 last_used_at
			// 就会更新，下一轮（默认 6 秒内）即进入采集范围。
			if !openAICodexTicketAccountRecentlyUsed(&account, cfg, now) {
				idle++
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
			zap.Int("idle_skipped", idle), zap.Int("unschedulable_skipped", unschedulable),
			zap.Int("plan_ineligible_skipped", ineligible),
			zap.Int("opted_out_skipped", optedOut),
			zap.Int("concurrency", cfg.HarvestConcurrency),
			zap.Int("proxy_pool", len(SplitOpenAICodexTicketHarvestProxyPool(s.openAICodexTicketHarvestProxyURLContext(ctx)))))
	}
}

// probeOnceOpenAICodexTicket 走打票代理打一发。命中合格 292（HTTP 200、长度==target、
// gAAAAA 前缀）就落库；否则记 Info miss，交给下个周期重试。同一 key 并发去重，避免上一发还没
// 回来又叠一发。
func (s *OpenAIGatewayService) probeOnceOpenAICodexTicket(ctx context.Context, account *Account, model string) {
	if s == nil || !isOpenAICodexTicketAccount(account) || ctx.Err() != nil || !s.openAICodexTicketEnabledContext(ctx) {
		return
	}
	// 撤销缺口：账号快照是在排队之前取的，排队期间（信号量等待 + 前面的探测各自最长
	// attempt_timeout）管理员可能已经关掉开关。发请求前必须以最新状态复核一次，否则
	// "关掉开关"会被在飞任务绕过——对一个正在被保护的账号继续发合成请求。
	//
	// 这是对"是否允许发出这发合成请求"的授权检查，取值方向必须 fail-closed：
	// 查不到（账号已删除）或查询出错时我们无法确认授权仍然有效，只能放弃本轮，
	// 等下一个周期重试。这与业务侧的 fail_closed 配置是两件事——那个决定的是
	// "客户端请求要不要失败"，而这里只是"后台要不要多发一发探测"，放弃的代价
	// 极低，误发的代价是对一个可能已被保护的账号动手。
	if account == nil || s.accountRepo == nil {
		return
	}
	latest, err := s.accountRepo.GetByID(ctx, account.ID)
	switch {
	// 账号已删除要单独归类：生产 repo 在找不到时返回 ErrAccountNotFound 而不是
	// (nil, nil)，若只判 latest == nil，真实的删除会被算进"查询失败"，掩盖掉
	// "这个号已经没了"这个确定结论。
	case errors.Is(err, ErrAccountNotFound):
		logger.L().Info("openai_codex_ticket probe cancelled",
			zap.Int64("account_id", account.ID), zap.String("model", model),
			zap.String("reason", "account_gone_while_queued"))
		return
	case err != nil:
		logger.L().Info("openai_codex_ticket probe cancelled",
			zap.Int64("account_id", account.ID), zap.String("model", model),
			zap.String("reason", "authorization_recheck_failed"), zap.Error(err))
		return
	case latest == nil:
		logger.L().Info("openai_codex_ticket probe cancelled",
			zap.Int64("account_id", account.ID), zap.String("model", model),
			zap.String("reason", "account_gone_while_queued"))
		return
	case !openAICodexTicketAccountEnabled(latest) || !openAICodexTicketAccountActiveProbeEnabled(latest):
		logger.L().Info("openai_codex_ticket probe cancelled",
			zap.Int64("account_id", account.ID), zap.String("model", model),
			zap.String("reason", "opted_out_while_queued"))
		return
	case !latest.IsSchedulable():
		logger.L().Info("openai_codex_ticket probe cancelled",
			zap.Int64("account_id", account.ID), zap.String("model", model),
			zap.String("reason", "unschedulable_while_queued"))
		return
	}
	cfg := s.openAICodexTicketConfig()
	// 代理池轮换：同一个出口反复打票会把自己打黑（返回 312），所以每发探测轮转取一个
	// 未在冷却中的出口。
	pool := SplitOpenAICodexTicketHarvestProxyPool(s.openAICodexTicketHarvestProxyURLContext(ctx))
	proxyURL := s.pickOpenAICodexTicketHarvestProxy(pool, time.Now())
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
		wantLen := openAICodexTicketTargetLength(account, cfg)
		if status != http.StatusOK || state == "" || len(state) != wantLen || !strings.HasPrefix(state, openAICodexTicketStatePrefix) {
			now := time.Now()
			s.noteOpenAICodexTicketProbeFailure(key, cfg, now)
			// 长度不符只在"该账号的合格长度是已知的"前提下才能归因到出口 IP。
			// 未知套餐走的是配置回落值（默认 292），此时一个合法的 332 会被误判成
			// 出口污染，进而把一个好代理错误地冷却掉。只有套餐明确时才标记。
			if status == http.StatusOK && state != "" && openAICodexTicketPlanKnown(account) {
				s.openAICodexTicketProxyBurn(proxyURL, time.Duration(cfg.HarvestProxyBurnCooldownSeconds)*time.Second, now)
			}
			// len 是关键诊断位：312 说明当前出口 IP 已被降智，不是网络问题。
			logger.L().Info("openai_codex_ticket probe miss",
				zap.Int64("account_id", account.ID), zap.String("model", model),
				zap.Int("http", status), zap.Int("len", len(state)),
				zap.String("proxy", MaskProxyURL(proxyURL)))
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
		s.openAICodexTicketProxyRecover(proxyURL)
		logger.L().Info("openai_codex_ticket harvested",
			zap.Int64("account_id", account.ID), zap.String("model", model),
			zap.Int("length", ticket.Length), zap.String("mode", "continuous"),
			zap.Time("issued_at", issuedAt), zap.Duration("capture_lag", now.Sub(issuedAt)),
			zap.String("proxy", MaskProxyURL(proxyURL)))
		return nil, nil
	})
}

// pickOpenAICodexTicketHarvestProxy 从代理池里轮转取一个出口。
//
// 选取规则：优先未被标记为"已烧"的代理，按原子计数器轮转以摊平负载；全部在冷却
// 中时退而使用最早被标记的那个（它冷却得最久，最可能已经恢复），而不是直接放弃
// 打票——放弃意味着这一轮所有账号都拿不到票。
func (s *OpenAIGatewayService) pickOpenAICodexTicketHarvestProxy(pool []string, now time.Time) string {
	if len(pool) == 0 {
		return ""
	}
	if len(pool) == 1 {
		return pool[0]
	}
	start := int(s.openaiCodexTicketProxyCursor.Add(1)-1) % len(pool)
	oldestIdx := -1
	var oldestAt time.Time
	for i := 0; i < len(pool); i++ {
		idx := (start + i) % len(pool)
		candidate := pool[idx]
		burnedUntil, burnedAt := s.openAICodexTicketProxyBurnState(candidate)
		if burnedUntil.IsZero() || !now.Before(burnedUntil) {
			return candidate
		}
		if oldestIdx < 0 || burnedAt.Before(oldestAt) {
			oldestIdx, oldestAt = idx, burnedAt
		}
	}
	return pool[oldestIdx]
}

// openAICodexTicketProxyBurn 记录某个代理的出口当前拿不到合格票（典型是返回 312，
// 即该 IP 已被降智），冷却期内不再优先选它。
func (s *OpenAIGatewayService) openAICodexTicketProxyBurn(proxyURL string, cooldown time.Duration, now time.Time) {
	if s == nil || strings.TrimSpace(proxyURL) == "" || cooldown <= 0 {
		return
	}
	s.openaiCodexTicketProxyBurn.Store(proxyURL, &openAICodexTicketProxyBurnState{
		burnedAt:    now,
		burnedUntil: now.Add(cooldown),
	})
}

// openAICodexTicketProxyRecover 该代理刚打出合格票，立即解除标记。
func (s *OpenAIGatewayService) openAICodexTicketProxyRecover(proxyURL string) {
	if s == nil || strings.TrimSpace(proxyURL) == "" {
		return
	}
	s.openaiCodexTicketProxyBurn.Delete(proxyURL)
}

func (s *OpenAIGatewayService) openAICodexTicketProxyBurnState(proxyURL string) (until, at time.Time) {
	if s == nil {
		return time.Time{}, time.Time{}
	}
	raw, ok := s.openaiCodexTicketProxyBurn.Load(proxyURL)
	if !ok {
		return time.Time{}, time.Time{}
	}
	state, ok := raw.(*openAICodexTicketProxyBurnState)
	if !ok || state == nil {
		return time.Time{}, time.Time{}
	}
	return state.burnedUntil, state.burnedAt
}

// openAICodexTicketProxyBurnState 记录某代理被标记的时刻与冷却截止。
type openAICodexTicketProxyBurnState struct {
	burnedAt    time.Time
	burnedUntil time.Time
}

// openAICodexTicketCandidateContextKey 暂存本次请求从上游响应头里看到的 turn-state。
const openAICodexTicketCandidateContextKey = "openai_codex_ticket_candidate"

// openAICodexTicketCandidate 候选必须绑定铸造账号。failover 时同一个 gin.Context
// 会被复用于多个账号的 attempt，裸 string 会让 A 号铸造的 state 落到 B 号名下——
// 这正是 guardOpenAICodexTurnStateEcho 存在的同一个坑。
type openAICodexTicketCandidate struct {
	accountID int64
	state     string
}

// openAICodexTicketPassiveCaptureEnabled 被动采集默认开启。
func (s *OpenAIGatewayService) openAICodexTicketPassiveCaptureEnabled() bool {
	cfg := s.openAICodexTicketConfig()
	if cfg.PassiveCaptureEnabled == nil {
		return true
	}
	return *cfg.PassiveCaptureEnabled
}

// stageOpenAICodexTicketCandidate 在响应头提交点暂存 turn-state 候选。
//
// 此刻还不能落库：上游实际响应的模型要等流式响应读完才知道，而一张从"已被降级
// 的回合"里拿到的 state 正是我们要拒绝的东西。所以先暂存，等 commit 时校验。
// 无条件覆盖：state 为空时必须清除上一个 attempt 留下的候选，否则 failover
// 换号后旧账号的 state 会被当成新账号的票。
func stageOpenAICodexTicketCandidate(c *gin.Context, account *Account, state string) {
	if c == nil {
		return
	}
	state = strings.TrimSpace(state)
	if state == "" || account == nil || account.ID <= 0 {
		c.Set(openAICodexTicketCandidateContextKey, (*openAICodexTicketCandidate)(nil))
		return
	}
	c.Set(openAICodexTicketCandidateContextKey, &openAICodexTicketCandidate{
		accountID: account.ID,
		state:     state,
	})
}

// stagedOpenAICodexTicketCandidate 只在候选确实由 account 铸造时返回 state。
func stagedOpenAICodexTicketCandidate(c *gin.Context, account *Account) string {
	if c == nil || account == nil {
		return ""
	}
	raw, ok := c.Get(openAICodexTicketCandidateContextKey)
	if !ok {
		return ""
	}
	candidate, _ := raw.(*openAICodexTicketCandidate)
	if candidate == nil || candidate.accountID != account.ID {
		return ""
	}
	return strings.TrimSpace(candidate.state)
}

// CommitOpenAICodexTicketFromResponse 被动采集的落库点：请求完成后，若本次上游
// 响应既没被降级、state 长度又符合该账号套餐，就把它存成票据。
//
// 这是票据的主路径，相对主动探测的优势是根本性的：
//   - 零额外请求，不消耗配额
//   - 走账号自己的业务出口 IP，不制造跨 ASN 关联特征
//   - state 来自一个已被证明"没有降级"的真实回合，而不是一次合成的 ping
//
// outboundModel 是真正发给上游的模型名；responseModel 是上游自报的响应模型。
// 两者不一致说明这一回合已被降级（实测 gpt-6-astra → gpt-5.6-luna），此时的
// state 不能留。
func (s *OpenAIGatewayService) CommitOpenAICodexTicketFromResponse(ctx context.Context, c *gin.Context, account *Account, outboundModel, responseModel string) {
	// 响应模型观测器在 Forward 开头创建、跨 compact fallback 与 attempt 共用。
	// 一旦它看到过互相冲突的模型声明，这次聚合结果就不足以证明"本回合未被降级"，
	// 宁可不留票。
	if observedUpstreamResponseModelConflict(c) {
		return
	}
	if s == nil || c == nil || account == nil {
		return
	}
	// 门禁与落库必须同源：客户端拿到终止事件后立刻断开是常态，此时 Forward 的
	// ctx 已 cancel，而 GetOpenAICodexTicketEnabled 在 ctx 取消时直接返回 yaml
	// fallback（不查缓存不查 DB）。面板开启但 yaml 未开的部署里，这会让被动采集
	// 恰好在这些断连回合被门禁拦死——而落库本身已经解绑了取消。
	ctx = context.WithoutCancel(ctx)
	if !s.openAICodexTicketPassiveCaptureEnabled() || !s.openAICodexTicketEnabledContext(ctx) {
		return
	}
	if !isOpenAICodexTicketAccount(account) || !openAICodexTicketAccountEnabled(account) {
		return
	}
	model := normalizeOpenAICodexTicketModel(outboundModel)
	if model == "" || !s.openAICodexTicketGatedModel(model) {
		return
	}
	state := stagedOpenAICodexTicketCandidate(c, account)
	if state == "" {
		return
	}
	cfg := s.openAICodexTicketConfig()
	wantLen := openAICodexTicketTargetLength(account, cfg)
	if len(state) != wantLen || !strings.HasPrefix(state, openAICodexTicketStatePrefix) {
		// 长度不符说明这一回合的 state 不是该套餐的"最短档"，通常伴随降级。
		return
	}
	// 上游自报模型与出站模型不一致 → 这一回合已被降级，state 不可留。
	// responseModel 为空表示没观测到（非流式或解析失败），保守起步：不落库。
	if !strings.EqualFold(strings.TrimSpace(responseModel), model) {
		return
	}
	// check-then-store 必须串行化：两个同账号同模型的并发请求可能各自读到旧票，
	// 然后用较旧的 issued_at 覆盖较新的。复用采集用的 singleflight 组按 key 收敛。
	key := openAICodexTicketKey(account.ID, model)
	_, _, _ = s.openaiCodexTicketFlight.Do("commit\x00"+key, func() (any, error) {
		s.commitOpenAICodexTicketLocked(ctx, account, model, state, cfg)
		return nil, nil
	})
}

func (s *OpenAIGatewayService) commitOpenAICodexTicketLocked(ctx context.Context, account *Account, model, state string, cfg config.OpenAICodexTicketConfig) {
	wantLen := openAICodexTicketTargetLength(account, cfg)
	now := time.Now()
	ttl := time.Duration(cfg.TTLSeconds) * time.Second
	issuedAt := resolveOpenAICodexTicketIssuedAt(state, now, ttl, account.ID, model)
	existing := s.lookupOpenAICodexTicket(account, model)
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
	// 已有更新的票就不覆盖（并发请求可能各自带回不同回合的 state）。
	if existing != nil && existing.valid(now, wantLen) && existing.IssuedAt.After(issuedAt) {
		return
	}
	// ctx 在函数入口已 WithoutCancel，与请求生命周期解绑。
	s.storeOpenAICodexTicket(ctx, account, ticket)
	s.clearOpenAICodexTicketBackoff(openAICodexTicketKey(account.ID, model))
	logger.L().Info("openai_codex_ticket captured passively",
		zap.Int64("account_id", account.ID), zap.String("model", model),
		zap.Int("length", ticket.Length), zap.Time("issued_at", issuedAt),
		zap.String("source", "business_response"))
}

// openAICodexTicketAccountRecentlyUsed 判断账号是否在活跃窗口内跑过流量。
// 窗口 <=0 表示不限制（退回上游行为：给所有 active 账号打票）。
// LastUsedAt 为零值（从未使用）时不打票。
func openAICodexTicketAccountRecentlyUsed(account *Account, cfg config.OpenAICodexTicketConfig, now time.Time) bool {
	if account == nil {
		return false
	}
	if cfg.HarvestActiveWindowSeconds <= 0 {
		return true
	}
	if account.LastUsedAt == nil || account.LastUsedAt.IsZero() {
		return false
	}
	return account.LastUsedAt.After(now.Add(-time.Duration(cfg.HarvestActiveWindowSeconds) * time.Second))
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

// SplitOpenAICodexTicketHarvestProxyPool 把打票代理设置拆成代理池。
// 兼容单个 URL（历史取值）与逗号/空白分隔的多个 URL：该功能的前提是出口 IP
// 新鲜度，单个 IP 反复打票会自己变黑，所以需要一批 IP 轮换。
func SplitOpenAICodexTicketHarvestProxyPool(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t' || r == ';'
	})
	pool := make([]string, 0, len(fields))
	seen := make(map[string]struct{}, len(fields))
	for _, item := range fields {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, dup := seen[item]; dup {
			continue
		}
		seen[item] = struct{}{}
		pool = append(pool, item)
	}
	return pool
}

// ValidateOpenAICodexTicketHarvestProxyPool 逐个校验代理池成员，错误里带序号但
// 不带凭据。空值合法（表示不配置，采集不会启动）。
func ValidateOpenAICodexTicketHarvestProxyPool(raw string) error {
	for i, item := range SplitOpenAICodexTicketHarvestProxyPool(raw) {
		if err := ValidateOpenAICodexTicketHarvestProxyURL(item); err != nil {
			return fmt.Errorf("harvest proxy #%d: %w", i+1, err)
		}
	}
	return nil
}

// IsMaskedOpenAICodexTicketHarvestProxyPool 判断提交上来的池串是否为掩码回显。
// 空串视为掩码（保留旧值），任一成员是掩码即整体判为掩码——半真半掩的提交无法
// 可靠合并，保留旧值比存进一个带 *** 的假密码安全。
func IsMaskedOpenAICodexTicketHarvestProxyPool(raw string) bool {
	pool := SplitOpenAICodexTicketHarvestProxyPool(raw)
	if len(pool) == 0 {
		return true
	}
	for _, item := range pool {
		if IsMaskedProxyURL(item) {
			return true
		}
	}
	return false
}

// MaskOpenAICodexTicketHarvestProxyPool 掩码整个池，保持分隔形式便于管理端回显。
func MaskOpenAICodexTicketHarvestProxyPool(raw string) string {
	pool := SplitOpenAICodexTicketHarvestProxyPool(raw)
	masked := make([]string, 0, len(pool))
	for _, item := range pool {
		if m := MaskProxyURL(item); m != "" {
			masked = append(masked, m)
		}
	}
	return strings.Join(masked, ",")
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
