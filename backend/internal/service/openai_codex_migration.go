package service

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const (
	CodexAutoMigrateKey    = "codex_ticket_auto_migrate"
	CodexMigrationStateKey = "codex_ticket_migration_state"
	CodexMigrationEpochKey = "codex_ticket_migration_epoch"
)

// 仅保存运维摘要；不保存代理地址、凭据、票原文或对话内容。
type CodexMigrationState struct {
	Result        string     `json:"result"`
	CheckedAt     time.Time  `json:"checked_at"`
	FromProxyID   int64      `json:"from_proxy_id"`
	ToProxyID     int64      `json:"to_proxy_id,omitempty"`
	Country       string     `json:"country,omitempty"`
	IPHash        string     `json:"ip_hash,omitempty"`
	ResponseModel string     `json:"response_model,omitempty"`
	MigratedAt    *time.Time `json:"migrated_at,omitempty"`
}

// 可选扩展接口，避免污染其他提供商的账号仓储契约。
type CodexMigrationRepository interface {
	ListCodexMigrationProxies(context.Context) ([]Proxy, error)
	CommitCodexMigration(context.Context, *Account, *Proxy, any, CodexMigrationState) (bool, error)
	StoreCodexTicketForRoute(context.Context, *Account, string, any) (bool, error)
	RecordCodexMigrationState(context.Context, *Account, CodexMigrationState) error
}

func codexAutoMigrate(account *Account) bool {
	if account == nil || account.IsCredentialShadow() {
		return false
	}
	v, _ := account.Extra[CodexAutoMigrateKey].(bool)
	return v
}

func codexMigrationEpoch(account *Account) string {
	if account == nil {
		return ""
	}
	v, _ := account.Extra[CodexMigrationEpochKey].(string)
	return v
}

func codexProxyID(account *Account) int64 {
	if account == nil || account.ProxyID == nil {
		return 0
	}
	return *account.ProxyID
}

func codexMigrationCheckDue(account *Account, now time.Time) bool {
	b, err := json.Marshal(account.Extra[CodexMigrationStateKey])
	if err != nil {
		return true
	}
	var state CodexMigrationState
	if json.Unmarshal(b, &state) != nil || (state.Result != "current_proxy_ok" && state.Result != "migrated") {
		return true
	}
	return now.Sub(state.CheckedAt) >= 5*time.Minute
}

func codexTicketRouteMatches(account *Account, ticket *openAICodexTicket) bool {
	if ticket == nil {
		return false
	}
	if ticket.RouteEpoch != codexMigrationEpoch(account) {
		return false
	}
	return ticket.SourceProxyID == nil || (*ticket.SourceProxyID == codexProxyID(account) &&
		(ticket.SourceProxyUpdatedAt == nil || (account.Proxy != nil && ticket.SourceProxyUpdatedAt.Equal(account.Proxy.UpdatedAt))))
}

func (s *OpenAIGatewayService) codexMigrationAuthorized(ctx context.Context, original *Account) (*Account, bool) {
	cfg := s.openAICodexTicketConfigContext(ctx)
	if ctx.Err() != nil || !cfg.Enabled || !cfg.HarvestLoopEnabled {
		return nil, false
	}
	a, err := s.accountRepo.GetByID(ctx, original.ID)
	if err != nil || a == nil || !a.IsSchedulable() || !isOpenAICodexTicketAccount(a) || !codexAutoMigrate(a) ||
		!openAICodexTicketAccountEnabled(a) || !openAICodexTicketAccountActiveProbeEnabled(a) ||
		codexMigrationEpoch(a) != codexMigrationEpoch(original) || openAICodexTicketAccountProxyURL(a) != openAICodexTicketAccountProxyURL(original) {
		return nil, false
	}
	return a, true
}

type codexEgress struct{ country, hash string }

// 地区和稳定性来自目标域名的 trace；无法观测时不允许自动跨出口。
func (s *OpenAIGatewayService) codexMigrationEgress(ctx context.Context, account *Account, proxy string) (codexEgress, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://chatgpt.com/cdn-cgi/trace", nil)
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAIHarvest))
	applyOpenAICodexTicketHarvestIdentity(req.Header, openAICodexTicketDefaultModel)
	resp, err := s.httpUpstream.Do(req, proxy, account.ID, account.Concurrency)
	if err != nil {
		return codexEgress{}, errors.New("egress_unknown")
	}
	if resp == nil || resp.Body == nil {
		return codexEgress{}, errors.New("egress_unknown")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return codexEgress{}, errors.New("egress_unknown")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil || len(b) > 4096 {
		return codexEgress{}, errors.New("egress_unknown")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			fields[k] = strings.TrimSpace(v)
		}
	}
	ip := net.ParseIP(fields["ip"])
	country := fields["loc"]
	if ip == nil || len(country) != 2 || country == "XX" || country[0] < 'A' || country[0] > 'Z' || country[1] < 'A' || country[1] > 'Z' {
		return codexEgress{}, errors.New("egress_unknown")
	}
	digest := sha256.Sum256([]byte(ip.String()))
	return codexEgress{country, hex.EncodeToString(digest[:8])}, nil
}

