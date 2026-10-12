package service

import (
	"crypto/sha256"
	"encoding/binary"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

// 会话池：session 指纹模式的结构修正。
//
// 旧行为是「1 个 installation + 1 个 session + N 个线程」，而且这 N 个线程没有
// 任何父子关系标记。真实 Codex 里一个 session 下出现多个线程只有一种来源——
// 父线程 spawn 子代理，子代理请求必带 x-openai-subagent / parent_thread_id /
// thread_source=subagent（见 research/codex-header-comparison-20260910 的 CLI
// 抓包）。所以旧结构是真实客户端产生不出来的。
//
// 会话池把共享同一账号的客户端线程分散到 N 个上游会话，并在同一会话内选出一个
// 根线程：根线程按用户线程上报，其余线程挂成它的子代理并补齐三个标记。
// N 由账号级 extra `codex_fingerprint_sessions` 控制，缺省 1 = 保持旧行为。
const (
	codexFingerprintSessionsExtraKey = "codex_fingerprint_sessions"
	codexFingerprintMaxSessions      = 64

	// codexSessionRootIdleTTL 根线程空闲多久后重新选举。只有根线程自己的请求
	// 续期：否则根线程早已下线，子代理还会把它当父线程挂满整个窗口。
	codexSessionRootIdleTTL = 6 * time.Hour
	// codexSessionRootMaxEntries 选举表容量上界（键是 账号身份 × 会话序号）。
	codexSessionRootMaxEntries = 20000

	// 子代理标记取值与 capacity 模式一致。
	codexSubagentMarker       = "collab_spawn"
	codexSubagentKind         = "thread_spawn"
	codexThreadSourceSubagent = "subagent"
)

// sessionPoolEnabled 会话池是否对本次请求生效。只有 session 模式显式配置了
// codex_fingerprint_sessions >= 2 才启用：缺省账号的出站身份与上线前逐字节一致，
// 不会在升级时静默改写（对齐 #5610 的教训）。
func (ids *codexFingerprintIDs) sessionPoolEnabled() bool {
	return ids != nil && ids.mode == codexFingerprintSession && ids.seed != "" && ids.sessions > 1
}

// codexFingerprintSessionCount 读账号级会话池大小。<=1 表示不启用（旧行为）。
func codexFingerprintSessionCount(extra map[string]any) int {
	if extra == nil {
		return 1
	}
	raw, exists := extra[codexFingerprintSessionsExtraKey]
	if !exists {
		return 1
	}
	n := parseExtraInt(raw)
	if n <= 1 {
		return 1
	}
	if n > codexFingerprintMaxSessions {
		return codexFingerprintMaxSessions
	}
	return n
}

// codexSessionBucket 把客户端线程确定性地分到某个会话序号。
func codexSessionBucket(seed, clientThreadID string, sessions int) int {
	if sessions <= 1 || seed == "" {
		return 0
	}
	sum := sha256.Sum256([]byte("sub2api:codex-session-bucket:v1:" + seed + ":" + clientThreadID))
	return int(binary.BigEndian.Uint32(sum[:4]) % uint32(sessions))
}

// resolveConvergedSessionIDIndexed 返回第 index 个会话的 session_id。
// index 0 沿用旧的单会话取值，使启用会话池不会让所有客户端同时换会话。
func resolveConvergedSessionIDIndexed(seed string, index int) string {
	if seed == "" {
		return ""
	}
	if index <= 0 {
		return resolveConvergedSessionID(seed)
	}
	return deriveStableUUIDv4("sub2api:codex-session-id:v2:" + seed + ":session:" + strconv.Itoa(index))
}

type codexSessionRoot struct {
	thread   string
	lastSeen time.Time
}

// codexSessionRootRegistry 记录每个（账号身份, 会话序号）当前的根线程。
// 进程级状态：重启后重新选举，代价是子代理角色会变一次。
type codexSessionRootRegistry struct {
	mu     sync.Mutex
	roots  map[string]codexSessionRoot
	writes uint64
}

var codexSessionRoots = &codexSessionRootRegistry{roots: make(map[string]codexSessionRoot)}

// elect 返回该会话当前的根线程，以及本线程是否就是根线程。
// 首个到达的线程当选；在任期内其他线程一律判为子代理。
func (r *codexSessionRootRegistry) elect(key, threadID string, now time.Time) (string, bool) {
	if key == "" || threadID == "" {
		return "", true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.roots[key]; ok && entry.thread != "" && now.Sub(entry.lastSeen) < codexSessionRootIdleTTL {
		if entry.thread == threadID {
			entry.lastSeen = now
			r.roots[key] = entry
			return threadID, true
		}
		return entry.thread, false
	}
	r.roots[key] = codexSessionRoot{thread: threadID, lastSeen: now}
	r.sweepLocked(now)
	return threadID, true
}

// sweepLocked 机会式清扫：每 256 次写入或超过容量上界时清掉过期任期。
func (r *codexSessionRootRegistry) sweepLocked(now time.Time) {
	r.writes++
	if r.writes%256 != 0 && len(r.roots) <= codexSessionRootMaxEntries {
		return
	}
	for k, v := range r.roots {
		if now.Sub(v.lastSeen) >= codexSessionRootIdleTTL {
			delete(r.roots, k)
		}
	}
}

func (r *codexSessionRootRegistry) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.roots = make(map[string]codexSessionRoot)
	r.writes = 0
}

// codexClientThreadIdentity 取客户端原始的线程标识与父线程标识。
// 与 codexClientTurnIdentity 同样的载体优先级：请求头优先于请求体。
func codexClientThreadIdentity(h http.Header, clientMetadata map[string]any) (thread string, parent string) {
	fromTurnMetadata := func(raw string) (string, string) {
		raw = strings.TrimSpace(raw)
		if raw == "" || !gjson.Valid(raw) {
			return "", ""
		}
		meta := gjson.Parse(raw)
		if !meta.IsObject() {
			return "", ""
		}
		return strings.TrimSpace(meta.Get("thread_id").String()), strings.TrimSpace(meta.Get("parent_thread_id").String())
	}
	if h != nil {
		thread = strings.TrimSpace(h.Get("thread-id"))
		parent = strings.TrimSpace(h.Get("x-codex-parent-thread-id"))
		metaThread, metaParent := fromTurnMetadata(h.Get("x-codex-turn-metadata"))
		if thread == "" {
			thread = metaThread
		}
		if parent == "" {
			parent = metaParent
		}
		if thread != "" || parent != "" {
			return thread, parent
		}
	}
	if clientMetadata == nil {
		return "", ""
	}
	if v, ok := clientMetadata["thread_id"].(string); ok {
		thread = strings.TrimSpace(v)
	}
	if v, ok := clientMetadata["x-codex-parent-thread-id"].(string); ok {
		parent = strings.TrimSpace(v)
	}
	embedded, _ := clientMetadata["x-codex-turn-metadata"].(string)
	metaThread, metaParent := fromTurnMetadata(embedded)
	if thread == "" {
		thread = metaThread
	}
	if parent == "" {
		parent = metaParent
	}
	return thread, parent
}

// codexClientWindowNumber 取客户端窗口编号（window_id 形如 "<thread>:N"）。
// 旧实现把出站窗口恒定写成 :0，真实 Codex 在 compact 之后编号会递增。
func codexClientWindowNumber(h http.Header, clientMetadata map[string]any) string {
	number := func(raw string) string {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return ""
		}
		_, suffix, ok := strings.Cut(raw, ":")
		if !ok {
			return ""
		}
		if _, err := strconv.ParseUint(suffix, 10, 32); err != nil {
			return ""
		}
		return suffix
	}
	fromTurnMetadata := func(raw string) string {
		raw = strings.TrimSpace(raw)
		if raw == "" || !gjson.Valid(raw) {
			return ""
		}
		return number(gjson.Parse(raw).Get("window_id").String())
	}
	if h != nil {
		if n := number(h.Get("x-codex-window-id")); n != "" {
			return n
		}
		if n := fromTurnMetadata(h.Get("x-codex-turn-metadata")); n != "" {
			return n
		}
	}
	if clientMetadata == nil {
		return ""
	}
	if v, ok := clientMetadata["x-codex-window-id"].(string); ok {
		if n := number(v); n != "" {
			return n
		}
	}
	embedded, _ := clientMetadata["x-codex-turn-metadata"].(string)
	return fromTurnMetadata(embedded)
}

