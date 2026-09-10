package service

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// CodexRequestPolicy is opt-in and snapshotted for the entire request/WS turn.
// A WS dial and a response.create each count as an upstream attempt.
type CodexRequestPolicy struct {
	Enabled             bool   `json:"enabled"`
	MaxAttempts         int    `json:"max_attempts"`
	RetryWindowSeconds  int    `json:"retry_window_seconds"`
	CooldownSeconds     int    `json:"cooldown_seconds"`
	IdentityMode        string `json:"identity_mode"`
	CapacityMode        string `json:"capacity_mode"`
	CapacityWaitSeconds int    `json:"capacity_wait_seconds"`
}

func (a *Account) codexRequestPolicy() CodexRequestPolicy {
	p := CodexRequestPolicy{MaxAttempts: 6, RetryWindowSeconds: 30, CooldownSeconds: 5, IdentityMode: "canonical", CapacityMode: "inherit", CapacityWaitSeconds: 10}
	if a == nil || !a.IsOpenAIOAuthLike() {
		return p
	}
	raw, ok := a.Extra["codex_request_policy"].(map[string]any)
	if !ok {
		return p
	}
	p.Enabled, _ = raw["enabled"].(bool)
	for k, v := range map[string]*int{"max_attempts": &p.MaxAttempts, "retry_window_seconds": &p.RetryWindowSeconds, "cooldown_seconds": &p.CooldownSeconds, "capacity_wait_seconds": &p.CapacityWaitSeconds} {
		if x, exists := raw[k]; exists {
			*v = parseExtraInt(x)
		}
	}
	p.MaxAttempts = max(1, min(20, p.MaxAttempts))
	p.RetryWindowSeconds = max(1, min(120, p.RetryWindowSeconds))
	p.CooldownSeconds = max(1, min(60, p.CooldownSeconds))
	p.CapacityWaitSeconds = max(0, min(60, p.CapacityWaitSeconds))
	if raw["identity_mode"] == "preserve" {
		p.IdentityMode = "preserve"
	}
	if v, _ := raw["capacity_mode"].(string); v == "queue" || v == "subagent" {
		p.CapacityMode = v
	}
	return p
}

const CodexRequestControlReason GatewayFailureReason = "codex_request_control"

func isCodexControlStop(err error) bool {
	var e *UpstreamFailoverError
	return errors.As(err, &e) && e.Reason == CodexRequestControlReason
}
func codexControlStop(code string, wait time.Duration) error {
	seconds := int(wait / time.Second)
	if wait%time.Second > 0 {
		seconds++
	}
	seconds = max(1, seconds)
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"type": "server_error", "code": code, "message": code}})
	return &UpstreamFailoverError{StatusCode: 503, ClientStatusCode: 503, ClientMessage: code, Reason: CodexRequestControlReason, NextAccountAction: NextAccountStop, RequestScopedTransient: true, ResponseBody: body, ResponseHeaders: http.Header{"Retry-After": {strconv.Itoa(seconds)}}}
}

type codexRequestControl struct {
	mu          sync.Mutex
	id          string
	parent      context.Context
	policy      CodexRequestPolicy
	configured  bool
	started     time.Time
	turn        int
	attempts    int
	total       int
	wait        time.Duration
	totalWait   time.Duration
	lastAccount int64
	lastModel   string
	lastFailure string
	group       int64
	finished    bool
}

func codexControl(ctx context.Context) *codexRequestControl {
	r := codexCapacityFromContext(ctx)
	if r == nil {
		return nil
	}
	return r.control
}
func newCodexRequestControl(ctx context.Context, group int64) *codexRequestControl {
	return &codexRequestControl{id: uuid.NewString(), parent: ctx, started: time.Now(), turn: 1, group: group}
}

// Only a new downstream turn resets the budget. Retries and account switches do not.
func resetCodexControlTurn(ctx context.Context, turn int) {
	if r := codexControl(ctx); r != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		if turn > r.turn {
			r.turn = turn
			r.attempts = 0
			r.started = time.Now()
			r.wait = 0
			r.lastFailure = ""
		}
	}
}
func (r *codexRequestControl) finish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished || !r.configured {
		return
	}
	r.finished = true
	slog.Info("codex_request_summary", "route_request_id", r.id, "turns", r.turn, "upstream_attempts", r.total, "last_account_id", r.lastAccount, "last_model", r.lastModel, "wait_ms", r.totalWait.Milliseconds(), "client_canceled", r.parent.Err() != nil)
}

