//go:build unit

package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// Concurrency<=0 在默认的 account_proxy 隔离下等于不限制 maxConnsPerHost，
// 单个账号可以被灌进任意多条并发流。天花板同时兜住"未配置"和"配得过大"。
func TestEffectiveUpstreamConcurrencyAppliesCeiling(t *testing.T) {
	require.Equal(t, 16, (&Account{Concurrency: 0}).EffectiveUpstreamConcurrency(16))
	require.Equal(t, 16, (&Account{Concurrency: -1}).EffectiveUpstreamConcurrency(16))
	require.Equal(t, 16, (&Account{Concurrency: 64}).EffectiveUpstreamConcurrency(16))
	require.Equal(t, 4, (&Account{Concurrency: 4}).EffectiveUpstreamConcurrency(16), "低于天花板的显式配置不被抬高")
	require.Equal(t, 16, (&Account{Concurrency: 16}).EffectiveUpstreamConcurrency(16))
}

func TestEffectiveUpstreamConcurrencyCeilingDisabled(t *testing.T) {
	require.Equal(t, 0, (&Account{Concurrency: 0}).EffectiveUpstreamConcurrency(0), "天花板关闭时沿用账号自身配置")
	require.Equal(t, 64, (&Account{Concurrency: 64}).EffectiveUpstreamConcurrency(0))
	require.Equal(t, 64, (&Account{Concurrency: 64}).EffectiveUpstreamConcurrency(-1))
	require.Equal(t, 0, (*Account)(nil).EffectiveUpstreamConcurrency(16))
}

// API Key 账号是用户自带额度，配额与风控由上游按 key 计量，不在网关侧收敛。
func TestOpenAIUpstreamConcurrencyOnlyClampsOAuth(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	svc.cfg.Gateway.OpenAIOAuthConcurrencyCeiling = 16

	oauth := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 0}
	require.Equal(t, 16, svc.openAIUpstreamConcurrency(oauth))

	setupToken := &Account{Platform: PlatformOpenAI, Type: AccountTypeSetupToken, Concurrency: 999}
	require.Equal(t, 16, svc.openAIUpstreamConcurrency(setupToken))

	apiKey := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 0}
	require.Equal(t, 0, svc.openAIUpstreamConcurrency(apiKey))

	svc.cfg.Gateway.OpenAIOAuthConcurrencyCeiling = 0
	require.Equal(t, 0, svc.openAIUpstreamConcurrency(oauth), "配置为 0 时关闭天花板")
}

// cfg 缺失时不能退化成"无上限"：默认值仍需生效。
func TestOpenAIUpstreamConcurrencyFallsBackToDefaultCeiling(t *testing.T) {
	svc := &OpenAIGatewayService{}
	oauth := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 0}
	require.Equal(t, config.DefaultOpenAIOAuthConcurrencyCeiling, svc.openAIUpstreamConcurrency(oauth))
	require.Equal(t, 0, svc.openAIUpstreamConcurrency(nil))
}
