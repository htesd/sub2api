package service

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// CodexSessionCapacityPolicy counts recently exposed roots, not HTTP requests.
// The registry is deliberately process-local; an API restart clears exposure.
type CodexSessionCapacityPolicy struct {
	MaxRootSessions         int  `json:"max_root_sessions"`
	WindowSeconds           int  `json:"window_seconds"`
	SubagentFallbackEnabled bool `json:"subagent_fallback_enabled"`
	MaxChildrenPerRoot      int  `json:"max_children_per_root"`
	MaxNewChildrenPerWindow int  `json:"max_new_children_per_window"`
}

// 默认不做根会话准入：身份投影层（确定性命名空间 + turn-state 归属）总是生效，
// 根上限属于未经上游验证的策略层，必须由管理员显式配置 max_root_sessions>0 才启用。
// 对齐 llm-access 的默认值（0 = 关闭准入）。
func defaultCodexCapacityPolicy() CodexSessionCapacityPolicy {
	return CodexSessionCapacityPolicy{0, 600, true, 8, 32}
}

func (a *Account) codexCapacityPolicy() (CodexSessionCapacityPolicy, bool) {
	p := defaultCodexCapacityPolicy()
	if a == nil || a.GetCodexFingerprintMode() != codexFingerprintCapacity {
		return p, false
	}
	if extra, ok := a.Extra["codex_session_capacity"].(map[string]any); ok {
		for k, dest := range map[string]*int{"max_root_sessions": &p.MaxRootSessions, "window_seconds": &p.WindowSeconds, "max_children_per_root": &p.MaxChildrenPerRoot, "max_new_children_per_window": &p.MaxNewChildrenPerWindow} {
			if raw, exists := extra[k]; exists {
				*dest = parseExtraInt(raw)
			}
		}
		if v, ok := extra["subagent_fallback_enabled"].(bool); ok {
			p.SubagentFallbackEnabled = v
		}
	}
	// Invalid configuration fails closed at admission; it must not silently disable limits.
	return p, true
}

func (p CodexSessionCapacityPolicy) valid() bool {
	return p.MaxRootSessions >= 0 && p.MaxRootSessions <= 100000 && p.WindowSeconds > 0 && p.WindowSeconds <= 86400 && p.MaxChildrenPerRoot > 0 && p.MaxChildrenPerRoot <= 10000 && p.MaxNewChildrenPerWindow > 0 && p.MaxNewChildrenPerWindow <= 100000
}

// admissionEnabled 区分两层：false 时只做身份投影与归属记账，不限制根会话数量，
// 因此不会因为本地容量把客户端请求拒掉。
func (p CodexSessionCapacityPolicy) admissionEnabled() bool { return p.MaxRootSessions > 0 }

type CodexSessionCapacityError struct {
	Code       string
	Status     int
	RetryAfter int
}

func (e *CodexSessionCapacityError) Error() string { return e.Code }

// ClientMessage 给需要客户端改动的错误一句可执行的说明；其余保持错误码原文。
func (e *CodexSessionCapacityError) ClientMessage() string {
	switch e.Code {
	case "session_identity_required":
		return "Session-capacity routing could not resolve a conversation identity. Send a stable session-id header, client_metadata.session_id, or prompt_cache_key; reuse it across turns and retries of the same conversation."
	case "session_continuation_requires_websocket":
		return "This account does not accept previous_response_id over HTTP. Resend the full conversation history, or use the WebSocket transport."
	case "session_capacity_requires_responses":
		return "Session-capacity accounts only serve /v1/responses and /v1/chat/completions; this endpoint needs a compatible account."
	}
	return e.Code
}
func AsCodexSessionCapacityError(err error) *CodexSessionCapacityError {
	var e *CodexSessionCapacityError
	errors.As(err, &e)
	return e
}
func capacityError(code string, status int, wait int) *CodexSessionCapacityError {
	wait = max(1, wait)
	// 400 是请求本身要改，等待不会让它变合法，不能给重试提示。
	if status == http.StatusBadRequest {
		wait = 0
	}
	return &CodexSessionCapacityError{Code: code, Status: status, RetryAfter: wait}
}