type codexCooldownKey struct {
	group   int64
	model   string
	account int64
}
type codexCooldown struct {
	until   time.Time
	updated time.Time
	seen    map[int64]time.Time
}
type codexControlRuntime struct {
	mu        sync.Mutex
	cooldowns map[codexCooldownKey]codexCooldown
	waiters   int
}

func (s *OpenAIGatewayService) codexControls() *codexControlRuntime {
	s.codexControlOnce.Do(func() {
		s.codexControlRuntime = &codexControlRuntime{cooldowns: make(map[codexCooldownKey]codexCooldown)}
	})
	return s.codexControlRuntime
}
func (r *codexControlRuntime) cooldown(group int64, model string, account int64, now time.Time) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	until := r.cooldowns[codexCooldownKey{group, model, account}].until
	if shared := r.cooldowns[codexCooldownKey{group, model, 0}].until; shared.After(until) {
		until = shared
	}
	return codexLongerDuration(0, until.Sub(now))
}
func (r *codexControlRuntime) overload(group int64, model string, account int64, delay time.Duration, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Diagnostics/control caches have bounded cardinality and never store credentials.
	for k, v := range r.cooldowns {
		if now.Sub(v.updated) > time.Minute && now.After(v.until) {
			delete(r.cooldowns, k)
		}
	}
	key := codexCooldownKey{group, model, account}
	if _, exists := r.cooldowns[key]; !exists && len(r.cooldowns) >= 2048 {
		return
	}
	v := r.cooldowns[key]
	v.updated = now
	if until := now.Add(delay); until.After(v.until) {
		v.until = until
	}
	r.cooldowns[key] = v
	key.account = 0
	if _, exists := r.cooldowns[key]; !exists && len(r.cooldowns) >= 2048 {
		return
	}
	v = r.cooldowns[key]
	v.updated = now
	if v.seen == nil {
		v.seen = make(map[int64]time.Time)
	}
	for id, t := range v.seen {
		if now.Sub(t) > 15*time.Second {
			delete(v.seen, id)
		}
	}
	// Three distinct accounts in the same authorized group/model are evidence
	// for a short shared cooldown, not a permanent account disable.
	if _, exists := v.seen[account]; exists || len(v.seen) < 16 {
		v.seen[account] = now
	}
	if len(v.seen) >= 3 && now.Add(delay).After(v.until) {
		v.until = now.Add(delay)
	}
	r.cooldowns[key] = v
}
func safeCodexModel(model string) string {
	if len(model) > 100 {
		return "<other>"
	}
	for _, c := range model {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._:/", c)) {
			return "<other>"
		}
	}
	return model
}

func (s *OpenAIGatewayService) beginCodexAttempt(ctx context.Context, account *Account, model, kind string, h http.Header) (func(int, error), error) {
	r := codexControl(ctx)
	p := account.codexRequestPolicy()
	if r == nil || account == nil || !account.IsOpenAIOAuthLike() {
		return func(int, error) {}, nil
	}
	configureCodexControl(ctx, account)
	r.mu.Lock()
	if !r.configured {
		r.mu.Unlock()
		return func(int, error) {}, nil
	}
	p = r.policy
	model = safeCodexModel(model)
	if err := r.parent.Err(); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	now := time.Now()
	remaining := time.Duration(p.RetryWindowSeconds)*time.Second - now.Sub(r.started)
	if r.attempts >= p.MaxAttempts || remaining <= 0 {
		slog.Info("codex_request_stopped", "route_request_id", r.id, "turn", r.turn, "reason", "codex_retry_budget_exhausted", "attempts", r.attempts)
		r.mu.Unlock()
		return nil, codexControlStop("codex_retry_budget_exhausted", time.Second)
	}
	group := r.group
	r.mu.Unlock()
	waitStarted := time.Now()
	for {
		delay := s.codexControls().cooldown(group, model, account.ID, time.Now())
		if delay <= 0 {
			break
		}
		if delay >= remaining-time.Since(waitStarted) {
			return nil, codexControlStop("codex_upstream_cooldown", delay)
		}
		delay += time.Duration(rand.Intn(100)) * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-r.parent.Done():
			timer.Stop()
			return nil, r.parent.Err()
		case <-timer.C:
		}
	}
	delay := time.Since(waitStarted)
	r.mu.Lock()
	if err := r.parent.Err(); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	if r.attempts >= p.MaxAttempts || time.Since(r.started) >= time.Duration(p.RetryWindowSeconds)*time.Second {
		r.mu.Unlock()
		return nil, codexControlStop("codex_retry_budget_exhausted", time.Second)
	}
	r.attempts++
	r.total++
	r.wait += delay
	r.totalWait += delay
	attempt, turn, id := r.attempts, r.turn, r.id
	previous := r.lastAccount
	r.lastAccount = account.ID
	r.lastModel = model
	r.lastFailure = ""
	r.mu.Unlock()
	source := codexCapacityFromContext(ctx)
	fields := []any{"route_request_id", id, "turn", turn, "attempt", attempt, "account_id", account.ID, "previous_account_id", previous, "model", model, "transport", kind, "wait_ms", delay.Milliseconds(), "identity_mode", account.codexRequestPolicy().IdentityMode, "capacity_mode", account.codexRequestPolicy().CapacityMode, "fingerprint_mode", string(account.GetCodexFingerprintMode()), "account_concurrency_limit", account.Concurrency}
	if source != nil {
		fields = append(fields, codexHeaderDiagnostics(source.headers, h)...)
	}
	slog.Info("codex_upstream_attempt", fields...)
	start := time.Now()
	return func(status int, err error) {
		outcome := "headers_received"
		if err != nil {
			outcome = "error"
		}
		if status == 0 && err == nil {
			outcome = "completed"
		}
		slog.Info("codex_upstream_result", "route_request_id", id, "turn", turn, "attempt", attempt, "account_id", account.ID, "model", model, "transport", kind, "status", status, "outcome", outcome, "duration_ms", time.Since(start).Milliseconds())
	}, nil
}

