package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type codexTicketFuncUpstream struct {
	HTTPUpstream
	do func(*http.Request) (*http.Response, error)
}

func (u *codexTicketFuncUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.do(req)
}
func codexTicketResponse() *http.Response {
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, fakeCodexTicketState(292))
	return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(strings.NewReader("data: {}\n\n"))}
}

func TestCodexTicketProbeBypassesPluginDuringWiring(t *testing.T) {
	manager := &PluginManager{}
	manager.route.Store(&pluginRoute{pluginID: 1, rolloutPercent: 100, unavailable: "plugin must not handle synthetic probes"})
	var calls atomic.Int64
	upstream := &codexTicketFuncUpstream{do: func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if HTTPUpstreamProfileFromContext(req.Context()) != HTTPUpstreamProfileOpenAIHarvest || !req.Close {
			return nil, errors.New("missing no-reuse transport profile")
		}
		return codexTicketResponse(), nil
	}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true}, upstream)
	svc.SetPluginManager(manager)
	account := ticketTestAccount(41)
	// This binding rejects ordinary traffic; harvesting still uses the dedicated transport.
	request, _ := http.NewRequest(http.MethodPost, "https://example.com", nil)
	_, err := svc.doOpenAIUpstream(request, "", account)
	require.Error(t, err)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 200; i++ {
			svc.SetPluginManager(manager)
		}
	}()
	close(start)
	for i := 0; i < 20; i++ {
		state, status, err := svc.fireOpenAICodexTicketProbe(context.Background(), account, "test-token", "gpt-6-astra", "http://proxy.example.com:8080", time.Second)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, status)
		require.Len(t, state, 292)
	}
	wg.Wait()
	require.Equal(t, int64(20), calls.Load())
}

type codexTicketLifecycleRepo struct {
	AccountRepository
	account Account
	list    func(context.Context) ([]Account, error)
	persist func(context.Context) error
}

// GetByID 供探测前的撤销复核使用（probeOnceOpenAICodexTicket）。返回与
// ListByPlatform 同一份账号，表示"开关未变"。
func (r *codexTicketLifecycleRepo) GetByID(_ context.Context, _ int64) (*Account, error) {
	acc := r.account
	return &acc, nil
}

func (r *codexTicketLifecycleRepo) ListByPlatform(ctx context.Context, _ string) ([]Account, error) {
	if r.list != nil {
		return r.list(ctx)
	}
	return []Account{r.account}, nil
}
func (r *codexTicketLifecycleRepo) UpdateExtra(ctx context.Context, _ int64, _ map[string]any) error {
	if r.persist != nil {
		return r.persist(ctx)
	}
	return nil
}

type codexTicketLifecycleSettings struct {
	SettingRepository
	get func(context.Context, string) (string, error)
}

func (r *codexTicketLifecycleSettings) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	return map[string]string{}, nil
}

func (r *codexTicketLifecycleSettings) GetValue(ctx context.Context, key string) (string, error) {
	return r.get(ctx, key)
}

