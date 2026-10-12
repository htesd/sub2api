package service

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sessionPoolAccount 造一个启用会话池的 session 模式账号。seed 每个用例不同，
// 根线程选举表是进程级的，不同用例必须落在不同命名空间上。
func sessionPoolAccount(t *testing.T, id int64, seed string, sessions int) *Account {
	t.Helper()
	extra := map[string]any{
		codexFingerprintModeExtraKey: "session",
		codexFingerprintSeedExtraKey: seed,
	}
	if sessions > 0 {
		extra[codexFingerprintSessionsExtraKey] = sessions
	}
	account := newTestOAuthAccount(id, extra)
	account.Credentials = map[string]any{"chatgpt_account_id": "acct-" + seed, "chatgpt_user_id": "user-" + seed}
	return account
}

// sessionPoolRequest 模拟一次完整请求：先改写 body、再改写出站头（与生产顺序一致），
// 返回出站头、client_metadata 以及内嵌 turn metadata。
func sessionPoolRequest(t *testing.T, account *Account, clientSession, clientThread, clientParent, clientWindow string) (http.Header, map[string]any, map[string]any) {
	t.Helper()
	clientHeaders := http.Header{}
	clientHeaders.Set("session-id", clientSession)
	if clientThread != "" {
		clientHeaders.Set("thread-id", clientThread)
	}
	if clientParent != "" {
		clientHeaders.Set("x-codex-parent-thread-id", clientParent)
	}
	if clientWindow != "" {
		clientHeaders.Set("x-codex-window-id", clientWindow)
	}
	turn := map[string]any{"thread_id": clientThread, "turn_id": "client-turn", "sandbox": "seccomp", "thread_source": "user"}
	if clientParent != "" {
		turn["parent_thread_id"] = clientParent
	}
	if clientWindow != "" {
		turn["window_id"] = clientWindow
	}
	turnRaw, err := json.Marshal(turn)
	require.NoError(t, err)
	clientHeaders.Set("x-codex-turn-metadata", string(turnRaw))

	ids := resolveCodexFingerprintIDsFromRequest(account, clientHeaders)
	require.NotNil(t, ids)

	reqBody := map[string]any{"client_metadata": map[string]any{
		"session_id":            clientSession,
		"thread_id":             clientThread,
		"x-codex-turn-metadata": string(turnRaw),
	}}
	require.True(t, applyCodexFingerprintClientMetadata(reqBody, ids))

	out := clientHeaders.Clone()
	applyCodexFingerprintHeaders(out, ids)

	cm, ok := reqBody["client_metadata"].(map[string]any)
	require.True(t, ok)
	var embedded map[string]any
	require.NoError(t, json.Unmarshal([]byte(cm["x-codex-turn-metadata"].(string)), &embedded))
	return out, cm, embedded
}

// 缺省（未配置 codex_fingerprint_sessions）必须与会话池上线前完全一致：
// 单会话、不挂子代理、thread_id 仍由 session-id 派生。
func TestSessionPoolDisabledByDefaultKeepsLegacyIdentity(t *testing.T) {
	codexSessionRoots.reset()
	account := sessionPoolAccount(t, 1, "a1111111-1111-4111-8111-111111111111", 0)
	seed, _ := codexFingerprintSeed(account.Extra)

	h, cm, embedded := sessionPoolRequest(t, account, "client-sess", "", "", "")

	assert.Equal(t, 1, codexFingerprintSessionCount(account.Extra))
	assert.Equal(t, resolveConvergedSessionID(seed), h.Get("session-id"))
	assert.Equal(t, resolveConvergedThreadID(seed, "client-sess"), h.Get("thread-id"))
	assert.Equal(t, resolveConvergedThreadID(seed, "client-sess"), cm["thread_id"])
	assert.Empty(t, h.Get("x-openai-subagent"))
	assert.Empty(t, h.Get("x-codex-parent-thread-id"))
	assert.NotContains(t, cm, "x-openai-subagent")
	assert.NotContains(t, embedded, "parent_thread_id")
	assert.NotContains(t, embedded, "subagent_kind")
	assert.Equal(t, "user", embedded["thread_source"], "根线程保持客户端上报的 user")
}

// 客户端 thread-id 与 session-id 相同（单线程 CLI 的常见形态）时，派生值不变，
// 启用会话池不会让这些客户端换线程。
func TestSessionPoolThreadIDMatchesLegacyWhenThreadEqualsSession(t *testing.T) {
	codexSessionRoots.reset()
	account := sessionPoolAccount(t, 2, "a2222222-2222-4222-8222-222222222222", 4)
	seed, _ := codexFingerprintSeed(account.Extra)
	h, _, _ := sessionPoolRequest(t, account, "same-id", "same-id", "", "")
	assert.Equal(t, resolveConvergedThreadID(seed, "same-id"), h.Get("thread-id"))
}