func (s *OpenAIGatewayService) noteCodexOverload(ctx context.Context, account *Account, model string, status int, h http.Header, err error) {
	r := codexControl(ctx)
	if r == nil || account == nil {
		return
	}
	var failure *UpstreamFailoverError
	overloaded := status == 503
	if errors.As(err, &failure) && failure.Reason != CodexRequestControlReason {
		overloaded = overloaded || failure.StatusCode == 503 || failure.IsOpenAICapacityShed()
		if h == nil {
			h = failure.ResponseHeaders
		}
	}
	if !overloaded {
		return
	}
	r.mu.Lock()
	enabled, p, group := r.configured, r.policy, r.group
	r.mu.Unlock()
	if !enabled {
		return
	}
	delay := time.Duration(p.CooldownSeconds) * time.Second
	if raw := h.Get("Retry-After"); raw != "" {
		if n, e := strconv.ParseInt(raw, 10, 32); e == nil && n > 0 {
			delay = codexLongerDuration(delay, time.Duration(n)*time.Second)
		} else if at, e := http.ParseTime(raw); e == nil {
			delay = codexLongerDuration(delay, time.Until(at))
		}
	}
	s.codexControls().overload(group, safeCodexModel(model), account.ID, delay, time.Now())
}

func codexAttestationState(h http.Header) string {
	raw := h.Get("x-oai-attestation")
	if raw == "" {
		return "absent"
	}
	if len(raw) > 16384 {
		return "oversize"
	}
	var envelope struct {
		V int    `json:"v"`
		S *int   `json:"s"`
		T string `json:"t"`
	}
	if json.Unmarshal([]byte(raw), &envelope) != nil || envelope.V != 1 || envelope.S == nil {
		return "unrecognized"
	}
	switch *envelope.S {
	case 0:
		if envelope.T != "" {
			return "token_returned"
		}
		return "missing_token"
	case 1:
		return "timeout"
	case 2:
		return "generation_failed"
	case 3:
		return "canceled"
	case 4:
		return "malformed_response"
	}
	return "unrecognized"
}
func codexHeaderDiagnostics(in, out http.Header) []any {
	changed := []string{}
	for _, k := range []string{"user-agent", "originator", "version", "session-id", "session_id", "thread-id", "conversation_id", "x-client-request-id", "x-codex-installation-id", "x-codex-turn-state", "x-codex-turn-metadata", "x-codex-window-id", "x-oai-attestation"} {
		if in.Get(k) != out.Get(k) {
			changed = append(changed, k)
		}
	}
	return []any{"changed_headers", changed, "attestation_in", codexAttestationState(in), "attestation_out", codexAttestationState(out), "turn_state_in", in.Get("x-codex-turn-state") != "", "turn_state_out", out.Get("x-codex-turn-state") != "", "synthetic_subagent", out.Get("x-openai-subagent") == "collab_spawn"}
}

func codexRequestModel(h http.Header) string {
	hint := h.Get(openAICodexRoutingHintHeader)
	model, _, _ := strings.Cut(strings.TrimPrefix(hint, "model="), ";")
	return model
}