type codexCapacityInput struct {
	Key                            int64
	Root, Thread, Parent, Previous string
	Mountable                      bool
	// Ephemeral：客户端没有声明任何会话标识，身份是本次请求合成的。这种请求
	// 不可能续接，也不代表一个可复用的会话，因此只在准入关闭时允许。
	Ephemeral bool
}
type codexCapacityAssignment struct {
	SessionID      string `json:"session_id"`
	ThreadID       string `json:"thread_id"`
	ParentThreadID string `json:"parent_thread_id,omitempty"`
	Synthetic      bool   `json:"synthetic_subagent"`
}
type codexCapacityBindingKey struct {
	Key    int64
	Thread string
}
type codexCapacityBinding struct {
	Assignment codexCapacityAssignment
	LastSent   time.Time
	Leases     int
	Responses  map[string]time.Time
	Ephemeral  bool
}
type codexCapacityRoot struct {
	Key      int64
	Thread   string
	LastSent time.Time
}
type codexCapacityChildStart struct {
	Key int64
	At  time.Time
}
type codexCapacityAccount struct {
	Roots       map[string]codexCapacityRoot
	Bindings    map[codexCapacityBindingKey]*codexCapacityBinding
	NewChildren []codexCapacityChildStart
}
type codexCapacityRegistry struct {
	mu         sync.Mutex
	accounts   map[string]*codexCapacityAccount
	turnOwners map[[32]byte]string
	turnOrder  [][32]byte
	turnNext   int
}

const codexCapacityRetention = 24 * time.Hour
const codexCapacityEphemeralRetention = time.Hour
const codexCapacityMaxBindings = 10000
const codexCapacityMaxResponses = 256

// codexCapacityForeignIdentity 是一个不可能等于任何真实凭据 namespace 的哨兵，
// 用于把归属未知的 turn-state 判为外来。
const codexCapacityForeignIdentity = "\x00foreign"

func newCodexCapacityRegistry() *codexCapacityRegistry {
	return &codexCapacityRegistry{accounts: make(map[string]*codexCapacityAccount)}
}
func (s *OpenAIGatewayService) codexCapacityRegistry() *codexCapacityRegistry {
	s.codexCapacityOnce.Do(func() { s.codexCapacity = newCodexCapacityRegistry() })
	return s.codexCapacity
}
func codexCapacityNode(account string, key int64, node string) string {
	// Length-prefixed namespace prevents delimiter ambiguity across client IDs.
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("sub2api:codex-capacity:v1:%d:%s:%d:%d:%s", len(account), account, key, len(node), node))).String()
}
func (a *codexCapacityAccount) prune(now time.Time) {
	for k, b := range a.Bindings {
		if b.Leases > 0 {
			continue
		}
		retention := codexCapacityRetention
		if b.Ephemeral {
			// 合成身份无法续接，留着只会白占绑定表和内存。
			retention = codexCapacityEphemeralRetention
		}
		if b.LastSent.IsZero() || now.Sub(b.LastSent) >= retention {
			delete(a.Bindings, k)
		}
	}
	for k, r := range a.Roots {
		if now.Sub(r.LastSent) >= codexCapacityRetention {
			delete(a.Roots, k)
		}
	}
	kept := a.NewChildren[:0]
	for _, child := range a.NewChildren {
		if now.Sub(child.At) < codexCapacityRetention {
			kept = append(kept, child)
		}
	}
	a.NewChildren = append([]codexCapacityChildStart(nil), kept...)
}

// activeRoots 只统计同一 (凭据, api key) 维度的根：宿主挂载也限制在同 key 内，
// 两者必须同域，否则一个 key 用满上限会让其它 key 既拿不到新根也找不到宿主。
// 活跃租约在窗口内视为占位；空闲超过窗口的长连接不再永久占用根位，它下一轮
// 由 plan 的既有会话豁免放行。
func (a *codexCapacityAccount) activeRoots(p CodexSessionCapacityPolicy, now time.Time, key int64) map[string]bool {
	window := time.Duration(p.WindowSeconds) * time.Second
	roots := make(map[string]bool)
	for id, r := range a.Roots {
		if r.Key == key && now.Sub(r.LastSent) < window {
			roots[id] = true
		}
	}
	for k, b := range a.Bindings {
		if k.Key != key || b.Leases == 0 {
			continue
		}
		if b.LastSent.IsZero() || now.Sub(b.LastSent) < window {
			roots[b.Assignment.SessionID] = true
		}
	}
	return roots
}