// 客户端真的开了多个线程时，不再被挤进同一个线程。
func TestSessionPoolDistinctClientThreadsStayDistinct(t *testing.T) {
	codexSessionRoots.reset()
	account := sessionPoolAccount(t, 3, "a3333333-3333-4333-8333-333333333333", 4)
	a, _, _ := sessionPoolRequest(t, account, "sess", "thread-a", "", "")
	b, _, _ := sessionPoolRequest(t, account, "sess", "thread-b", "", "")
	assert.NotEqual(t, a.Get("thread-id"), b.Get("thread-id"))
}

// 会话池把客户端分散到 N 个会话；序号 0 沿用旧的单会话取值。
func TestSessionPoolSpreadsClientsAcrossSessions(t *testing.T) {
	codexSessionRoots.reset()
	account := sessionPoolAccount(t, 4, "a4444444-4444-4444-8444-444444444444", 4)
	seed, _ := codexFingerprintSeed(account.Extra)

	seen := map[string]bool{}
	for _, client := range []string{"c1", "c2", "c3", "c4", "c5", "c6", "c7", "c8", "c9", "c10", "c11", "c12"} {
		h, _, _ := sessionPoolRequest(t, account, client, client, "", "")
		seen[h.Get("session-id")] = true
	}
	assert.Greater(t, len(seen), 1, "必须用到多个上游会话")
	assert.LessOrEqual(t, len(seen), 4, "不得超过配置的会话数")
	assert.True(t, seen[resolveConvergedSessionID(seed)], "序号 0 沿用旧会话取值")
}

// 同一会话内：首个线程当根，其余线程挂成它的子代理并补齐三个标记。
func TestSessionPoolMountsSubagentsUnderElectedRoot(t *testing.T) {
	codexSessionRoots.reset()
	const seed = "a5555555-5555-4555-8555-555555555555"
	account := sessionPoolAccount(t, 5, seed, 2)

	// 挑两个落在同一会话序号的客户端线程。
	var first, second string
	for _, candidate := range []string{"t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8"} {
		if first == "" {
			first = candidate
			continue
		}
		if codexSessionBucket(seed, candidate, 2) == codexSessionBucket(seed, first, 2) {
			second = candidate
			break
		}
	}
	require.NotEmpty(t, second, "需要两个同会话的客户端线程")

	rootHeader, rootCM, rootMeta := sessionPoolRequest(t, account, first, first, "", "")
	subHeader, subCM, subMeta := sessionPoolRequest(t, account, second, second, "", "")

	assert.Equal(t, rootHeader.Get("session-id"), subHeader.Get("session-id"), "两者在同一会话下")

	// 根线程：没有父线程，也没有子代理标记。
	assert.Empty(t, rootHeader.Get("x-codex-parent-thread-id"))
	assert.Empty(t, rootHeader.Get("x-openai-subagent"))
	assert.NotContains(t, rootMeta, "parent_thread_id")
	assert.NotContains(t, rootMeta, "subagent_kind")
	assert.NotContains(t, rootCM, "x-openai-subagent")

	// 子代理：父线程指向根线程的出站 thread_id，三个标记齐全（头、体、内嵌各一份）。
	assert.Equal(t, rootHeader.Get("thread-id"), subHeader.Get("x-codex-parent-thread-id"))
	assert.Equal(t, rootHeader.Get("thread-id"), subMeta["parent_thread_id"])
	assert.Equal(t, rootHeader.Get("thread-id"), subCM["x-codex-parent-thread-id"])
	assert.Equal(t, codexSubagentMarker, subHeader.Get("x-openai-subagent"))
	assert.Equal(t, codexSubagentMarker, subCM["x-openai-subagent"])
	assert.Equal(t, codexSubagentKind, subMeta["subagent_kind"])
	assert.Equal(t, codexThreadSourceSubagent, subMeta["thread_source"])
	assert.Equal(t, "seccomp", subMeta["sandbox"], "非身份字段保留")
}

// 根线程的后续请求仍然是根线程，角色不会在回合之间抖动。
func TestSessionPoolRootRoleStableAcrossRequests(t *testing.T) {
	codexSessionRoots.reset()
	account := sessionPoolAccount(t, 6, "a6666666-6666-4666-8666-666666666666", 2)
	for i := 0; i < 3; i++ {
		h, _, _ := sessionPoolRequest(t, account, "only-client", "only-client", "", "")
		assert.Empty(t, h.Get("x-openai-subagent"), "第 %d 次请求", i+1)
		assert.Empty(t, h.Get("x-codex-parent-thread-id"), "第 %d 次请求", i+1)
	}
}

