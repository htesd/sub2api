package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type migrationTestRepo struct {
	AccountRepository
	a        *Account
	p        Proxy
	commits  int
	stored   int
	state    CodexMigrationState
	storeErr error
}

func (r *migrationTestRepo) GetByID(context.Context, int64) (*Account, error) {
	a := *r.a
	a.Extra = maps.Clone(r.a.Extra)
	a.Credentials = maps.Clone(r.a.Credentials)
	return &a, nil
}
func (r *migrationTestRepo) ListCodexMigrationProxies(context.Context) ([]Proxy, error) {
	return []Proxy{r.p}, nil
}
func (r *migrationTestRepo) CommitCodexMigration(_ context.Context, a *Account, p *Proxy, t any, state CodexMigrationState) (bool, error) {
	if !codexAutoMigrate(r.a) || codexProxyID(r.a) != codexProxyID(a) {
		return false, nil
	}
	r.commits++
	r.a.Proxy = p
	r.a.ProxyID = &p.ID
	r.a.Extra[CodexMigrationEpochKey] = t.(*openAICodexTicket).RouteEpoch
	r.a.Extra[openAICodexTicketExtraKey(openAICodexTicketDefaultModel)] = t
	r.state = state
	return true, nil
}
func (r *migrationTestRepo) StoreCodexTicketForRoute(_ context.Context, a *Account, key string, t any) (bool, error) {
	if r.storeErr != nil {
		return false, r.storeErr
	}
	if codexProxyID(a) != codexProxyID(r.a) || codexMigrationEpoch(a) != codexMigrationEpoch(r.a) {
		return false, nil
	}
	r.stored++
	r.a.Extra[key] = t
	return true, nil
}
func (r *migrationTestRepo) RecordCodexMigrationState(_ context.Context, _ *Account, state CodexMigrationState) error {
	r.state = state
	return nil
}

func migrationSSE(model, terminal string) string {
	return `data: {"type":"response.created","response":{"model":"` + model + `"}}` + "\n\n" + `data: {"type":"` + terminal + `","response":{"model":"` + model + `","status":"completed"}}` + "\n\n"
}

func TestCodexMigrationCompleteEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"astra", migrationSSE("gpt-6-astra", "response.completed"), true},
		{"luna", migrationSSE("gpt-5.6-luna", "response.completed"), false},
		{"failed", migrationSSE("gpt-6-astra", "response.failed"), false},
		{"no_terminal", `data: {"type":"response.created","response":{"model":"gpt-6-astra"}}` + "\n\n", false},
		{"conflict", `data: {"type":"response.created","response":{"model":"gpt-5.6-luna"}}` + "\n\n" + migrationSSE("gpt-6-astra", "response.completed"), false},
		{"malformed", "data: invalid\n\n" + migrationSSE("gpt-6-astra", "response.completed"), false},
		{"error", `data: {"type":"error"}` + "\n\n" + migrationSSE("gpt-6-astra", "response.completed"), false},
		{"top_level_conflict", `data: {"type":"response.created","response":{"model":"gpt-6-astra"}}` + "\n\n" + `data: {"type":"response.completed","model":"gpt-5.6-luna","response":{"status":"completed"}}` + "\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := readCodexMigrationModel(strings.NewReader(tc.body))
			require.Equal(t, tc.valid, ok)
		})
	}
}