// rootsFullWait 返回最早有根位空出的秒数，避免统一回一个整窗口的 Retry-After。
func (a *codexCapacityAccount) rootsFullWait(p CodexSessionCapacityPolicy, now time.Time, key int64, active map[string]bool) int {
	window := time.Duration(p.WindowSeconds) * time.Second
	wait := window
	found := false
	for id, r := range a.Roots {
		if r.Key != key || !active[id] {
			continue
		}
		if remaining := window - now.Sub(r.LastSent); remaining < wait {
			wait, found = remaining, true
		}
	}
	if !found {
		return p.WindowSeconds
	}
	return max(1, int(math.Ceil(wait.Seconds())))
}

// evictIdleBindingLocked 淘汰最旧的空闲绑定；活跃租约永不淘汰。
func (a *codexCapacityAccount) evictIdleBindingLocked() bool {
	var victim codexCapacityBindingKey
	var oldest time.Time
	found := false
	for k, b := range a.Bindings {
		if b.Leases > 0 {
			continue
		}
		if !found || b.LastSent.Before(oldest) {
			victim, oldest, found = k, b.LastSent, true
		}
	}
	if found {
		delete(a.Bindings, victim)
	}
	return found
}

func capacityChildActive(b *codexCapacityBinding, p CodexSessionCapacityPolicy, now time.Time) bool {
	return b.Leases > 0 || !b.LastSent.IsZero() && now.Sub(b.LastSent) < time.Duration(p.WindowSeconds)*time.Second
}
func (a *codexCapacityAccount) plan(identity string, p CodexSessionCapacityPolicy, in codexCapacityInput, overflow bool, now time.Time) (codexCapacityAssignment, error) {
	zero := codexCapacityAssignment{}
	if !p.valid() {
		return zero, capacityError("session_capacity_invalid_policy", 503, 1)
	}
	if identity == "" || in.Key <= 0 || in.Root == "" || in.Thread == "" {
		return zero, capacityError("session_identity_required", 400, 1)
	}
	// 准入开启时上限才有意义，此时必须要求客户端给稳定标识；准入关闭时合成
	// 身份只用于投影隔离，不占任何配额，没有理由把请求拒掉。
	if in.Ephemeral && p.admissionEnabled() {
		return zero, capacityError("session_identity_required", 400, 1)
	}
	key := codexCapacityBindingKey{in.Key, in.Thread}
	existing := a.Bindings[key]
	if in.Previous != "" {
		if existing == nil || existing.LastSent.IsZero() {
			return zero, capacityError("session_continuation_unbound", 409, 1)
		}
		seen, ok := existing.Responses[in.Previous]
		if !ok || now.Sub(seen) >= codexCapacityRetention {
			return zero, capacityError("session_continuation_unbound", 409, 1)
		}
	}
	active := a.activeRoots(p, now, in.Key)
	out := codexCapacityAssignment{SessionID: codexCapacityNode(identity, in.Key, in.Root), ThreadID: codexCapacityNode(identity, in.Key, in.Thread)}
	if in.Parent != "" {
		out.ParentThreadID = codexCapacityNode(identity, in.Key, in.Parent)
	}
	if family := a.Bindings[codexCapacityBindingKey{in.Key, in.Parent}]; in.Parent != "" && family != nil {
		out.SessionID = family.Assignment.SessionID
		out.ParentThreadID = family.Assignment.ThreadID
	} else if family := a.Bindings[codexCapacityBindingKey{in.Key, in.Root}]; family != nil {
		out.SessionID = family.Assignment.SessionID
	}
	if existing != nil {
		out = existing.Assignment
	}
	if existing == nil && len(a.Bindings) >= codexCapacityMaxBindings {
		// 绑定表是内存上界，不是策略上限：淘汰最旧的空闲绑定，只有全部在租约中
		// 才拒绝。否则一天内积累到上界后所有新会话都会被硬拒到重启为止。
		if !a.evictIdleBindingLocked() {
			return zero, capacityError("session_bindings_full", 429, p.WindowSeconds)
		}
	}
	// 身份投影层不做数量限制：准入关闭时到此为止，后面全是根/子会话配额。
	if !p.admissionEnabled() {
		return out, nil
	}
	// 已曝光的既有会话不再受新根准入限制：上限只约束新根暴露，不能把正在进行
	// 的对话在第二轮打成 429（空闲长连接让出根位后仍然要能继续说话）。
	if existing != nil && !existing.LastSent.IsZero() {
		return out, nil
	}
	children := func(root string) int {
		n := 0
		for _, b := range a.Bindings {
			if b.Assignment.Synthetic && b.Assignment.SessionID == root && capacityChildActive(b, p, now) {
				n++
			}
		}
		return n
	}
	if active[out.SessionID] || len(active) < p.MaxRootSessions {
		if existing != nil && out.Synthetic && !capacityChildActive(existing, p, now) && children(out.SessionID) >= p.MaxChildrenPerRoot {
			return zero, capacityError("session_children_full", 429, a.rootsFullWait(p, now, in.Key, active))
		}
		return out, nil
	}
	if existing != nil || in.Previous != "" || in.Parent != "" || !in.Mountable || !overflow || !p.SubagentFallbackEnabled {
		return zero, capacityError("session_roots_full", 429, a.rootsFullWait(p, now, in.Key, active))
	}
	created := 0
	for _, child := range a.NewChildren {
		if child.Key == in.Key && now.Sub(child.At) < time.Duration(p.WindowSeconds)*time.Second {
			created++
		}
	}
	for k, b := range a.Bindings {
		if k.Key == in.Key && b.Assignment.Synthetic && b.LastSent.IsZero() {
			created++
		}
	}
	if created >= p.MaxNewChildrenPerWindow {
		return zero, capacityError("session_new_children_limit", 429, a.rootsFullWait(p, now, in.Key, active))
	}
	host := ""
	bestChildren := int(^uint(0) >> 1)
	var oldest time.Time
	for id, r := range a.Roots {
		if r.Key != in.Key || !active[id] {
			continue
		}
		n := children(id)
		if n >= p.MaxChildrenPerRoot {
			continue
		}
		if host == "" || n < bestChildren || n == bestChildren && (r.LastSent.Before(oldest) || r.LastSent.Equal(oldest) && id < host) {
			host = id
			bestChildren = n
			oldest = r.LastSent
		}
	}
	if host == "" {
		return zero, capacityError("session_no_eligible_host", 429, a.rootsFullWait(p, now, in.Key, active))
	}
	out.SessionID = host
	out.ParentThreadID = a.Roots[host].Thread
	out.Synthetic = true
	return out, nil
}