// adoptClientThread 按客户端线程标识重算 thread_id / session_id / 父线程 / 窗口。
//
//   - thread_id 改为从客户端线程标识派生。客户端 thread_id == session-id（单线程
//     CLI 的常见形态）时取值与旧实现一致；真正开了多线程的客户端不再被挤进同一个
//     线程。
//   - 会话池启用时，线程按 bucket 分散到 N 个会话，并在会话内选根线程；非根线程
//     挂成子代理（parent_thread_id + x-openai-subagent + subagent_kind +
//     thread_source）。
//   - 客户端自己声明了父线程时按它自己的子代理处理：只做确定性映射，不参与选举、
//     不补标记（标记本来就在它的请求里）。
//   - 窗口编号沿用客户端值。
//
// 只在任何载体写出线程字段之前生效（与 adoptClientTurn 同样的冻结语义）。
func (ids *codexFingerprintIDs) adoptClientThread(h http.Header, clientMetadata map[string]any) {
	if ids == nil || ids.threadBound || ids.threadApplied || !ids.sessionPoolEnabled() {
		return
	}
	clientThread, clientParent := codexClientThreadIdentity(h, clientMetadata)
	if clientThread == "" {
		clientThread = ids.clientSessionID
	}
	if clientThread == "" {
		return
	}

	ids.threadID = resolveConvergedThreadID(ids.seed, clientThread)
	if ids.threadID == "" {
		return
	}
	// 客户端自己的子代理必须和父线程落在同一个上游会话里——真实 Codex 的子代理
	// 就是在父线程所在会话内 spawn 的。所以按父线程分桶，不按自己分桶。
	bucketKey := clientThread
	if clientParent != "" {
		bucketKey = clientParent
	}
	bucket := codexSessionBucket(ids.seed, bucketKey, ids.sessions)
	if sessionID := resolveConvergedSessionIDIndexed(ids.seed, bucket); sessionID != "" {
		ids.sessionID = sessionID
	}

	switch {
	case clientParent != "":
		// 客户端自己的子代理：父线程按同一规则映射，和父线程自己的出站 thread_id 相等。
		ids.parentThread = resolveConvergedThreadID(ids.seed, clientParent)
	case ids.namespace != "":
		root, isRoot := codexSessionRoots.elect(ids.namespace+"\x00"+strconv.Itoa(bucket), ids.threadID, time.Now())
		if !isRoot && root != "" {
			ids.parentThread = root
			ids.mounted = true
		}
	}

	window := codexClientWindowNumber(h, clientMetadata)
	if window == "" {
		window = "0"
	}
	ids.windowID = ids.threadID + ":" + window
	ids.threadBound = true
}

