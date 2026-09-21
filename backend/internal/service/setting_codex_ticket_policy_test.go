package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type codexTicketPolicyBarrierRepo struct {
	*codexTicketSettingRepo
	started chan struct{}
	release chan struct{}
}

func (r *codexTicketPolicyBarrierRepo) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	close(r.started)
	select {
	case <-r.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return map[string]string{SettingKeyOpenAICodexTicketHarvestLoopEnabled: "true", SettingKeyOpenAICodexTicketRequiredModels: ""}, nil
}

func TestCodexTicketPolicyOldLoadCannotOverwriteSave(t *testing.T) {
	base := &codexTicketSettingRepo{codexPolicyMigrationRepoStub: &codexPolicyMigrationRepoStub{values: map[string]string{}}}
	repo := &codexTicketPolicyBarrierRepo{codexTicketSettingRepo: base, started: make(chan struct{}), release: make(chan struct{})}
	settings := NewSettingService(repo, &config.Config{})
	done := make(chan config.OpenAICodexTicketConfig, 1)
	go func() {
		done <- settings.ResolveOpenAICodexTicketConfig(context.Background(), config.OpenAICodexTicketConfig{})
	}()
	<-repo.started
	// 另一个调用方等待同一查询时，取消必须立即返回。
	ctx, cancel := context.WithCancel(context.Background())
	waiter := make(chan struct{})
	go func() { settings.ResolveOpenAICodexTicketConfig(ctx, config.OpenAICodexTicketConfig{}); close(waiter) }()
	cancel()
	select {
	case <-waiter:
	case <-time.After(time.Second):
		t.Fatal("取消无法打断共享查询等待")
	}
	settings.publishCodexTicketPolicy(&SystemSettings{OpenAICodexTicketHarvestLoopEnabled: false, OpenAICodexTicketRequiredModels: "gpt-6-astra"})
	close(repo.release)
	select {
	case cfg := <-done:
		require.False(t, cfg.HarvestLoopEnabled)
		require.Equal(t, []string{"gpt-6-astra"}, cfg.RequiredModels)
	case <-time.After(time.Second):
		t.Fatal("查询未完成")
	}
	cfg := settings.ResolveOpenAICodexTicketConfig(context.Background(), config.OpenAICodexTicketConfig{})
	require.False(t, cfg.HarvestLoopEnabled)
	require.Equal(t, []string{"gpt-6-astra"}, cfg.RequiredModels)
}

func TestCodexTicketPolicyReadFailureKeepsSafePolicy(t *testing.T) {
	repo := &codexTicketSettingRepo{codexPolicyMigrationRepoStub: &codexPolicyMigrationRepoStub{values: map[string]string{}}, err: errors.New("database down")}
	settings := NewSettingService(repo, &config.Config{})
	cfg := settings.ResolveOpenAICodexTicketConfig(context.Background(), config.OpenAICodexTicketConfig{HarvestLoopEnabled: true})
	require.False(t, cfg.HarvestLoopEnabled)
	require.True(t, openAICodexTicketModelRequired(cfg, "gpt-6-astra"))
	settings.openAICodexTicketPolicyCache.Store(&cachedCodexTicketPolicy{values: map[string]string{SettingKeyOpenAICodexTicketHarvestLoopEnabled: "false", SettingKeyOpenAICodexTicketRequiredModels: "gpt-6-astra"}})
	cfg = settings.ResolveOpenAICodexTicketConfig(context.Background(), config.OpenAICodexTicketConfig{HarvestLoopEnabled: true})
	require.False(t, cfg.HarvestLoopEnabled)
	require.True(t, openAICodexTicketModelRequired(cfg, "gpt-6-astra"))
	require.False(t, openAICodexTicketModelRequired(cfg, "gpt-5.6-sol"))
}

