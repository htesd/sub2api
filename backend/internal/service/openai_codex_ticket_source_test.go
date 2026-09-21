package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type ticketProxyRecordingUpstream struct {
	HTTPUpstream
	proxies []string
	do      func(int, *http.Request) (*http.Response, error)
}

func (u *ticketProxyRecordingUpstream) Do(req *http.Request, proxy string, _ int64, _ int) (*http.Response, error) {
	u.proxies = append(u.proxies, proxy)
	return u.do(len(u.proxies), req)
}

func ticketAccountWithProxy() *Account {
	a := ticketTestAccount(41)
	a.Status = StatusActive
	a.Proxy = &Proxy{ID: 100, Protocol: "http", Host: "original.example", Port: 8080, Username: "user", Password: "proxy-secret"}
	a.ProxyID = &a.Proxy.ID
	a.Proxy.UpdatedAt = time.Now().Add(-time.Hour).UTC()
	return a
}

func TestCodexTicketAccountProxyPriorityAndBoundedFallback(t *testing.T) {
	for _, tc := range []struct {
		name       string
		firstCode  int
		firstState string
		firstErr   error
		pool       bool
		poolFails  bool
		calls      int
		source     string
	}{
		{"原代理成功不访问池", 200, fakeCodexTicketState(292), nil, true, false, 1, "account_proxy"},
		{"无需独立代理池", 200, fakeCodexTicketState(292), nil, false, false, 1, "account_proxy"},
		{"票规格不符换池", 200, fakeCodexTicketState(312), nil, true, false, 2, "harvest_pool"},
		{"网络失败换池", 0, "", io.EOF, true, false, 2, "harvest_pool"},
		{"服务错误换池", 503, "", nil, true, false, 2, "harvest_pool"},
		{"认证失败停止", 401, "", nil, true, false, 1, ""},
		{"权限失败停止", 403, "", nil, true, false, 1, ""},
		{"限流不换出口重试", 429, "", nil, true, false, 1, ""},
		{"池也失败只退避一次", 200, fakeCodexTicketState(312), nil, true, true, 2, ""},
		{"池只有原代理不重复", 503, "", nil, false, false, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := ticketAccountWithProxy()
			pool := a.Proxy.URL() // 池里重复的 A 不能成为第二次尝试。
			if tc.pool {
				pool += ",http://fallback.example:8080,http://third.example:8080"
			} else if tc.firstCode == 200 {
				pool = ""
			}
			u := &ticketProxyRecordingUpstream{do: func(n int, req *http.Request) (*http.Response, error) {
				require.Empty(t, req.Header.Get(openAICodexTurnStateHeader))
				require.Equal(t, "Bearer tok", req.Header.Get("Authorization"))
				if n == 1 {
					h := http.Header{}
					h.Set(openAICodexTurnStateHeader, tc.firstState)
					return &http.Response{StatusCode: tc.firstCode, Header: h, Body: io.NopCloser(strings.NewReader(""))}, tc.firstErr
				}
				if tc.poolFails {
					return nil, io.EOF
				}
				return codexTicketResponse(), nil
			}}
			svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, HarvestLoopEnabled: true, HarvestProxyURL: pool}, u)
			svc.accountRepo = &ticketTestPassthroughRepo{account: a}
			// 即使池的全局冷却记录包含 A，也不能跳过本账号首选代理。
			svc.openAICodexTicketProxyBurn(a.Proxy.URL(), time.Hour, time.Now())
			svc.probeOnceOpenAICodexTicket(context.Background(), a, "gpt-6-astra")
			require.Len(t, u.proxies, tc.calls)
			require.Equal(t, a.Proxy.URL(), u.proxies[0])
			if tc.calls == 2 {
				require.Equal(t, "http://fallback.example:8080", u.proxies[1])
			}
			ticket := svc.lookupOpenAICodexTicket(a, "gpt-6-astra")
			if tc.source == "" {
				require.Nil(t, ticket)
				raw, ok := svc.openaiCodexTicketBackoff.Load(openAICodexTicketKey(a.ID, "gpt-6-astra"))
				require.True(t, ok)
				require.Equal(t, 1, raw.(*openAICodexTicketBackoff).failures)
				return
			}
			require.NotNil(t, ticket)
			require.Equal(t, tc.source, ticket.Source)
			require.Equal(t, tc.calls, ticket.Attempts)
			if tc.source == "account_proxy" {
				require.Equal(t, a.ProxyID, ticket.SourceProxyID)
				require.Equal(t, a.Proxy.UpdatedAt, *ticket.SourceProxyUpdatedAt)
			} else {
				require.Nil(t, ticket.SourceProxyID)
			}
			encoded, err := json.Marshal(ticket)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "proxy-secret")
			require.NotContains(t, string(encoded), "http://")
		})
	}
}