func TestCodexTicketHarvesterStopCancelsInFlightWork(t *testing.T) {
	for _, stage := range []string{"settings-enabled", "settings-proxy", "accounts", "upstream", "persist"} {
		t.Run(stage, func(t *testing.T) {
			started := make(chan struct{})
			cancelled := make(chan struct{})
			var once sync.Once
			block := func(ctx context.Context) error {
				once.Do(func() { close(started) })
				<-ctx.Done()
				close(cancelled)
				return ctx.Err()
			}
			account := ticketTestAccount(41)
			account.Status = StatusActive
			repo := &codexTicketLifecycleRepo{account: *account}
			upstream := &codexTicketFuncUpstream{do: func(req *http.Request) (*http.Response, error) {
				if stage == "upstream" {
					return nil, block(req.Context())
				}
				return codexTicketResponse(), nil
			}}
			svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, HarvestLoopEnabled: true, HarvestProxyURL: "http://proxy.example.com:8080", HarvestAttemptTimeoutSeconds: 25, Models: []string{"gpt-6-astra"}}, upstream)
			svc.accountRepo = repo
			if stage == "accounts" {
				repo.list = func(ctx context.Context) ([]Account, error) { return nil, block(ctx) }
			}
			if stage == "persist" {
				repo.persist = block
			}
			if strings.HasPrefix(stage, "settings-") {
				svc.settingService = NewSettingService(&codexTicketLifecycleSettings{get: func(ctx context.Context, key string) (string, error) {
					if stage == "settings-enabled" && key == SettingKeyOpenAICodexTicketEnabled || stage == "settings-proxy" && key == SettingKeyOpenAICodexTicketHarvestProxyURL {
						return "", block(ctx)
					}
					if key == SettingKeyOpenAICodexTicketEnabled {
						return "true", nil
					}
					return "", ErrSettingNotFound
				}}, svc.cfg)
			}
			svc.StartOpenAICodexTicketHarvester()
			t.Cleanup(svc.StopOpenAICodexTicketHarvester)
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("harvester did not reach " + stage)
			}
			// Repeated start must not create a second loop or overwrite the cancellation state.
			svc.StartOpenAICodexTicketHarvester()
			stopped := make(chan struct{})
			go func() { svc.StopOpenAICodexTicketHarvester(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("stop waited for the probe timeout")
			}
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				t.Fatal("in-flight operation did not receive cancellation")
			}
			svc.StopOpenAICodexTicketHarvester()
			svc.StartOpenAICodexTicketHarvester()
		})
	}
}

type codexTicketHeaderOnlyBody struct{ reads, closes int }

func (b *codexTicketHeaderOnlyBody) Read([]byte) (int, error) { b.reads++; return 0, io.EOF }
func (b *codexTicketHeaderOnlyBody) Close() error             { b.closes++; return nil }
func TestCodexTicketProbeClosesStreamWithoutDraining(t *testing.T) {
	body := &codexTicketHeaderOnlyBody{}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{}, &codexTicketFuncUpstream{do: func(*http.Request) (*http.Response, error) {
		response := codexTicketResponse()
		response.Body = body
		return response, nil
	}})
	_, _, err := svc.fireOpenAICodexTicketProbe(context.Background(), ticketTestAccount(41), "test-token", "gpt-6-astra", "", time.Second)
	require.NoError(t, err)
	require.Zero(t, body.reads)
	require.Equal(t, 1, body.closes)
}

func TestCodexTicketPolicyExemptsCredentialShadows(t *testing.T) {
	parentID := int64(41)
	parent := ticketTestAccount(parentID)
	shadow := ticketTestAccount(42)
	shadow.ParentAccountID = &parentID
	shadow.Status = StatusActive
	cfg := config.OpenAICodexTicketConfig{Enabled: true, FailClosed: true, HarvestLoopEnabled: true, HarvestProxyURL: "http://proxy.example.com:8080"}
	upstream := &httpUpstreamRecorder{}
	svc := ticketTestService(t, cfg, upstream)
	svc.accountRepo = &codexTicketRefreshRepo{accounts: []Account{*shadow}}
	require.True(t, svc.openAICodexTicketBlocksAccount(parent, "gpt-6-astra"))
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
		shadow.Type = accountType
		require.False(t, svc.openAICodexTicketBlocksAccount(shadow, "gpt-6-astra"))
		headers := http.Header{}
		headers.Set(openAICodexTurnStateHeader, "client-state")
		require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), shadow, "gpt-6-astra", headers))
		require.Equal(t, "client-state", headers.Get(openAICodexTurnStateHeader))
		require.Empty(t, OpenAICodexTicketStatuses(shadow, cfg, time.Now()))
		svc.probeOnceOpenAICodexTicket(context.Background(), shadow, "gpt-6-astra")
	}
	svc.refreshOpenAICodexTickets(context.Background())
	require.Empty(t, upstream.requests)
}

