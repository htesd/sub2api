package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type codexCapacityContextKey struct{}

const codexCapacityGinKey = "codex_capacity_request"

type codexCapacityRequest struct {
	mu               sync.Mutex
	input            codexCapacityInput
	headers          http.Header
	originalTurn     map[string]any
	originalMetadata map[string]any
	originalCache    string
	supported        bool
	websocket        bool
	lease            *codexCapacityLease
	release          func()
	accountID        int64
	restore          map[string]string
}

func parseCodexCapacityInput(c *gin.Context, body []byte) *codexCapacityRequest {
	h := c.Request.Header.Clone()
	root := openAIRequestPayloadView(body)
	cm := map[string]any{}
	_ = json.Unmarshal([]byte(root.Get("client_metadata").Raw), &cm)
	turn := map[string]any{}
	raw := root.Get("client_metadata.x-codex-turn-metadata").String()
	if raw == "" {
		raw = h.Get("x-codex-turn-metadata")
	}
	_ = json.Unmarshal([]byte(raw), &turn)
	str := func(m map[string]any, k string) string { v, _ := m[k].(string); return strings.TrimSpace(v) }
	first := func(values ...string) string {
		for _, v := range values {
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
		return ""
	}
	p := c.Request.URL.Path
	chatCompat := strings.HasSuffix(p, "/chat/completions")
	headerSession := first(h.Get("session-id"), h.Get("session_id"))
	if chatCompat {
		headerSession = explicitOpenAIHeaderSessionID(c)
	}
	session := first(str(cm, "session_id"), str(turn, "session_id"), headerSession, root.Get("prompt_cache_key").String())
	thread := first(str(cm, "thread_id"), str(turn, "thread_id"), h.Get("thread-id"), session)
	parent := first(str(cm, "x-codex-parent-thread-id"), str(turn, "parent_thread_id"), h.Get("x-codex-parent-thread-id"))
	responses := strings.HasSuffix(p, "/responses") || chatCompat
	previous := root.Get("previous_response_id").String()
	mountable := responses && parent == "" && str(cm, "x-openai-subagent") == "" && h.Get("x-openai-subagent") == "" && str(turn, "subagent_kind") == ""
	if chatCompat && session == "" && mountable && previous == "" {
		// Stateless Chat Completions has no required conversation identifier.
		// Count each such inbound request independently; never equate matching
		// prompts with one conversation. This state survives internal retries.
		session = thread
		if session == "" {
			session = "cc_request_" + uuid.NewString()
		}
		if thread == "" {
			thread = session
		}
	}
	return &codexCapacityRequest{input: codexCapacityInput{Key: getAPIKeyIDFromContext(c), Root: session, Thread: thread, Parent: parent, Previous: previous, Mountable: mountable}, headers: h, originalTurn: turn, originalMetadata: cm, originalCache: root.Get("prompt_cache_key").String(), supported: responses || strings.HasSuffix(p, "/responses/compact"), websocket: GetOpenAIClientTransport(c) == OpenAIClientTransportWS, restore: make(map[string]string)}
}

// WithCodexSessionCapacity captures client identity before any per-account transformation.
// It does not enable the feature; only accounts explicitly selecting capacity participate.
// The caller must defer ReleaseCodexSessionCapacity until forwarding/draining ends.
func WithCodexSessionCapacity(ctx context.Context, c *gin.Context, body []byte) context.Context {
	if c == nil || c.Request == nil {
		return ctx
	}
	state := parseCodexCapacityInput(c, body)
	c.Set(codexCapacityGinKey, state)
	return context.WithValue(ctx, codexCapacityContextKey{}, state)
}
func codexCapacityFromContext(ctx context.Context) *codexCapacityRequest {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(codexCapacityContextKey{}).(*codexCapacityRequest)
	return r
}
func codexCapacityFromGin(c *gin.Context) *codexCapacityRequest {
	if c == nil {
		return nil
	}
	v, _ := c.Get(codexCapacityGinKey)
	r, _ := v.(*codexCapacityRequest)
	return r
}
func (r *codexCapacityRequest) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.release != nil {
		r.release()
	}
	r.lease = nil
	r.release = nil
	r.accountID = 0
}
func (r *codexCapacityRequest) stage(accountID int64, l *codexCapacityLease, release func()) {
	r.clear()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lease = l
	r.release = release
	r.accountID = accountID
	r.restore = make(map[string]string)
	prior := l.previousIdentity
	if prior == "" {
		prior = l.identity
	}
	bodyToken, _ := r.originalMetadata[openAICodexTurnStateHeader].(string)
	for _, token := range []string{r.headers.Get(openAICodexTurnStateHeader), bodyToken} {
		l.registry.foreignTurnState(r.input.Key, token, l.identity, prior)
	}
}
func capacityLeaseForAccount(c *gin.Context, account *Account) *codexCapacityLease {
	r := codexCapacityFromGin(c)
	if r == nil || account == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.accountID != account.ID {
		return nil
	}
	return r.lease
}
func markCodexCapacitySent(ctx context.Context) {
	if r := codexCapacityFromContext(ctx); r != nil {
		r.mu.Lock()
		l := r.lease
		r.mu.Unlock()
		if l != nil {
			l.sent(time.Now())
		}
	}
}