func TestCodexTicketRequiredModelBootstrapsFromAccountProxy(t *testing.T) {
	a := ticketAccountWithProxy()
	a.LastUsedAt = nil
	u := &ticketProxyRecordingUpstream{do: func(int, *http.Request) (*http.Response, error) { return codexTicketResponse(), nil }}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled: true, FailClosed: true, HarvestLoopEnabled: true,
		Models: []string{"gpt-6-astra", "gpt-5.6-sol"}, RequiredModels: []string{"gpt-6-astra"},
	}, u)
	svc.accountRepo = &codexTicketLifecycleRepo{account: *a}
	require.True(t, svc.openAICodexTicketBlocksAccount(a, "gpt-6-astra"))
	svc.refreshOpenAICodexTickets(context.Background())
	require.Equal(t, []string{a.Proxy.URL()}, u.proxies)
	require.False(t, svc.openAICodexTicketBlocksAccount(a, "gpt-6-astra"))
	h := http.Header{}
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), a, "gpt-6-astra", h))
	require.Len(t, h.Get(openAICodexTurnStateHeader), 292)
	require.Nil(t, svc.lookupOpenAICodexTicket(a, "gpt-5.6-sol"), "非名单模型仍尊重活跃窗口")
	// 有票且未临近过期，不会继续打。
	svc.refreshOpenAICodexTickets(context.Background())
	require.Len(t, u.proxies, 1)
}

func TestCodexTicketUsesLatestAccountProxy(t *testing.T) {
	snapshot := ticketAccountWithProxy()
	latest := ticketAccountWithProxy()
	latest.Proxy.Host = "new.example"
	latest.Credentials["access_token"] = "new-token"
	u := &ticketProxyRecordingUpstream{do: func(_ int, req *http.Request) (*http.Response, error) {
		require.Equal(t, "Bearer new-token", req.Header.Get("Authorization"))
		return codexTicketResponse(), nil
	}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, HarvestLoopEnabled: true}, u)
	svc.accountRepo = &ticketTestPassthroughRepo{account: latest}
	svc.probeOnceOpenAICodexTicket(context.Background(), snapshot, "gpt-6-astra")
	require.Equal(t, []string{latest.Proxy.URL()}, u.proxies)
}

func TestCodexTicketFallbackRechecksAuthorization(t *testing.T) {
	for _, change := range []string{"account_off", "global_off", "proxy_changed", "cancel"} {
		t.Run(change, func(t *testing.T) {
			a := ticketAccountWithProxy()
			repo := &codexTicketRevokeRepo{snapshot: *a, latest: *a}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, HarvestLoopEnabled: true, HarvestProxyURL: "http://fallback.example:8080"}, nil)
			u := &ticketProxyRecordingUpstream{do: func(int, *http.Request) (*http.Response, error) {
				switch change {
				case "account_off":
					repo.setLatest(func(a *Account) { a.Extra = map[string]any{openAICodexTicketActiveProbeExtraKey: false} })
				case "global_off":
					svc.cfg.Gateway.OpenAICodexTicket.HarvestLoopEnabled = false
				case "proxy_changed":
					repo.setLatest(func(a *Account) { a.Proxy = &Proxy{Protocol: "http", Host: "new.example", Port: 8080} })
				case "cancel":
					cancel()
				}
				return nil, io.EOF
			}}
			svc.accountRepo, svc.httpUpstream = repo, u
			svc.probeOnceOpenAICodexTicket(ctx, a, "gpt-6-astra")
			require.Len(t, u.proxies, 1)
		})
	}
}