// codexTicketRevokeRepo 模拟"排队期间管理员关掉开关"：ListByPlatform 返回开启
// 状态的快照，随后 GetByID 返回 latest（可在运行中切换）。
type codexTicketRevokeRepo struct {
	AccountRepository
	snapshot Account
	mu       sync.Mutex
	latest   Account
	getErr   error
	gone     bool
	getCalls atomic.Int64
}

func (r *codexTicketRevokeRepo) ListByPlatform(context.Context, string) ([]Account, error) {
	return []Account{r.snapshot}, nil
}

func (r *codexTicketRevokeRepo) GetByID(context.Context, int64) (*Account, error) {
	r.getCalls.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getErr != nil {
		return nil, r.getErr
	}
	if r.gone {
		return nil, nil
	}
	acc := r.latest
	return &acc, nil
}

func (r *codexTicketRevokeRepo) setLatest(mutate func(*Account)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	mutate(&r.latest)
}

func (r *codexTicketRevokeRepo) UpdateExtra(context.Context, int64, map[string]any) error {
	return nil
}

// 排队期间被撤销的账号不得再发出探测请求。
//
// 授权检查必须 fail-closed：查不到或查询出错时无法确认授权仍有效，只能放弃本轮。
// 这与业务侧 fail_closed 配置无关——放弃一发后台探测的代价极低，误发的代价是对一个
// 可能已被保护的账号动手。
func TestRefreshOpenAICodexTickets_RespectsOptOutWhileQueued(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*codexTicketRevokeRepo)
	}{
		{"总开关被关闭", func(r *codexTicketRevokeRepo) {
			r.setLatest(func(a *Account) {
				a.Extra = map[string]any{
					openAICodexTicketActiveProbeExtraKey: true,
					openAICodexTicketEnabledExtraKey:     false,
				}
			})
		}},
		{"主动探测被关闭", func(r *codexTicketRevokeRepo) {
			r.setLatest(func(a *Account) {
				a.Extra = map[string]any{openAICodexTicketActiveProbeExtraKey: false}
			})
		}},
		{"账号已不可调度", func(r *codexTicketRevokeRepo) {
			r.setLatest(func(a *Account) { a.Schedulable = false })
		}},
		{"账号已被删除", func(r *codexTicketRevokeRepo) {
			// 生产 repo 找不到账号时返回 ErrAccountNotFound，不是 (nil, nil)。
			// 桩必须照这个形状来，否则测不到真实的删除路径。
			r.mu.Lock()
			r.getErr = ErrAccountNotFound
			r.mu.Unlock()
		}},
		{"账号已被删除且 repo 返回空值", func(r *codexTicketRevokeRepo) {
			r.mu.Lock()
			r.gone = true
			r.mu.Unlock()
		}},
		{"授权复核查询失败", func(r *codexTicketRevokeRepo) {
			r.mu.Lock()
			r.getErr = errors.New("db down")
			r.mu.Unlock()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := *ticketTestAccount(41)
			snapshot.Status = StatusActive

			repo := &codexTicketRevokeRepo{snapshot: snapshot, latest: snapshot}
			tc.prepare(repo)

			upstream := &httpUpstreamRecorder{err: io.EOF}
			svc := ticketTestService(t, config.OpenAICodexTicketConfig{
				Enabled:            true,
				HarvestLoopEnabled: true,
				HarvestProxyURL:    "socks5h://harvest",
				Models:             []string{"gpt-6-astra"},
			}, upstream)
			svc.accountRepo = repo

			svc.refreshOpenAICodexTickets(context.Background())

			require.Positive(t, repo.getCalls.Load(),
				"发请求前必须以最新账号状态复核，否则撤销会被在飞任务绕过")
			require.Empty(t, upstream.requests,
				"排队期间被撤销/无法确认授权的账号不得再发出探测请求")
		})
	}

	// 正向对照：授权仍然有效时必须真的发出探测，否则上面的断言可能只是
	// "探测整体不工作"。
	t.Run("授权有效时正常探测", func(t *testing.T) {
		snapshot := *ticketTestAccount(41)
		snapshot.Status = StatusActive
		repo := &codexTicketRevokeRepo{snapshot: snapshot, latest: snapshot}
		upstream := &httpUpstreamRecorder{err: io.EOF}
		svc := ticketTestService(t, config.OpenAICodexTicketConfig{
			Enabled:            true,
			HarvestLoopEnabled: true,
			HarvestProxyURL:    "socks5h://harvest",
			Models:             []string{"gpt-6-astra"},
		}, upstream)
		svc.accountRepo = repo

		svc.refreshOpenAICodexTickets(context.Background())
		require.Len(t, upstream.requests, 1, "授权有效时应发出探测")
	})
}