// 客户端自己就是子代理时：父线程按同一规则映射，必须等于父线程自己的出站
// thread_id（旧实现把客户端原值直接发给上游，而且 thread_id 只由 session-id
// 派生，会出现自己当自己父线程）。
func TestSessionPoolClientDeclaredParentMapsToParentThread(t *testing.T) {
	codexSessionRoots.reset()
	account := sessionPoolAccount(t, 7, "a7777777-7777-4777-8777-777777777777", 2)

	parentHeader, _, _ := sessionPoolRequest(t, account, "sess-x", "thread-main", "", "")
	childHeader, childCM, childMeta := sessionPoolRequest(t, account, "sess-x", "thread-child", "thread-main", "")

	assert.Equal(t, parentHeader.Get("thread-id"), childHeader.Get("x-codex-parent-thread-id"))
	assert.Equal(t, parentHeader.Get("thread-id"), childMeta["parent_thread_id"])
	assert.Equal(t, parentHeader.Get("session-id"), childHeader.Get("session-id"), "子代理必须和父线程同会话")
	assert.NotEqual(t, childHeader.Get("thread-id"), childHeader.Get("x-codex-parent-thread-id"), "不得自己当自己的父线程")
	assert.Empty(t, childHeader.Get("x-openai-subagent"), "客户端自带的子代理不额外补标记")
	assert.NotContains(t, childCM, "x-openai-subagent")
	assert.Equal(t, "user", childMeta["thread_source"], "沿用客户端上报值")
}

// 窗口编号沿用客户端值（旧实现恒定写成 :0）。
func TestSessionPoolPreservesClientWindowNumber(t *testing.T) {
	codexSessionRoots.reset()
	account := sessionPoolAccount(t, 8, "a8888888-8888-4888-8888-888888888888", 2)
	h, cm, embedded := sessionPoolRequest(t, account, "win-client", "win-client", "", "client-thread:3")
	assert.Equal(t, h.Get("thread-id")+":3", h.Get("x-codex-window-id"))
	assert.Equal(t, h.Get("thread-id")+":3", cm["x-codex-window-id"])
	assert.Equal(t, h.Get("thread-id")+":3", embedded["window_id"])

	none, _, _ := sessionPoolRequest(t, account, "win-none", "win-none", "", "")
	assert.Equal(t, none.Get("thread-id")+":0", none.Get("x-codex-window-id"), "客户端没给编号时回落到 0")
}

// 引用其他线程/窗口的字段不得把客户端原值交给上游。
func TestSessionPoolConvergesThreadScopedIDs(t *testing.T) {
	codexSessionRoots.reset()
	const seed = "a9999999-9999-4999-8999-999999999999"
	account := sessionPoolAccount(t, 9, seed, 2)
	clientHeaders := http.Header{}
	clientHeaders.Set("session-id", "scoped-client")
	clientHeaders.Set("thread-id", "scoped-client")
	clientHeaders.Set("x-codex-turn-metadata", `{"thread_id":"scoped-client","forked_from_thread_id":"other-thread","guardian_classifier_source_thread_id":"guard-thread","context_window_id":"ctx-window"}`)
	ids := resolveCodexFingerprintIDsFromRequest(account, clientHeaders)
	require.NotNil(t, ids)
	out := clientHeaders.Clone()
	applyCodexFingerprintHeaders(out, ids)

	var meta map[string]any
	require.NoError(t, json.Unmarshal([]byte(out.Get("x-codex-turn-metadata")), &meta))
	assert.Equal(t, resolveConvergedThreadID(seed, "other-thread"), meta["forked_from_thread_id"])
	assert.Equal(t, resolveConvergedThreadID(seed, "guard-thread"), meta["guardian_classifier_source_thread_id"])
	assert.Equal(t, resolveConvergedThreadID(seed, "ctx-window"), meta["context_window_id"])
}