type codexCapacityLease struct {
	registry         *codexCapacityRegistry
	identity         string
	previousIdentity string
	key              codexCapacityBindingKey
	binding          *codexCapacityBinding
	Assignment       codexCapacityAssignment
	once             sync.Once
	released         bool
}

func (r *codexCapacityRegistry) reserve(identity string, p CodexSessionCapacityPolicy, in codexCapacityInput, overflow bool, now time.Time) (*codexCapacityLease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	previousIdentity := r.bindingOwnerLocked(in, now)
	a := r.accounts[identity]
	if a == nil {
		a = &codexCapacityAccount{Roots: make(map[string]codexCapacityRoot), Bindings: make(map[codexCapacityBindingKey]*codexCapacityBinding)}
		r.accounts[identity] = a
	}
	a.prune(now)
	out, err := a.plan(identity, p, in, overflow, now)
	if err != nil {
		return nil, err
	}
	key := codexCapacityBindingKey{in.Key, in.Thread}
	b := a.Bindings[key]
	if b == nil {
		// in.Thread 可能是请求体零拷贝视图里的子串（只带 prompt_cache_key 的客户端），
		// 绑定要留 24 小时，必须存独立副本，否则整份请求体跟着被钉住。
		key.Thread = strings.Clone(key.Thread)
		b = &codexCapacityBinding{Assignment: out, Responses: make(map[string]time.Time), Ephemeral: in.Ephemeral}
		a.Bindings[key] = b
	}
	b.Leases++
	return &codexCapacityLease{registry: r, identity: identity, previousIdentity: previousIdentity, key: key, binding: b, Assignment: out}, nil
}
func (l *codexCapacityLease) release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.registry.mu.Lock()
		defer l.registry.mu.Unlock()
		l.released = true
		l.binding.Leases--
		// 合成身份用完即弃：它不可能被续接，留下来只会占绑定表。
		if l.binding.Leases == 0 && (l.binding.LastSent.IsZero() || l.binding.Ephemeral) {
			delete(l.registry.accounts[l.identity].Bindings, l.key)
		}
	})
}
func (l *codexCapacityLease) sent(now time.Time) {
	if l == nil {
		return
	}
	l.registry.mu.Lock()
	defer l.registry.mu.Unlock()
	if l.released {
		return
	}
	a := l.registry.accounts[l.identity]
	b := l.binding
	if b.LastSent.IsZero() && b.Assignment.Synthetic {
		a.NewChildren = append(a.NewChildren, codexCapacityChildStart{Key: l.key.Key, At: now})
	}
	if now.After(b.LastSent) {
		b.LastSent = now
	}
	root := a.Roots[b.Assignment.SessionID]
	root.Key = l.key.Key
	if now.After(root.LastSent) {
		root.LastSent = now
	}
	// 根线程首次写入即固定：同根的兄弟线程不能把它覆盖成自己的 thread，
	// 否则后续挂到该根的合成子会话会把 parent 指向兄弟线程而不是真正的根。
	if root.Thread == "" {
		if !b.Assignment.Synthetic && b.Assignment.ParentThreadID == "" {
			root.Thread = b.Assignment.ThreadID
		} else {
			root.Thread = b.Assignment.ParentThreadID
		}
	}
	a.Roots[b.Assignment.SessionID] = root
}
func (l *codexCapacityLease) rememberResponse(id string, now time.Time) {
	if l == nil || id == "" || len(id) > 256 {
		return
	}
	l.registry.mu.Lock()
	defer l.registry.mu.Unlock()
	b := l.binding
	for id, t := range b.Responses {
		if now.Sub(t) >= codexCapacityRetention {
			delete(b.Responses, id)
		}
	}
	if len(b.Responses) >= codexCapacityMaxResponses {
		var oldest string
		var at time.Time
		for id, t := range b.Responses {
			if oldest == "" || t.Before(at) {
				oldest = id
				at = t
			}
		}
		delete(b.Responses, oldest)
	}
	// id 来自 gjson 解析回包，是整份 response.completed 的子串；不复制就会让每个
	// 回包（几十 KB 到几 MB）陪着这条记录留 24 小时。
	b.Responses[strings.Clone(id)] = now
}