func TestCodexTicketSourceSurvivesPersistenceAndStatus(t *testing.T) {
	for _, source := range []string{"account_proxy", "harvest_pool", "business_response", "", "invalid"} {
		t.Run(source, func(t *testing.T) {
			a := ticketAccountWithProxy()
			ticket := &openAICodexTicket{State: fakeCodexTicketState(292), Length: 292, ExpiresAt: time.Now().Add(time.Hour), Source: source, SourceProxyID: a.ProxyID}
			data, err := json.Marshal(ticket)
			require.NoError(t, err)
			var persisted any
			require.NoError(t, json.Unmarshal(data, &persisted))
			a.Extra[openAICodexTicketExtraKey("gpt-6-astra")] = persisted
			statuses := OpenAICodexTicketStatuses(a, config.OpenAICodexTicketConfig{Enabled: true, Models: []string{"gpt-6-astra"}}, time.Now())
			require.Len(t, statuses, 1)
			require.True(t, statuses[0].Ready)
			if source == "" || source == "invalid" {
				require.Equal(t, "unknown", statuses[0].Source)
				require.Nil(t, statuses[0].SourceProxyID)
			} else {
				require.Equal(t, source, statuses[0].Source)
			}
		})
	}
}

func TestCodexTicketFallbackUsesLatestPoolAndNormalizesOriginal(t *testing.T) {
	for _, protocol := range []string{"http", "socks5"} {
		t.Run(protocol, func(t *testing.T) {
			a := ticketAccountWithProxy()
			a.Proxy.Protocol = protocol
			equivalent := a.Proxy.URL() + "/?unused=1#fragment"
			if protocol == "socks5" {
				equivalent = strings.Replace(equivalent, "socks5://", "socks5h://", 1)
			}
			svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, HarvestLoopEnabled: true, HarvestProxyURL: "http://removed.example:8080"}, nil)
			u := &ticketProxyRecordingUpstream{do: func(n int, _ *http.Request) (*http.Response, error) {
				if n == 1 {
					svc.cfg.Gateway.OpenAICodexTicket.HarvestProxyURL = equivalent + ",http://new.example:8080"
					return nil, io.EOF
				}
				return codexTicketResponse(), nil
			}}
			svc.accountRepo, svc.httpUpstream = &ticketTestPassthroughRepo{account: a}, u
			svc.probeOnceOpenAICodexTicket(context.Background(), a, "gpt-6-astra")
			require.Equal(t, []string{a.Proxy.URL(), "http://new.example:8080"}, u.proxies)
		})
	}
	require.True(t, sameOpenAICodexTicketProxy("http://HOST:80/", "http://host"))
	require.True(t, sameOpenAICodexTicketProxy("https://host:443", "https://host"))
	require.True(t, sameOpenAICodexTicketProxy("socks5://host:1080", "socks5h://host"))
	require.False(t, sameOpenAICodexTicketProxy("http://a:p@host:80", "http://b:p@host:80"))
}

func TestCodexTicketSourceDetectsProxyConfigChanges(t *testing.T) {
	a := ticketAccountWithProxy()
	id, revision := *a.ProxyID, a.Proxy.UpdatedAt
	a.Extra[openAICodexTicketExtraKey("gpt-6-astra")] = &openAICodexTicket{
		State: fakeCodexTicketState(292), Length: 292, ExpiresAt: time.Now().Add(time.Hour),
		Source: "account_proxy", SourceProxyID: &id, SourceProxyUpdatedAt: &revision,
	}
	cfg := config.OpenAICodexTicketConfig{Enabled: true, Models: []string{"gpt-6-astra"}}
	require.False(t, OpenAICodexTicketStatuses(a, cfg, time.Now())[0].SourceProxyChanged)
	a.Proxy.UpdatedAt = a.Proxy.UpdatedAt.Add(time.Minute)
	require.True(t, OpenAICodexTicketStatuses(a, cfg, time.Now())[0].SourceProxyChanged, "同编号修改代理配置也要可见")
	a.Proxy.UpdatedAt = revision
	a.ProxyID = nil
	require.True(t, OpenAICodexTicketStatuses(a, cfg, time.Now())[0].SourceProxyChanged)
}