// 客户端只在 body 里声明线程时，头和体必须得到同一套线程身份。
func TestSessionPoolBodyOnlyThreadConsistentAcrossCarriers(t *testing.T) {
	codexSessionRoots.reset()
	account := sessionPoolAccount(t, 10, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", 2)
	clientHeaders := http.Header{}
	clientHeaders.Set("session-id", "body-only")
	ids := resolveCodexFingerprintIDsFromRequest(account, clientHeaders)
	require.NotNil(t, ids)

	reqBody := map[string]any{"client_metadata": map[string]any{
		"session_id":            "body-only",
		"thread_id":             "body-thread",
		"x-codex-turn-metadata": `{"thread_id":"body-thread"}`,
	}}
	require.True(t, applyCodexFingerprintClientMetadata(reqBody, ids))
	out := http.Header{}
	out.Set("x-codex-turn-metadata", `{"sandbox":"seccomp"}`)
	applyCodexFingerprintHeaders(out, ids)

	cm := reqBody["client_metadata"].(map[string]any)
	assert.Equal(t, cm["thread_id"], out.Get("thread-id"))
	assert.Equal(t, cm["session_id"], out.Get("session-id"))
	assert.Equal(t, cm["x-codex-window-id"], out.Get("x-codex-window-id"))
}

// 根线程空闲超过任期后重新选举；期间只有根线程自己的请求续期。
func TestSessionPoolRootReelectedAfterIdleTTL(t *testing.T) {
	codexSessionRoots.reset()
	now := time.Now()
	root, isRoot := codexSessionRoots.elect("ns\x001", "thread-root", now)
	require.True(t, isRoot)
	require.Equal(t, "thread-root", root)

	root, isRoot = codexSessionRoots.elect("ns\x001", "thread-other", now.Add(time.Minute))
	assert.False(t, isRoot)
	assert.Equal(t, "thread-root", root, "任期内父线程不变")

	root, isRoot = codexSessionRoots.elect("ns\x001", "thread-other", now.Add(codexSessionRootIdleTTL+time.Second))
	assert.True(t, isRoot, "根线程空闲超过任期后重新选举")
	assert.Equal(t, "thread-other", root)
}

// 会话数配置的边界：非法值回落到 1，超过上限夹到上限。
func TestSessionPoolSessionCountBounds(t *testing.T) {
	assert.Equal(t, 1, codexFingerprintSessionCount(nil))
	assert.Equal(t, 1, codexFingerprintSessionCount(map[string]any{}))
	assert.Equal(t, 1, codexFingerprintSessionCount(map[string]any{codexFingerprintSessionsExtraKey: 0}))
	assert.Equal(t, 1, codexFingerprintSessionCount(map[string]any{codexFingerprintSessionsExtraKey: -3}))
	assert.Equal(t, 1, codexFingerprintSessionCount(map[string]any{codexFingerprintSessionsExtraKey: "bad"}))
	assert.Equal(t, 6, codexFingerprintSessionCount(map[string]any{codexFingerprintSessionsExtraKey: "6"}))
	assert.Equal(t, 5, codexFingerprintSessionCount(map[string]any{codexFingerprintSessionsExtraKey: float64(5)}))
	assert.Equal(t, codexFingerprintMaxSessions, codexFingerprintSessionCount(map[string]any{codexFingerprintSessionsExtraKey: 1000}))
}

// device / capacity / full 模式不受会话池影响。
func TestSessionPoolDoesNotTouchOtherModes(t *testing.T) {
	codexSessionRoots.reset()
	for _, mode := range []string{"device", "capacity", "full"} {
		t.Run(mode, func(t *testing.T) {
			account := newTestOAuthAccount(20, map[string]any{
				codexFingerprintModeExtraKey:     mode,
				codexFingerprintSeedExtraKey:     "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
				codexFingerprintSessionsExtraKey: 4,
			})
			clientHeaders := http.Header{}
			clientHeaders.Set("session-id", "other-mode")
			clientHeaders.Set("thread-id", "other-mode-thread")
			clientHeaders.Set("x-codex-turn-metadata", `{"thread_id":"other-mode-thread","forked_from_thread_id":"keep-me"}`)
			ids := resolveCodexFingerprintIDsFromRequest(account, clientHeaders)
			require.NotNil(t, ids)
			assert.False(t, ids.threadBound)
			assert.Empty(t, ids.parentThread)
			assert.False(t, ids.mounted)

			out := clientHeaders.Clone()
			applyCodexFingerprintHeaders(out, ids)
			assert.Empty(t, out.Get("x-openai-subagent"))
			var meta map[string]any
			require.NoError(t, json.Unmarshal([]byte(out.Get("x-codex-turn-metadata")), &meta))
			if mode == "full" {
				seed, _ := codexFingerprintSeed(account.Extra)
				assert.Equal(t, resolveConvergedSessionID(seed), out.Get("thread-id"), "full 模式线程仍等于会话")
			}
			assert.Equal(t, "keep-me", meta["forked_from_thread_id"], "%s 模式不做线程引用映射", mode)
		})
	}
}