func TestCodexTicketPolicyHotUpdate(t *testing.T) {
	repo := &codexTicketSettingRepo{codexPolicyMigrationRepoStub: &codexPolicyMigrationRepoStub{values: map[string]string{
		SettingKeyOpenAICodexTicketEnabled:            "true",
		SettingKeyOpenAICodexTicketHarvestLoopEnabled: "false",
		SettingKeyOpenAICodexTicketRequiredModels:     "gpt-6-astra",
		SettingKeyOpenAICodexTicketHarvestProxyURL:    "http://db.example:8080",
	}}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, HarvestLoopEnabled: false, HarvestProxyURL: "http://yaml.example:8080"}, nil)
	svc.settingService = NewSettingService(repo, svc.cfg)
	account := ticketTestAccount(41)
	account.LastUsedAt = nil // 名单不会因闲置放行，也不能因此无法主动取票。
	require.ErrorIs(t, svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", http.Header{}), ErrOpenAICodexTicketUnavailable)
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), account, "gpt-5.6-sol", http.Header{}))
	require.True(t, svc.openAICodexTicketBlocksAccount(account, "gpt-6-astra"))
	require.False(t, svc.openAICodexTicketBlocksAccount(account, "gpt-5.6-sol"))
	cfg := svc.openAICodexTicketConfig()
	require.False(t, cfg.HarvestLoopEnabled)
	require.Equal(t, "http://db.example:8080", cfg.HarvestProxyURL)
	states := OpenAICodexTicketStatuses(account, cfg, time.Now())
	require.True(t, states[0].Blocked)
	require.False(t, states[1].Blocked)

	repo.values[SettingKeyOpenAICodexTicketHarvestLoopEnabled] = "true"
	repo.values[SettingKeyOpenAICodexTicketRequiredModels] = ""
	svc.settingService.openAICodexTicketPolicyCache.Store(&cachedCodexTicketPolicy{})
	require.True(t, svc.openAICodexTicketConfig().HarvestLoopEnabled)
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", http.Header{}))
	require.False(t, svc.openAICodexTicketBlocksAccount(account, "gpt-6-astra"))
	require.False(t, svc.cfg.Gateway.OpenAICodexTicket.HarvestLoopEnabled, "不能修改共享启动配置")
}

func TestCodexTicketLoopCanEnableAfterDisabledStartup(t *testing.T) {
	// 通过线程安全缓存模拟一次设置刷新，确认同一后台循环从关闭变为开启。
	repo := &codexTicketSettingRepo{codexPolicyMigrationRepoStub: &codexPolicyMigrationRepoStub{values: map[string]string{
		SettingKeyOpenAICodexTicketEnabled:         "true",
		SettingKeyOpenAICodexTicketHarvestProxyURL: "http://proxy.example:8080",
	}}}
	account := ticketTestAccount(41)
	account.Status = StatusActive
	account.LastUsedAt = nil
	called := make(chan struct{}, 1)
	upstream := &codexTicketFuncUpstream{do: func(req *http.Request) (*http.Response, error) {
		select {
		case called <- struct{}{}:
		default:
		}
		return codexTicketResponse(), nil
	}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, Models: []string{"gpt-6-astra"}, HarvestProbeIntervalSeconds: 1}, upstream)
	svc.accountRepo = &codexTicketLifecycleRepo{account: *account}
	svc.settingService = NewSettingService(repo, svc.cfg)
	setPolicy := func(enabled string) {
		svc.settingService.openAICodexTicketPolicyCache.Store(&cachedCodexTicketPolicy{values: map[string]string{
			SettingKeyOpenAICodexTicketHarvestLoopEnabled: enabled,
			SettingKeyOpenAICodexTicketRequiredModels:     "gpt-6-astra",
		}, expiresAt: time.Now().Add(time.Minute)})
	}
	setPolicy("false")
	svc.StartOpenAICodexTicketHarvester()
	t.Cleanup(svc.StopOpenAICodexTicketHarvester)
	select {
	case <-called:
		t.Fatal("关闭状态仍发送探测")
	case <-time.After(100 * time.Millisecond):
	}
	setPolicy("true")
	select {
	case <-called:
	case <-time.After(3 * time.Second):
		t.Fatal("热开启未启动探测")
	}
	setPolicy("false")
	select {
	case <-called:
	default:
	}
	svc.probeOnceOpenAICodexTicket(context.Background(), account, "gpt-6-astra")
	select {
	case <-called:
		t.Fatal("热关闭后仍发送探测")
	default:
	}
}