// 必须读到完整成功终态；任何错误、冲突模型、截断都不能促成代理迁移。
func readCodexMigrationModel(r io.Reader) (string, bool) {
	scanner := bufio.NewScanner(io.LimitReader(r, 1024*1024+1))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var data []string
	model := ""
	invalid := false
	consume := func() (bool, bool) {
		payload := []byte(strings.Join(data, "\n"))
		data = nil
		if bytes.Equal(payload, []byte("[DONE]")) {
			return true, false
		}
		if !gjson.ValidBytes(payload) {
			invalid = true
			return false, false
		}
		if openai.ConflictingResponseModels(payload) {
			invalid = true
		}
		for _, field := range gjson.GetManyBytes(payload, "response.model", "model") {
			if field.Exists() && field.Type != gjson.String {
				invalid = true
			}
			m := strings.ToLower(strings.TrimSpace(field.String()))
			if m != "" {
				if model != "" && model != m {
					invalid = true
				}
				model = m
			}
		}
		typ := gjson.GetBytes(payload, "type").String()
		switch typ {
		case "response.completed", "response.done":
			return true, !invalid && model == openAICodexTicketDefaultModel && openai.CompletedResponseEvidence(payload, typ)
		case "error", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
			return true, false
		}
		return false, false
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if len(data) > 0 {
				if stop, ok := consume(); stop {
					return model, ok
				}
			}
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if scanner.Err() == nil && len(data) > 0 {
		_, ok := consume()
		return model, ok
	}
	return model, false
}

func (s *OpenAIGatewayService) fireCodexMigrationProbe(ctx context.Context, account *Account, token, proxy, session, ticket string) (string, int, string, bool) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.openAICodexTicketConfigContext(ctx).HarvestAttemptTimeoutSeconds)*time.Second)
	defer cancel()
	body := []byte(`{"model":"gpt-6-astra","store":false,"stream":true,"instructions":"Reply with exactly: pong","input":[{"role":"user","content":[{"type":"input_text","text":"ping"}]}]}`)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, chatgptCodexURL, bytes.NewReader(body))
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAIHarvest))
	req.Close = true
	req.Host = "chatgpt.com"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("session_id", session)
	applyCodexFingerprintHeaders(req.Header, resolveCodexFingerprintIDsFromRequest(account, req.Header))
	if ticket != "" {
		req.Header.Set(openAICodexTurnStateHeader, ticket)
	}
	if resolveAndSetOpenAIChatGPTAccountHeaders(ctx, s.accountRepo, req.Header, account) != nil {
		return "", 0, "", false
	}
	applyOpenAICodexTicketHarvestIdentity(req.Header, openAICodexTicketDefaultModel)
	resp, err := s.httpUpstream.Do(req, proxy, account.ID, account.Concurrency)
	if err != nil || resp == nil {
		return "", 0, "", false
	}
	if resp.Body == nil {
		return "", resp.StatusCode, "", false
	}
	defer resp.Body.Close()
	state := extractOpenAICodexTurnState(resp.Header)
	if resp.StatusCode != http.StatusOK {
		return state, resp.StatusCode, "", false
	}
	model, ok := readCodexMigrationModel(resp.Body)
	return state, resp.StatusCode, model, ok
}