// convergedThreadFields 返回 session 模式要写入 turn metadata 的线程字段。
// 调用即视为已写出，之后 adoptClientThread 不再改动 ids。
func (ids *codexFingerprintIDs) convergedThreadFields() map[string]any {
	if ids == nil || !ids.sessionPoolEnabled() {
		return nil
	}
	ids.threadApplied = true
	fields := map[string]any{}
	if ids.mounted {
		fields["subagent_kind"] = codexSubagentKind
		fields["thread_source"] = codexThreadSourceSubagent
	}
	return fields
}

// codexConvergedThreadScopedFields 是 turn metadata 里引用其他线程/窗口的字段，
// 按线程命名空间确定性映射，避免把客户端原值交给上游。
var codexConvergedThreadScopedFields = []string{"forked_from_thread_id", "guardian_classifier_source_thread_id", "context_window_id"}

// convergeThreadScopedIDs 改写线程引用字段与 parent_thread_id。
// 必须在写入 convergedThreadFields 之前调用。
func (ids *codexFingerprintIDs) convergeThreadScopedIDs(metadata map[string]any) {
	if ids == nil || metadata == nil || !ids.sessionPoolEnabled() {
		return
	}
	for _, field := range codexConvergedThreadScopedFields {
		raw, ok := metadata[field].(string)
		if raw = strings.TrimSpace(raw); !ok || raw == "" {
			continue
		}
		metadata[field] = resolveConvergedThreadID(ids.seed, raw)
	}
	if ids.parentThread != "" {
		metadata["parent_thread_id"] = ids.parentThread
	} else {
		delete(metadata, "parent_thread_id")
	}
}

// applyCodexFingerprintThreadHeaders 写出线程相关的出站头。
func applyCodexFingerprintThreadHeaders(h http.Header, ids *codexFingerprintIDs) {
	if h == nil || ids == nil || !ids.sessionPoolEnabled() {
		return
	}
	if ids.parentThread != "" {
		h.Set("x-codex-parent-thread-id", ids.parentThread)
	} else {
		h.Del("x-codex-parent-thread-id")
	}
	if ids.mounted {
		h.Set("x-openai-subagent", codexSubagentMarker)
	}
}

// applyCodexFingerprintThreadClientMetadata 写出线程相关的 client_metadata 字段。
func applyCodexFingerprintThreadClientMetadata(existing map[string]any, ids *codexFingerprintIDs) {
	if existing == nil || ids == nil || !ids.sessionPoolEnabled() {
		return
	}
	if ids.parentThread != "" {
		existing["x-codex-parent-thread-id"] = ids.parentThread
	} else {
		delete(existing, "x-codex-parent-thread-id")
	}
	if ids.mounted {
		existing["x-openai-subagent"] = codexSubagentMarker
	}
}