// Every request frame remains in the admitted logical thread. Missing fields on
// incremental frames inherit the connection identity, never create a new root.
func validateCodexCapacityFrame(c *gin.Context, body []byte) error {
	r := codexCapacityFromGin(c)
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lease == nil {
		return nil
	}
	// Incremental frames inherit connection identity. The initial parser's
	// thread=session fallback must not overwrite a distinct admitted thread.
	frameContext := &gin.Context{Request: c.Request.Clone(c.Request.Context())}
	frameContext.Request.Header = make(http.Header)
	frame := parseCodexCapacityInput(frameContext, body)
	view := openAIRequestPayloadView(body)
	hasThread := view.Get("client_metadata.thread_id").String() != "" || gjson.Get(view.Get("client_metadata.x-codex-turn-metadata").String(), "thread_id").String() != ""
	if !hasThread {
		frame.input.Thread = r.input.Thread
	}
	if frame.input.Root == "" {
		frame.input.Root = r.input.Root
	}

	if frame.input.Thread != "" && frame.input.Thread != r.input.Thread {
		return capacityError("session_thread_changed", 409, 1)
	}
	if frame.input.Root != "" && frame.input.Root != r.input.Root {
		return capacityError("session_root_changed", 409, 1)
	}
	if frame.input.Previous != "" {
		in := r.input
		in.Previous = frame.input.Previous
		if r.lease.registry.responseOwner(in, time.Now()) != r.lease.identity {
			return capacityError("session_continuation_unbound", 409, 1)
		}
	}
	// Turn metadata is frame-scoped; omitted identity inherits the admitted thread.
	r.originalTurn = frame.originalTurn
	r.originalMetadata = frame.originalMetadata
	if frame.originalCache != "" {
		r.originalCache = frame.originalCache
	}
	return nil
}