// 复核必须发生在**取得并发槽之后**，而不是排队之前。
//
// 构造：并发上限 1、两个模型。任务 A 先拿到槽并在上游调用处阻塞，任务 B 在槽上排队；
// 此时把账号改成已撤销，再放行 A。若复核位置正确（槽之后），B 会看到最新状态并取消，
// 上游只收到 A 这一发；若复核被错误地移到排队之前，B 早已用旧快照通过检查，上游会
// 收到两发。
func TestRefreshOpenAICodexTickets_RechecksAfterQueueing(t *testing.T) {
	snapshot := *ticketTestAccount(41)
	snapshot.Status = StatusActive

	repo := &codexTicketRevokeRepo{snapshot: snapshot, latest: snapshot}

	firstInFlight := make(chan struct{})
	release := make(chan struct{})
	upstream := &codexTicketBarrierUpstream{firstInFlight: firstInFlight, release: release}

	// B 到达取槽边界的同步点。没有它的话，B 有可能在撤销之后才被调度到——那时
	// 即便复核被错误地放在取槽之前，B 也会读到已撤销的状态并取消，测试就成了偶然
	// 通过。等 GetByID 至少被调用过一次（A 的那次）再撤销，确保 B 的排队已经发生
	// 在撤销之前。
	waitForQueued := func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if repo.getCalls.Load() >= 1 {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("第二发探测未在预期时间内进入排队")
	}

	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:            true,
		HarvestLoopEnabled: true,
		HarvestProxyURL:    "socks5h://harvest",
		HarvestConcurrency: 1,
		Models:             []string{"gpt-6-astra", "gpt-5.6-sol"},
	}, upstream)
	svc.accountRepo = repo

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.refreshOpenAICodexTickets(context.Background())
	}()

	select {
	case <-firstInFlight:
	case <-time.After(5 * time.Second):
		t.Fatal("第一发探测未在预期时间内进入上游调用")
	}

	// A 占着唯一的并发槽，B 正在排队。此刻撤销。
	waitForQueued()
	repo.setLatest(func(a *Account) {
		a.Extra = map[string]any{openAICodexTicketActiveProbeExtraKey: false}
	})
	close(release)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("采集周期未结束")
	}

	require.Equal(t, int64(1), upstream.calls.Load(),
		"排队中的第二发必须在取得并发槽后复核到撤销并取消；若复核位于排队之前，这里会是 2")
}

// codexTicketBarrierUpstream 让第一发探测阻塞在上游调用处，用于制造真实排队。
type codexTicketBarrierUpstream struct {
	HTTPUpstream
	calls         atomic.Int64
	once          sync.Once
	firstInFlight chan struct{}
	release       chan struct{}
}

func (u *codexTicketBarrierUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls.Add(1)
	u.once.Do(func() {
		close(u.firstInFlight)
		select {
		case <-u.release:
		case <-req.Context().Done():
		}
	})
	return nil, io.EOF
}
