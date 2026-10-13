package service

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"
)

// 登记表里的字符串要保留 24 小时，必须是独立副本。gjson 取出的值是原文的子串，
// 直接存进 map 会把整份请求体/回包钉在内存里（线上 .31 因此 58 小时涨到 22.7G）。
func TestCodexCapacityRegistryDoesNotPinCallerBuffers(t *testing.T) {
	body := []byte(`{"prompt_cache_key":"thread-from-body","id":"resp_from_body"}`)
	// 模拟 parseRawJSONView 的零拷贝视图：子串直接指向 body 的底层数组。
	view := unsafe.String(unsafe.SliceData(body), len(body))
	sub := func(s string) string {
		i := strings.Index(view, s)
		require.GreaterOrEqual(t, i, 0)
		return view[i : i+len(s)]
	}
	inBody := func(s string) bool {
		p := uintptr(unsafe.Pointer(unsafe.StringData(s)))
		base := uintptr(unsafe.Pointer(unsafe.SliceData(body)))
		return p >= base && p < base+uintptr(len(body))
	}

	r := newCodexCapacityRegistry()
	now := time.Now()
	l := exposeCapacity(t, r, capacityAdmissionPolicy(), capacityInput(1, "root", sub("thread-from-body")), now)
	l.rememberResponse(sub("resp_from_body"), now)

	bindings := r.accounts["account"].Bindings
	require.Len(t, bindings, 1)
	for key := range bindings {
		require.Equal(t, "thread-from-body", key.Thread)
		require.False(t, inBody(key.Thread), "binding key still aliases the request body")
	}
	require.Len(t, l.binding.Responses, 1)
	for id := range l.binding.Responses {
		require.Equal(t, "resp_from_body", id)
		require.False(t, inBody(id), "response id still aliases the response payload")
	}
}

// 走真实回包路径：每个 response.completed 只应留下一个短 id，不能把整份回包留住。
func TestCodexCapacityResponseRestoreDoesNotRetainPayloads(t *testing.T) {
	body := []byte(`{"model":"gpt-5.1","client_metadata":{"session_id":"leak","thread_id":"leak"}}`)
	c, a, svc := capacityContext(t, body)
	r := svc.codexCapacityRegistry()
	now := time.Now()
	state := codexCapacityFromGin(c)
	l, err := r.reserve(codexAccountIdentityNamespace(a), capacityAdmissionPolicy(), state.input, true, now)
	require.NoError(t, err)
	state.stage(a.ID, l, l.release)

	const responses = 16
	const payloadBytes = 2 << 20
	padding := strings.Repeat("x", payloadBytes)
	heapAlloc := func() uint64 {
		runtime.GC()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}
	before := heapAlloc()
	for i := 0; i < responses; i++ {
		event := []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%02d","output":[{"text":%q}]}}`, i, padding))
		_ = restoreCodexToolNamesFromContext(c, event)
	}
	after := heapAlloc()

	require.Len(t, l.binding.Responses, responses)
	var growth uint64
	if after > before {
		growth = after - before
	}
	// 泄漏时会留住 16 × 2MiB = 32MiB；修复后只剩 16 个短 id。
	require.Less(t, growth, uint64(8<<20), "live heap grew by %d bytes after %d responses", growth, responses)
}