func (s *OpenAIGatewayService) probeCodexMigration(ctx context.Context, account *Account) {
	repo, ok := s.accountRepo.(CodexMigrationRepository)
	if !ok {
		return
	}
	key := openAICodexTicketKey(account.ID, openAICodexTicketDefaultModel)
	_, _, _ = s.openaiCodexTicketFlight.Do(key, func() (any, error) {
		current, allowed := s.codexMigrationAuthorized(ctx, account)
		if !allowed {
			return nil, nil
		}
		account = current
		cfg := s.openAICodexTicketConfigContext(ctx)
		status := CodexMigrationState{Result: "probe_failed", CheckedAt: time.Now(), FromProxyID: codexProxyID(account)}
		if b, e := json.Marshal(account.Extra[CodexMigrationStateKey]); e == nil {
			var old CodexMigrationState
			if json.Unmarshal(b, &old) == nil {
				status.MigratedAt = old.MigratedAt
			}
		}
		defer func() {
			if status.Result != "migrated" && status.Result != "current_proxy_ok" {
				s.noteOpenAICodexTicketProbeFailure(key, cfg, time.Now())
				_ = repo.RecordCodexMigrationState(ctx, account, status)
			}
		}()
		if account.Proxy == nil || !account.Proxy.IsActive() || account.Proxy.IsExpired(time.Now()) {
			status.Result = "source_proxy_missing"
			return nil, nil
		}
		token, _, err := s.GetAccessToken(ctx, account)
		if err != nil || token == "" {
			status.Result = "token_unavailable"
			return nil, nil
		}
		// token provider可能替换凭据；取刷新后的快照，并核对实际使用的token仍属于它。
		refreshed, stillAllowed := s.codexMigrationAuthorized(ctx, account)
		if !stillAllowed {
			return nil, nil
		}
		account = refreshed
		// 优先原代理；当前票可用时携票验证，未绑定/未知旧池票不作为阳性依据。
		session := uuid.NewString()
		sent := ""
		originalTicket := s.lookupOpenAICodexTicket(account, openAICodexTicketDefaultModel)
		if originalTicket.valid(time.Now(), openAICodexTicketTargetLength(account, cfg)) {
			sent = originalTicket.State
		}
		state, httpStatus, model, valid := s.fireCodexMigrationProbe(ctx, account, token, account.Proxy.URL(), session, sent)
		status.ResponseModel = model
		if valid {
			if state == "" {
				state = sent
			}
			t := newCodexMigrationTicket(account, account.Proxy, state, cfg.TTLSeconds)
			if state == sent && originalTicket != nil {
				t.IssuedAt = originalTicket.IssuedAt
				t.ExpiresAt = originalTicket.ExpiresAt
			}
			if t.valid(time.Now(), openAICodexTicketTargetLength(account, cfg)) {
				if _, allowed = s.codexMigrationAuthorized(ctx, account); !allowed {
					return nil, nil
				}
				stored, storeErr := repo.StoreCodexTicketForRoute(ctx, account, openAICodexTicketExtraKey(t.Model), t)
				if storeErr != nil || !stored {
					status.Result = "configuration_changed"
					return nil, nil
				}
				s.openaiCodexTickets.Store(key, t)
				status.Result = "current_proxy_ok"
				_ = repo.RecordCodexMigrationState(ctx, account, status)
				s.clearOpenAICodexTicketBackoff(key)
				return nil, nil
			}
			// 模型健康与取票成功是两件事；无票/规格不符不构成迁移理由。
			status.Result = "current_proxy_ok_no_ticket"
			return nil, nil
		}
		if httpStatus == 401 || httpStatus == 403 || httpStatus == 429 {
			status.Result = "upstream_limited"
			return nil, nil
		}
		// 本来无票时，上面那次请求只是采票，降级响应不能判定原出口不可用；
		// 先拿新票在原代理复验，避免把本可留下的账号迁走。
		if sent == "" {
			t := newCodexMigrationTicket(account, account.Proxy, state, cfg.TTLSeconds)
			if t.valid(time.Now(), openAICodexTicketTargetLength(account, cfg)) {
				if _, allowed = s.codexMigrationAuthorized(ctx, account); !allowed {
					return nil, nil
				}
				_, replayStatus, replayModel, replayOK := s.fireCodexMigrationProbe(ctx, account, token, account.Proxy.URL(), session, state)
				if replayStatus == 401 || replayStatus == 403 || replayStatus == 429 {
					status.Result = "upstream_limited"
					return nil, nil
				}
				status.ResponseModel = replayModel
				if replayOK {
					if _, allowed = s.codexMigrationAuthorized(ctx, account); !allowed {
						return nil, nil
					}
					stored, storeErr := repo.StoreCodexTicketForRoute(ctx, account, openAICodexTicketExtraKey(t.Model), t)
					if storeErr != nil || !stored {
						status.Result = "configuration_changed"
						return nil, nil
					}
					s.openaiCodexTickets.Store(key, t)
					status.Result = "current_proxy_ok"
					_ = repo.RecordCodexMigrationState(ctx, account, status)
					s.clearOpenAICodexTicketBackoff(key)
					return nil, nil
				}
			}
		}
		if status.MigratedAt != nil && time.Since(*status.MigratedAt) < 10*time.Minute {
			status.Result = "migration_cooldown"
			return nil, nil
		}
		if _, allowed = s.codexMigrationAuthorized(ctx, account); !allowed {
			return nil, nil
		}
		// 原出口画像不再参与判定（不比国家、不比 IP），省掉这一发 trace。
		proxies, err := repo.ListCodexMigrationProxies(ctx)
		if err != nil {
			status.Result = "proxy_lookup_failed"
			return nil, nil
		}
		pool := SplitOpenAICodexTicketHarvestProxyPool(s.openAICodexTicketHarvestProxyURLContext(ctx))
		// 最多核验三个候选出口、最多向一个同地区候选发GPT-6请求；复用池轮换/退避。
		eligible := map[string]*Proxy{}
		urls := []string{}
		for i := range proxies {
			p := &proxies[i]
			if p.ID == account.Proxy.ID || !p.IsActive() || p.IsExpired(time.Now()) {
				continue
			}
			for _, u := range pool {
				if sameOpenAICodexTicketProxy(u, p.URL()) {
					if _, exists := eligible[u]; exists {
						break
					}
					eligible[u] = p
					urls = append(urls, u)
					break
				}
			}
		}
		status.Result = "no_same_region_candidate"
		for checked := 0; checked < 3 && len(urls) > 0; checked++ {
			candidate := s.pickOpenAICodexTicketHarvestProxy(urls, time.Now())
			if candidate == "" {
				break
			}
			p := eligible[candidate]
			next := []string{}
			for _, u := range urls {
				if u != candidate {
					next = append(next, u)
				}
			}
			urls = next
			if _, allowed = s.codexMigrationAuthorized(ctx, account); !allowed {
				return nil, nil
			}
			// 不再要求候选与原出口同国家：能拿到真模型优先于出口地理一致性。
			// trace 只用于记录出口画像，失败或跨国都不阻断切换。
			status.ToProxyID = p.ID
			if egress, err := s.codexMigrationEgress(ctx, account, p.URL()); err == nil {
				status.Country = egress.country
				status.IPHash = egress.hash
			}
			if _, allowed = s.codexMigrationAuthorized(ctx, account); !allowed {
				return nil, nil
			}
			// 采票请求本身常被降级（无票时上游多返回 luna），它只负责产出 turn-state；
			// 能力证明一律留给下一步的携票复验，不能用采票响应的模型名提前否决候选。
			state, code, observed, _ := s.fireCodexMigrationProbe(ctx, account, token, p.URL(), session, "")
			status.ResponseModel = observed
			status.Result = "candidate_ticket_invalid"
			if code == 401 || code == 403 || code == 429 {
				status.Result = "upstream_limited"
				return nil, nil
			}
			t := newCodexMigrationTicket(account, p, state, cfg.TTLSeconds)
			if !t.valid(time.Now(), openAICodexTicketTargetLength(account, cfg)) {
				return nil, nil
			}
			// 票据与出口必须同源：候选出口采到合格票即提交迁移，不再要求携票复验拿到
			// 完整 astra。原因是 astra 降级由服务端按账号决定，任何出口都救不回已降级的
			// 账号（当天 20 例请求形状 × 2 出口 × 有票/无票全部返回 luna），把 astra 当
			// 切换前置条件会让所有迁移永远停在 candidate_not_astra，票被丢弃、账号在拒发
			// 名单下彻底不可用。降级识别改由请求期的降智阻断（.10）承担：真降级时冷却账号
			// 并换号，因此这里切过去最差是中性，不会把用户留在错误模型上。
			if _, allowed = s.codexMigrationAuthorized(ctx, account); !allowed {
				return nil, nil
			}

			if _, allowed = s.codexMigrationAuthorized(ctx, account); !allowed {
				return nil, nil
			}
			// 池热更新撤销候选后也不得提交迁移。
			stillInPool := false
			for _, u := range SplitOpenAICodexTicketHarvestProxyPool(s.openAICodexTicketHarvestProxyURLContext(ctx)) {
				if sameOpenAICodexTicketProxy(u, p.URL()) {
					stillInPool = true
				}
			}
			if !stillInPool {
				status.Result = "candidate_removed"
				return nil, nil
			}
			status.Result = "migrated"
			status.CheckedAt = time.Now()
			status.MigratedAt = &status.CheckedAt
			if !t.valid(time.Now(), openAICodexTicketTargetLength(account, cfg)) {
				status.Result = "ticket_replay_failed"
				return nil, nil
			}
			t.RouteEpoch = uuid.NewString()
			committed, err := repo.CommitCodexMigration(ctx, account, p, t, status)
			if err != nil || !committed {
				status.Result = "configuration_changed"
				return nil, nil
			}
			// 删除所有旧出口内存票；新快照会从同事务更新的数据库加载票。
			for _, m := range cfg.Models {
				s.openaiCodexTickets.Delete(openAICodexTicketKey(account.ID, m))
			}
			s.clearOpenAICodexTicketBackoff(key)
			logger.L().Info("openai_codex_ticket proxy migrated", zap.Int64("account_id", account.ID), zap.Int64("from_proxy_id", status.FromProxyID), zap.Int64("to_proxy_id", p.ID), zap.String("model", openAICodexTicketDefaultModel), zap.String("country", status.Country))
			return nil, nil
		}
		return nil, nil
	})
}

