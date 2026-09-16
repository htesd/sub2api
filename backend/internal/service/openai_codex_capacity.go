package service

import (
	"errors"
	"fmt"
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

func defaultCodexCapacityPolicy() CodexSessionCapacityPolicy {
	return CodexSessionCapacityPolicy{5, 600, true, 8, 32}
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
	return p.MaxRootSessions > 0 && p.MaxRootSessions <= 100000 && p.WindowSeconds > 0 && p.WindowSeconds <= 86400 && p.MaxChildrenPerRoot > 0 && p.MaxChildrenPerRoot <= 10000 && p.MaxNewChildrenPerWindow > 0 && p.MaxNewChildrenPerWindow <= 100000
}

type CodexSessionCapacityError struct {
	Code       string
	Status     int
	RetryAfter int
}

func (e *CodexSessionCapacityError) Error() string { return e.Code }
func AsCodexSessionCapacityError(err error) *CodexSessionCapacityError {
	var e *CodexSessionCapacityError
	errors.As(err, &e)
	return e
}
func capacityError(code string, status int, wait int) *CodexSessionCapacityError {
	return &CodexSessionCapacityError{code, status, max(1, wait)}
}

type codexCapacityInput struct {
	Key                            int64
	Root, Thread, Parent, Previous string
	Mountable                      bool
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
}
type codexCapacityRoot struct {
	Key      int64
	Thread   string
	LastSent time.Time
}
type codexCapacityAccount struct {
	Roots       map[string]codexCapacityRoot
	Bindings    map[codexCapacityBindingKey]*codexCapacityBinding
	NewChildren []time.Time
}
type codexCapacityRegistry struct {
	mu         sync.Mutex
	accounts   map[string]*codexCapacityAccount
	turnOwners map[[32]byte]string
	turnOrder  [][32]byte
	turnNext   int
}

const codexCapacityRetention = 24 * time.Hour
const codexCapacityMaxBindings = 10000
const codexCapacityMaxResponses = 256

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
		if b.Leases == 0 && (b.LastSent.IsZero() || now.Sub(b.LastSent) >= codexCapacityRetention) {
			delete(a.Bindings, k)
		}
	}
	for k, r := range a.Roots {
		if now.Sub(r.LastSent) >= codexCapacityRetention {
			delete(a.Roots, k)
		}
	}
	n := 0
	for n < len(a.NewChildren) && now.Sub(a.NewChildren[n]) >= codexCapacityRetention {
		n++
	}
	a.NewChildren = append([]time.Time(nil), a.NewChildren[n:]...)
}
func (a *codexCapacityAccount) activeRoots(p CodexSessionCapacityPolicy, now time.Time) map[string]bool {
	roots := make(map[string]bool)
	for id, r := range a.Roots {
		if now.Sub(r.LastSent) < time.Duration(p.WindowSeconds)*time.Second {
			roots[id] = true
		}
	}
	for _, b := range a.Bindings {
		if b.Leases > 0 {
			roots[b.Assignment.SessionID] = true
		}
	}
	return roots
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
	active := a.activeRoots(p, now)
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
		return zero, capacityError("session_bindings_full", 429, p.WindowSeconds)
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
			return zero, capacityError("session_children_full", 429, p.WindowSeconds)
		}
		return out, nil
	}
	if existing != nil || in.Previous != "" || in.Parent != "" || !in.Mountable || !overflow || !p.SubagentFallbackEnabled {
		return zero, capacityError("session_roots_full", 429, p.WindowSeconds)
	}
	created := 0
	for _, at := range a.NewChildren {
		if now.Sub(at) < time.Duration(p.WindowSeconds)*time.Second {
			created++
		}
	}
	for _, b := range a.Bindings {
		if b.Assignment.Synthetic && b.LastSent.IsZero() {
			created++
		}
	}
	if created >= p.MaxNewChildrenPerWindow {
		return zero, capacityError("session_new_children_limit", 429, p.WindowSeconds)
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
		return zero, capacityError("session_no_eligible_host", 429, p.WindowSeconds)
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
		b = &codexCapacityBinding{Assignment: out, Responses: make(map[string]time.Time)}
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
		if l.binding.Leases == 0 && l.binding.LastSent.IsZero() {
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
		a.NewChildren = append(a.NewChildren, now)
	}
	if now.After(b.LastSent) {
		b.LastSent = now
	}
	root := a.Roots[b.Assignment.SessionID]
	root.Key = l.key.Key
	if now.After(root.LastSent) {
		root.LastSent = now
	}
	if !b.Assignment.Synthetic && b.Assignment.ParentThreadID == "" {
		root.Thread = b.Assignment.ThreadID
	}
	if root.Thread == "" {
		root.Thread = b.Assignment.ParentThreadID
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
	b.Responses[id] = now
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