func responseHeader(r *http.Response) http.Header {
	if r == nil {
		return nil
	}
	return r.Header
}

// Forward result catches stream-level overloads (including HTTP 200 failures).
func (s *OpenAIGatewayService) finishCodexForward(ctx context.Context, account *Account, body []byte, result *OpenAIForwardResult, err error) {
	if account == nil {
		return
	}
	model := gjson.GetBytes(body, "model").String()
	if result != nil && result.UpstreamModel != "" {
		model = result.UpstreamModel
	}
	if r := codexControl(ctx); r != nil {
		r.mu.Lock()
		if r.lastAccount == account.ID && r.lastModel != "" {
			model = r.lastModel
		}
		r.mu.Unlock()
	}
	s.noteCodexOverload(ctx, account, model, 0, nil, err)
	if r := codexControl(ctx); r != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.configured {
			success := err == nil && result != nil && result.SucceededForScheduling()
			slog.Info("codex_forward_result", "route_request_id", r.id, "turn", r.turn, "account_id", account.ID, "success", success, "failure_code", codexForwardFailureCode(r, result, err), "upstream_attempts", r.attempts, "input_tokens", codexResultUsage(result).InputTokens, "output_tokens", codexResultUsage(result).OutputTokens, "cache_read_tokens", codexResultUsage(result).CacheReadInputTokens)
		}
	}
}

func codexLongerDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func configureCodexControl(ctx context.Context, account *Account) {
	r := codexControl(ctx)
	p := account.codexRequestPolicy()
	if r == nil || !p.Enabled {
		return
	}
	r.mu.Lock()
	first := !r.configured
	if first {
		r.configured = true
		r.policy = p
	}
	id := r.id
	r.mu.Unlock()
	if first {
		logger.FromContext(ctx).Info("codex request context", zap.String("route_request_id", id))
	}
}
func codexResultUsage(r *OpenAIForwardResult) OpenAIUsage {
	if r == nil {
		return OpenAIUsage{}
	}
	return r.Usage
}
func codexFailureCode(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.Canceled) {
		return "client_canceled"
	}
	var e *UpstreamFailoverError
	if errors.As(err, &e) {
		if e.Reason == CodexRequestControlReason {
			return "request_control"
		}
		if e.IsOpenAICapacityShed() {
			return "server_is_overloaded"
		}
		if e.IsCredentialFailure() {
			return "credential_failure"
		}
		switch e.StatusCode {
		case 429:
			return "rate_limited"
		case 503:
			return "service_unavailable"
		case 502:
			return "bad_gateway"
		}
	}
	return "other_error"
}
func (s *OpenAIGatewayService) beginCodexDial(ctx context.Context, account *Account, h http.Header) (func(int, http.Header, error), error) {
	model := codexRequestModel(h)
	done, err := s.beginCodexAttempt(ctx, account, model, "ws_dial", h)
	if err != nil {
		return nil, err
	}
	return func(status int, headers http.Header, err error) {
		done(status, err)
		s.noteCodexOverload(ctx, account, model, status, headers, err)
	}, nil
}

func codexControlIdentityHeader(a *Account, key string) bool {
	if !a.codexRequestPolicy().Enabled {
		return false
	}
	return key == "session-id" || key == "thread-id" || key == "x-client-request-id"
}
func codexControlTurnBase(ctx context.Context) int {
	if r := codexControl(ctx); r != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.turn - 1
	}
	return 0
}
func codexForwardFailureCode(r *codexRequestControl, result *OpenAIForwardResult, err error) string {
	if err != nil {
		if r.lastFailure != "" && !isCodexControlStop(err) && !errors.Is(err, context.Canceled) {
			return r.lastFailure
		}
		return codexFailureCode(err)
	}
	if result != nil && !result.SucceededForScheduling() {
		if r.lastFailure != "" {
			return r.lastFailure
		}
		return "stream_failed"
	}
	return "none"
}
func (s *OpenAIGatewayService) observeCodexStreamFailure(ctx context.Context, account *Account, model string, h http.Header, payload []byte) {
	if !isOpenAIUpstreamCapacityShedEvent(payload) {
		return
	}
	if r := codexControl(ctx); r != nil {
		r.mu.Lock()
		r.lastFailure = "server_is_overloaded"
		if account != nil && r.lastAccount == account.ID && r.lastModel != "" {
			model = r.lastModel
		}
		r.mu.Unlock()
	}
	s.noteCodexOverload(ctx, account, model, 503, h, nil)
}