func TestCodexMigrationSameRegionAndHotRevocation(t *testing.T) {
	for _, scenario := range []string{"success", "cross_region", "unknown_region", "candidate_luna", "original_mint_ok", "revoke", "proxy_changed", "unstable_ip", "original_ok", "original_no_ticket", "original_wrong_ticket", "429", "candidate_429"} {
		t.Run(scenario, func(t *testing.T) {
			a := ticketAccountWithProxy()
			a.Proxy.Status = StatusActive
			a.Extra[CodexAutoMigrateKey] = true
			repo := &migrationTestRepo{a: a, p: Proxy{ID: 101, Protocol: "http", Host: "candidate.example", Port: 8080, Status: StatusActive, UpdatedAt: time.Now().UTC()}}
			posts := 0
			traces := 0
			session := ""
			ticket := fakeCodexTicketState(292)
			var u *ticketProxyRecordingUpstream
			u = &ticketProxyRecordingUpstream{do: func(n int, req *http.Request) (*http.Response, error) {
				body := ""
				headers := http.Header{}
				code := 200
				if req.Method == "GET" {
					require.Empty(t, req.Header.Get("Authorization"))
					traces++
					country := "SG"
					ip := "192.0.2.1"
					if traces > 1 {
						ip = "192.0.2.2"
						if scenario == "cross_region" {
							country = "US"
						}
						if scenario == "unknown_region" {
							country = "XX"
						}
					}
					if traces == 3 && scenario == "unstable_ip" {
						ip = "192.0.2.3"
					}
					body = "ip=" + ip + "\nloc=" + country + "\n"
				} else {
					posts++
					if session == "" {
						session = req.Header.Get("session_id")
					}
					require.Equal(t, session, req.Header.Get("session_id"))
					var b map[string]any
					require.NoError(t, json.NewDecoder(req.Body).Decode(&b))
					require.Equal(t, "gpt-6-astra", b["model"])
					candidate := strings.Contains(u.proxies[n-1], "candidate.example")
					carried := req.Header.Get(openAICodexTurnStateHeader)
					if carried != "" {
						// 复验必须携带刚采到的那张票，不能是别处的票。
						require.Equal(t, ticket, carried)
					}
					// 无票的首个请求只是采票，上游按真实表现返回降级模型；
					// 只有携票复验才可能拿到完整 astra。
					model := "gpt-5.6-luna"
					switch {
					case scenario == "original_ok" || scenario == "original_no_ticket" || scenario == "original_wrong_ticket":
						model = "gpt-6-astra"
					case scenario == "original_mint_ok":
						if !candidate && carried != "" {
							model = "gpt-6-astra"
						}
					case candidate && carried != "" && scenario != "candidate_luna":
						model = "gpt-6-astra"
					}
					if posts == 1 && scenario == "429" {
						code = 429
					}
					if candidate && carried == "" && scenario == "candidate_429" {
						code = 429
					}
					if candidate && carried == "" && scenario == "revoke" {
						a.Extra[CodexAutoMigrateKey] = false
					}
					if candidate && carried == "" && scenario == "proxy_changed" {
						a.Proxy = &Proxy{ID: 102, Protocol: "http", Host: "new.example", Port: 8080}
						a.ProxyID = &a.Proxy.ID
					}
					headers.Set(openAICodexTurnStateHeader, ticket)
					if posts == 1 && scenario == "original_no_ticket" {
						headers.Del(openAICodexTurnStateHeader)
					}
					if posts == 1 && scenario == "original_wrong_ticket" {
						headers.Set(openAICodexTurnStateHeader, fakeCodexTicketState(332))
					}
					body = migrationSSE(model, "response.completed")
				}
				return &http.Response{StatusCode: code, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
			}}
			svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, HarvestLoopEnabled: true, HarvestProxyURL: repo.p.URL()}, u)
			svc.accountRepo = repo
			svc.probeOnceOpenAICodexTicket(context.Background(), a, "gpt-6-astra")
			// 新不变量：候选出口采到合格票即切换，采票响应是否 astra 不再有否决权
			// （降级由请求期的降智阻断兜住），因此 candidate_luna 同样必须提交。
			// 出口地理一致性不再是切换前置条件：跨国、地区未知、IP 不稳定一律照切，
			// 拿到真模型优先于出口画像稳定。
			switch scenario {
			case "success", "candidate_luna", "cross_region", "unknown_region", "unstable_ip":
				require.Equal(t, 1, repo.commits)
				require.Equal(t, int64(101), *a.ProxyID)
				require.NotEmpty(t, codexMigrationEpoch(a))
				if scenario == "success" {
					// 原代理采票+复验两发，候选仅采票一发，共三次上游请求。
					require.Equal(t, 3, posts)
					// 记录的是候选采票响应模型：无票采票请求上游本就多返回 luna。
					require.Equal(t, "gpt-5.6-luna", repo.state.ResponseModel)
				}
			default:
				require.Zero(t, repo.commits)
			}
			if scenario == "429" {
				require.Equal(t, 1, posts)
				require.Equal(t, "upstream_limited", repo.state.Result)
			}
			if scenario == "original_ok" {
				require.Equal(t, 1, posts)
				require.Equal(t, 1, repo.stored)
				require.Equal(t, "current_proxy_ok", repo.state.Result)
			}
			if scenario == "original_mint_ok" {
				// 原出口本来就能用，只是缺票；采票复验通过后留在原代理。
				require.Equal(t, 2, posts)
				require.Equal(t, 1, repo.stored)
				require.Equal(t, "current_proxy_ok", repo.state.Result)
				require.Equal(t, int64(100), *a.ProxyID)
			}
			if scenario == "original_no_ticket" || scenario == "original_wrong_ticket" {
				require.Equal(t, 1, posts)
				require.Equal(t, "current_proxy_ok_no_ticket", repo.state.Result)
			}
			if scenario == "candidate_luna" {
				// 候选采票响应是 luna 也照切：票与出口同源优先，降级交给降智阻断处理。
				require.Equal(t, "migrated", repo.state.Result)
			}
			if scenario == "candidate_429" {
				require.Equal(t, 3, posts)
				require.Equal(t, "upstream_limited", repo.state.Result)
			}
		})
	}
}

func TestCodexMigrationPersistenceFailurePreservesLegacyFallback(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		a := ticketAccountWithProxy()
		a.Extra[CodexAutoMigrateKey] = enabled
		repo := &migrationTestRepo{a: a, storeErr: errors.New("database unavailable")}
		svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true}, nil)
		svc.accountRepo = repo
		ticket := newCodexMigrationTicket(a, a.Proxy, fakeCodexTicketState(292), 3600)
		require.Equal(t, !enabled, svc.storeOpenAICodexTicket(context.Background(), a, ticket))
		_, exists := svc.openaiCodexTickets.Load(openAICodexTicketKey(a.ID, ticket.Model))
		require.Equal(t, !enabled, exists)
	}
}

func TestCodexMigrationRejectsOldRouteMemoryAndProtectsState(t *testing.T) {
	a := ticketAccountWithProxy()
	a.Extra[CodexMigrationEpochKey] = "new"
	a.Extra[CodexAutoMigrateKey] = false
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true}, nil)
	old := &openAICodexTicket{State: fakeCodexTicketState(292), Length: 292, ExpiresAt: time.Now().Add(time.Hour), RouteEpoch: "old"}
	svc.openaiCodexTickets.Store(openAICodexTicketKey(a.ID, "gpt-6-astra"), old)
	require.Nil(t, svc.lookupOpenAICodexTicket(a, "gpt-6-astra"))
	out := MergeOpenAICodexTicketExtra(map[string]any{CodexMigrationEpochKey: "spoof", CodexMigrationStateKey: "spoof", CodexAutoMigrateKey: true}, a.Extra)
	require.Equal(t, "new", out[CodexMigrationEpochKey])
	require.NotContains(t, out, CodexMigrationStateKey)
	require.Equal(t, true, out[CodexAutoMigrateKey])
}