func newCodexMigrationTicket(a *Account, p *Proxy, state string, ttl int) *openAICodexTicket {
	now := time.Now()
	issued := resolveOpenAICodexTicketIssuedAt(state, now, time.Duration(ttl)*time.Second, a.ID, openAICodexTicketDefaultModel)
	return &openAICodexTicket{AccountID: a.ID, Model: openAICodexTicketDefaultModel, State: state, Length: len(state), CapturedAt: now, IssuedAt: issued, ExpiresAt: issued.Add(time.Duration(ttl) * time.Second), Source: "account_proxy", SourceProxyID: &p.ID, SourceProxyUpdatedAt: &p.UpdatedAt, RouteEpoch: codexMigrationEpoch(a), ModelVerified: true}
}

// commitOpenAICodexPoolTicketMigration 把「从打票池代理采到的票」与账号出口绑定到一起：
// 在同一事务内切换账号业务代理到该池代理、落票、换路由代际并清掉旧出口票。
// 这是票据同源不变量的落点——票在哪个出口签发，账号就从哪个出口发请求。
// 返回 false 表示无法切换（未开迁移开关、池代理不在代理管理内/已停用/已过期、提交被 CAS 拒绝），
// 此时调用方必须丢弃这张票，绝不允许跨出口注入。
func (s *OpenAIGatewayService) commitOpenAICodexPoolTicketMigration(ctx context.Context, account *Account, proxyURL string, ticket *openAICodexTicket, models []string) bool {
	repo, ok := s.accountRepo.(CodexMigrationRepository)
	if !ok || account == nil || ticket == nil {
		return false
	}
	// 迁移开关保护的是「你手工配好的出口」：已绑代理的账号必须开关打开才允许被改出口；
	// 未绑代理的账号没有出口偏好可保护，为了让它们能持有可用票，一律允许跟票切过去。
	bound := account.ProxyID != nil && *account.ProxyID > 0
	if bound {
		if !codexAutoMigrate(account) {
			return false
		}
		if _, allowed := s.codexMigrationAuthorized(ctx, account); !allowed {
			return false
		}
	}
	proxies, err := repo.ListCodexMigrationProxies(ctx)
	if err != nil {
		return false
	}
	var target *Proxy
	now := time.Now()
	for i := range proxies {
		p := &proxies[i]
		if p.IsActive() && !p.IsExpired(now) && sameOpenAICodexTicketProxy(p.URL(), proxyURL) {
			target = p
			break
		}
	}
	if target == nil {
		logger.L().Info("openai_codex_ticket pool ticket dropped", zap.Int64("account_id", account.ID),
			zap.String("model", ticket.Model), zap.String("reason", "proxy_not_manageable"),
			zap.String("proxy", MaskProxyURL(proxyURL)))
		return false
	}
	ticket.SourceProxyID = &target.ID
	ticket.SourceProxyUpdatedAt = &target.UpdatedAt
	ticket.RouteEpoch = uuid.NewString()
	status := CodexMigrationState{
		Result: "migrated", CheckedAt: now, MigratedAt: &now,
		FromProxyID: codexProxyID(account), ToProxyID: target.ID,
	}
	committed, err := repo.CommitCodexMigration(ctx, account, target, ticket, status)
	if err != nil || !committed {
		return false
	}
	for _, m := range models {
		s.openaiCodexTickets.Delete(openAICodexTicketKey(account.ID, m))
	}
	logger.L().Info("openai_codex_ticket proxy migrated", zap.Int64("account_id", account.ID),
		zap.Int64("from_proxy_id", status.FromProxyID), zap.Int64("to_proxy_id", target.ID),
		zap.String("model", ticket.Model), zap.String("trigger", "pool_harvest"))
	return true
}
