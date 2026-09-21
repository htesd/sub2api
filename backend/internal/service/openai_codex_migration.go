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
		if status.MigratedAt != nil && time.Since(*status.MigratedAt) < 10*time.Minute {
			status.Result = "migration_cooldown"
			return nil, nil
		}
		if _, allowed = s.codexMigrationAuthorized(ctx, account); !allowed {
			return nil, nil
		}
		origin, err := s.codexMigrationEgress(ctx, account, account.Proxy.URL())
		if err != nil {
			status.Result = "egress_unknown"
			return nil, nil
		}
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
			egress, err := s.codexMigrationEgress(ctx, account, p.URL())
			if err != nil || egress.country != origin.country || egress.hash == origin.hash {
				continue
			}
			status.ToProxyID = p.ID
			status.Country = egress.country
			status.IPHash = egress.hash
			if _, allowed = s.codexMigrationAuthorized(ctx, account); !allowed {
				return nil, nil
			}
			state, code, observed, matched := s.fireCodexMigrationProbe(ctx, account, token, p.URL(), session, "")
			status.ResponseModel = observed
			status.Result = "candidate_not_astra"
			if code == 401 || code == 403 || code == 429 {
				status.Result = "upstream_limited"
				return nil, nil
			}
			t := newCodexMigrationTicket(account, p, state, cfg.TTLSeconds)
			if !matched || !t.valid(time.Now(), openAICodexTicketTargetLength(account, cfg)) {
				if matched {
					status.Result = "candidate_ticket_invalid"
				}
				return nil, nil
			}
			// 使用新票在同代理复验，防止只读头/无票成功就把业务迁过去。
			if _, allowed = s.codexMigrationAuthorized(ctx, account); !allowed {
				return nil, nil
			}
			_, _, observed, matched = s.fireCodexMigrationProbe(ctx, account, token, p.URL(), session, state)
			if !matched {
				status.Result = "ticket_replay_failed"
				status.ResponseModel = observed
				return nil, nil
			}
			after, err := s.codexMigrationEgress(ctx, account, p.URL())
			if err != nil || after != egress {
				status.Result = "egress_changed"
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