// codexCapacityOccupancy 是准入决策的可观测快照，用于把"为什么被拒"落到日志里。
type codexCapacityOccupancy struct {
	Roots       int
	RootLimit   int
	Bindings    int
	NewChildren int
}

func (r *codexCapacityRegistry) occupancy(identity string, key int64, p CodexSessionCapacityPolicy, now time.Time) codexCapacityOccupancy {
	out := codexCapacityOccupancy{RootLimit: p.MaxRootSessions}
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.accounts[identity]
	if a == nil {
		return out
	}
	out.Roots = len(a.activeRoots(p, now, key))
	out.Bindings = len(a.Bindings)
	window := time.Duration(p.WindowSeconds) * time.Second
	for _, child := range a.NewChildren {
		if child.Key == key && now.Sub(child.At) < window {
			out.NewChildren++
		}
	}
	return out
}

func (r *codexCapacityRegistry) responseOwner(in codexCapacityInput, now time.Time) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, a := range r.accounts {
		if b := a.Bindings[codexCapacityBindingKey{in.Key, in.Thread}]; b != nil {
			if at, ok := b.Responses[in.Previous]; ok && now.Sub(at) < codexCapacityRetention {
				return id
			}
		}
	}
	return ""
}

func (r *codexCapacityRegistry) bindingOwnerLocked(in codexCapacityInput, now time.Time) string {
	for _, thread := range []string{in.Thread, in.Parent, in.Root} {
		if thread == "" {
			continue
		}
		owner := ""
		var latest time.Time
		for id, a := range r.accounts {
			if b := a.Bindings[codexCapacityBindingKey{in.Key, thread}]; b != nil && !b.LastSent.IsZero() && now.Sub(b.LastSent) < codexCapacityRetention {
				if owner == "" || b.LastSent.After(latest) || b.LastSent.Equal(latest) && id < owner {
					owner, latest = id, b.LastSent
				}
			}
		}
		if owner != "" {
			return owner
		}
	}
	return ""
}

func (r *codexCapacityRegistry) boundOwner(in codexCapacityInput) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bindingOwnerLocked(in, time.Now())
}

// Inspect known response IDs before any ordinary-account fallback. A changed
// thread/key must not turn a capacity continuation into an unbound new request.
func (r *codexCapacityRegistry) continuationOwner(in codexCapacityInput, now time.Time) (string, error) {
	if in.Previous == "" {
		return "", nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, a := range r.accounts {
		for key, b := range a.Bindings {
			if at, ok := b.Responses[in.Previous]; ok && now.Sub(at) < codexCapacityRetention {
				if key.Key != in.Key || key.Thread != in.Thread {
					return "", capacityError("session_continuation_owner_mismatch", 409, 1)
				}
				return id, nil
			}
		}
	}
	return "", nil
}