// Apply the final assignment after legacy namespace and device normalization.
// Only typed protocol fields are edited; model text, tool arguments and encrypted
// checkpoints remain opaque.
func projectCodexCapacity(c *gin.Context, account *Account, body []byte, h http.Header) ([]byte, error) {
	r := codexCapacityFromGin(c)
	if r == nil {
		return body, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lease == nil || account == nil || r.accountID != account.ID {
		return body, nil
	}
	a := r.lease.Assignment
	identity := r.lease.identity
	owner := r.input.Key
	cm := map[string]any{}
	v := gjson.GetBytes(body, "client_metadata")
	if v.Exists() && !v.IsObject() {
		return nil, fmt.Errorf("invalid client_metadata")
	}
	if v.IsObject() {
		if err := json.Unmarshal([]byte(v.Raw), &cm); err != nil {
			return nil, err
		}
	}
	if token, _ := cm[openAICodexTurnStateHeader].(string); r.lease.registry.foreignTurnState(owner, token, identity, identity) {
		delete(cm, openAICodexTurnStateHeader)
	}
	if h != nil && r.lease.registry.foreignTurnState(owner, h.Get(openAICodexTurnStateHeader), identity, identity) {
		h.Del(openAICodexTurnStateHeader)
	}
	turn := map[string]any{}
	if len(body) == 0 && h != nil {
		if raw := h.Get("x-codex-turn-metadata"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &turn); err != nil || turn == nil {
				return nil, capacityError("invalid_turn_metadata", 400, 1)
			}
		}
	}
	if raw, _ := cm["x-codex-turn-metadata"].(string); raw != "" {
		if err := json.Unmarshal([]byte(raw), &turn); err != nil {
			return nil, err
		}
		if turn == nil {
			return nil, capacityError("invalid_turn_metadata", 400, 1)
		}
	}
	remember := func(field, mapped, original string) { r.restore[field+":"+mapped] = original }
	for _, x := range []struct{ field, value, original string }{{"session_id", a.SessionID, r.input.Root}, {"thread_id", a.ThreadID, r.input.Thread}} {
		cm[x.field] = x.value
		turn[x.field] = x.value
		remember(x.field, x.value, x.original)
	}
	if a.ParentThreadID != "" {
		cm["x-codex-parent-thread-id"] = a.ParentThreadID
		turn["parent_thread_id"] = a.ParentThreadID
		remember("parent_thread_id", a.ParentThreadID, r.input.Parent)
		remember("x-codex-parent-thread-id", a.ParentThreadID, r.input.Parent)
	} else {
		delete(cm, "x-codex-parent-thread-id")
		delete(turn, "parent_thread_id")
	}
	for _, field := range []string{"turn_id", "parent_turn_id", "root_turn_id", "forked_from_thread_id", "context_window_id", "guardian_classifier_source_thread_id"} {
		raw, _ := r.originalTurn[field].(string)
		if x, ok := r.originalMetadata[field].(string); ok {
			raw = x
		}
		if raw == "" {
			continue
		}
		mapped := codexCapacityNode(identity, owner, raw)
		turn[field] = mapped
		if _, ok := r.originalMetadata[field]; ok {
			cm[field] = mapped
		}
		remember(field, mapped, raw)
	}
	if a.Synthetic {
		cm["x-openai-subagent"] = "collab_spawn"
		turn["subagent_kind"] = "thread_spawn"
		turn["thread_source"] = "subagent"
		for _, f := range []string{"parent_turn_id", "root_turn_id", "forked_from_thread_id", "forked_from_ordinal_exclusive"} {
			delete(turn, f)
			delete(cm, f)
		}
	}
	windowRaw, _ := r.originalMetadata["x-codex-window-id"].(string)
	if windowRaw == "" {
		windowRaw, _ = r.originalTurn["window_id"].(string)
	}
	if windowRaw == "" {
		windowRaw = r.headers.Get("x-codex-window-id")
	}
	if windowRaw != "" {
		mapped := codexCapacityNode(identity, owner, windowRaw)
		if base, n, ok := strings.Cut(windowRaw, ":"); ok && base == r.input.Thread {
			if _, err := strconv.ParseUint(n, 10, 64); err == nil {
				mapped = a.ThreadID + ":" + n
			}
		}
		cm["x-codex-window-id"] = mapped
		turn["window_id"] = mapped
		remember("window_id", mapped, windowRaw)
		remember("x-codex-window-id", mapped, windowRaw)
	}
	cache := a.SessionID
	if r.originalCache != "" && r.originalCache != r.input.Root {
		cache = codexCapacityNode(identity, owner, "cache:"+r.originalCache)
	}
	remember("prompt_cache_key", cache, r.originalCache)
	// Preserve the device boundary after all session and turn edits.
	if ids := stagedCodexFingerprintIDs(c, account); ids != nil {
		cm["x-codex-installation-id"] = ids.installationID
		turn["installation_id"] = ids.installationID
		if h != nil {
			h.Set("x-codex-installation-id", ids.installationID)
		}
	}
	turnBytes, err := json.Marshal(turn)
	if err != nil {
		return nil, err
	}
	cm["x-codex-turn-metadata"] = string(turnBytes)
	next := body
	if len(body) > 0 {
		next, err = sjson.SetBytes(next, "prompt_cache_key", cache)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(c.Request.URL.Path, "/responses/compact") {
			next, err = sjson.DeleteBytes(next, "client_metadata")
		} else {
			next, err = sjson.SetBytes(next, "client_metadata", cm)
		}
		if err != nil {
			return nil, err
		}
		var patchErr error
		gjson.GetBytes(next, "input").ForEach(func(i, item gjson.Result) bool {
			original := item.Get("internal_chat_message_metadata_passthrough.turn_id").String()
			if original != "" {
				if raw, ok := r.restore["turn_id:"+original]; ok {
					original = raw
				}
				mapped := codexCapacityNode(identity, owner, original)
				remember("turn_id", mapped, original)
				next, patchErr = sjson.SetBytes(next, fmt.Sprintf("input.%d.internal_chat_message_metadata_passthrough.turn_id", i.Int()), mapped)
			}
			return patchErr == nil
		})
		if patchErr != nil {
			return nil, patchErr
		}
	}
	if h != nil {
		for _, name := range []string{"session-id", "session_id"} {
			h.Set(name, a.SessionID)
		}
		h.Set("thread-id", a.ThreadID)
		h.Set("x-client-request-id", a.ThreadID)
		// conversation_id is connection/thread affinity, never the shared root.
		if h.Get("conversation_id") != "" {
			h.Set("conversation_id", a.ThreadID)
		}
		if a.ParentThreadID != "" {
			h.Set("x-codex-parent-thread-id", a.ParentThreadID)
		} else {
			h.Del("x-codex-parent-thread-id")
		}
		if marker, _ := cm["x-openai-subagent"].(string); marker != "" {
			h.Set("x-openai-subagent", marker)
		}
		if w, ok := cm["x-codex-window-id"].(string); ok {
			h.Set("x-codex-window-id", w)
		}
		headerTurn := make(map[string]any, len(turn))
		for k, v := range turn {
			if k != "tool_namespaces_info" {
				headerTurn[k] = v
			}
		}
		b, _ := json.Marshal(headerTurn)
		h.Set("x-codex-turn-metadata", asciiCodexCapacityJSON(b))
	}
	return next, nil
}

func restoreCodexCapacityResponse(c *gin.Context, body []byte) []byte {
	r := codexCapacityFromGin(c)
	if r == nil {
		return body
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lease == nil {
		return body
	}
	response := gjson.ParseBytes(body)
	prefix := ""
	if response.Get("response").IsObject() {
		response = response.Get("response")
		prefix = "response."
	}
	if id := response.Get("id").String(); id != "" && (prefix != "" || response.Get("output").Exists()) {
		r.lease.rememberResponse(id, time.Now())
	}
	// Restore response metadata only. Never recursively rewrite model/tool payloads.
	out := body
	if cm := response.Get("client_metadata"); cm.IsObject() {
		m := map[string]any{}
		if json.Unmarshal([]byte(cm.Raw), &m) == nil {
			restore := func(values map[string]any) {
				for k, v := range values {
					if s, ok := v.(string); ok {
						if orig, ok := r.restore[k+":"+s]; ok {
							if orig == "" {
								delete(values, k)
							} else {
								values[k] = orig
							}
						}
					}
				}
			}
			restore(m)
			if raw, ok := m["x-codex-turn-metadata"].(string); ok {
				turn := map[string]any{}
				if json.Unmarshal([]byte(raw), &turn) == nil {
					restore(turn)
					if r.lease.Assignment.Synthetic {
						delete(turn, "subagent_kind")
						delete(turn, "thread_source")
					}
					b, _ := json.Marshal(turn)
					m["x-codex-turn-metadata"] = string(b)
				}
			}
			if r.lease.Assignment.Synthetic {
				delete(m, "x-openai-subagent")
			}
			if b, e := sjson.SetBytes(out, prefix+"client_metadata", m); e == nil {
				out = b
			}
		}
	}
	if key := response.Get("prompt_cache_key").String(); key != "" {
		if original, ok := r.restore["prompt_cache_key:"+key]; ok {
			var b []byte
			var e error
			if original == "" {
				b, e = sjson.DeleteBytes(out, prefix+"prompt_cache_key")
			} else {
				b, e = sjson.SetBytes(out, prefix+"prompt_cache_key", original)
			}
			if e == nil {
				out = b
			}
		}
	}
	return out
}

func HasCodexSessionCapacity(c *gin.Context) bool {
	r := codexCapacityFromGin(c)
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lease != nil
}
func ReleaseCodexSessionCapacity(c *gin.Context) {
	if r := codexCapacityFromGin(c); r != nil {
		r.clear()
	}
}
func applyCodexCapacityHTTPRequest(c *gin.Context, account *Account, req *http.Request, body []byte) error {
	if _, enabled := account.codexCapacityPolicy(); !enabled {
		return nil
	}
	if capacityLeaseForAccount(c, account) == nil {
		return capacityError("session_capacity_not_admitted", 400, 1)
	}
	projected, err := projectCodexCapacity(c, account, body, req.Header)
	if err != nil {
		return err
	}
	req.Body = io.NopCloser(bytes.NewReader(projected))
	req.ContentLength = int64(len(projected))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(projected)), nil }
	return nil
}

// HTTP header values must remain ASCII even if client turn metadata contains Unicode.
func asciiCodexCapacityJSON(raw []byte) string {
	var b strings.Builder
	for _, r := range string(raw) {
		if r < 128 {
			b.WriteRune(r)
		} else if r <= 0xffff {
			fmt.Fprintf(&b, "\\u%04x", r)
		} else {
			r -= 0x10000
			fmt.Fprintf(&b, "\\u%04x\\u%04x", 0xd800+(r>>10), 0xdc00+(r&1023))
		}
	}
	return b.String()
}
