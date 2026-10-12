//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

// enableOpenAIAutoDisableTrainingForTest 打开进程级开关并在测试结束后恢复。
// 调用方不能是顶层 t.Parallel()：顶层串行测试不会与并行测试重叠。
func enableOpenAIAutoDisableTrainingForTest(t *testing.T, enabled bool) {
	t.Helper()
	previous := OpenAIAutoDisableTrainingEnabled()
	SetOpenAIAutoDisableTrainingEnabled(enabled)
	t.Cleanup(func() { SetOpenAIAutoDisableTrainingEnabled(previous) })
}

func TestAdminService_EnsureOpenAIPrivacy_RetriesNonSuccessModes(t *testing.T) {
	enableOpenAIAutoDisableTrainingForTest(t, true)

	for _, mode := range []string{PrivacyModeFailed, PrivacyModeCFBlocked} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			privacyCalls := 0
			svc := &adminServiceImpl{
				accountRepo: &mockAccountRepoForGemini{},
				privacyClientFactory: func(proxyURL string) (*req.Client, error) {
					privacyCalls++
					return nil, errors.New("factory failed")
				},
			}

			account := &Account{
				ID:       101,
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Credentials: map[string]any{
					"access_token": "token-1",
				},
				Extra: map[string]any{
					"privacy_mode": mode,
				},
			}

			got := svc.EnsureOpenAIPrivacy(context.Background(), account)

			require.Equal(t, PrivacyModeFailed, got)
			require.Equal(t, 1, privacyCalls)
		})
	}
}

func TestTokenRefreshService_ensureOpenAIPrivacy_RetriesNonSuccessModes(t *testing.T) {
	enableOpenAIAutoDisableTrainingForTest(t, true)

	cfg := &config.Config{
		TokenRefresh: config.TokenRefreshConfig{
			MaxRetries:          1,
			RetryBackoffSeconds: 0,
		},
	}

	for _, mode := range []string{PrivacyModeFailed, PrivacyModeCFBlocked} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			service := NewTokenRefreshService(&tokenRefreshAccountRepo{}, nil, nil, nil, nil, nil, nil, cfg, nil)
			privacyCalls := 0
			service.SetPrivacyDeps(func(proxyURL string) (*req.Client, error) {
				privacyCalls++
				return nil, errors.New("factory failed")
			}, nil)

			account := &Account{
				ID:       202,
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Credentials: map[string]any{
					"access_token": "token-2",
				},
				Extra: map[string]any{
					"privacy_mode": mode,
				},
			}

			service.ensureOpenAIPrivacy(context.Background(), account)

			require.Equal(t, 1, privacyCalls)
		})
	}
}

// 默认（开关关闭）时，自动流程不得调用训练数据共享 PATCH：新建/刷新/管理端刷新都只走 Ensure，
// 不会把管理员手动打开的共享设置改回去。
func TestOpenAIPrivacy_AutoDisableTrainingOffByDefault(t *testing.T) {
	enableOpenAIAutoDisableTrainingForTest(t, false)

	privacyCalls := 0
	factory := func(proxyURL string) (*req.Client, error) {
		privacyCalls++
		return nil, errors.New("factory failed")
	}
	newAccount := func() *Account {
		return &Account{
			ID:          303,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeOAuth,
			Credentials: map[string]any{"access_token": "token-3"},
		}
	}

	admin := &adminServiceImpl{accountRepo: &mockAccountRepoForGemini{}, privacyClientFactory: factory}
	require.Equal(t, "", admin.EnsureOpenAIPrivacy(context.Background(), newAccount()))

	cfg := &config.Config{TokenRefresh: config.TokenRefreshConfig{MaxRetries: 1}}
	refresher := NewTokenRefreshService(&tokenRefreshAccountRepo{}, nil, nil, nil, nil, nil, nil, cfg, nil)
	refresher.SetPrivacyDeps(factory, nil)
	refresher.ensureOpenAIPrivacy(context.Background(), newAccount())

	oauth := &OpenAIOAuthService{privacyClientFactory: factory}
	info := &OpenAITokenInfo{AccessToken: "token-3", PlanType: "pro"}
	oauth.enrichTokenInfo(context.Background(), info, "", true)
	require.Equal(t, "", info.PrivacyMode)

	// 唯一一次客户端构造来自导入路径的 accounts/check 查询，没有隐私 PATCH。
	require.Equal(t, 1, privacyCalls)

	// 手动按钮（ForceOpenAIPrivacy）不受开关影响。
	require.Equal(t, PrivacyModeFailed, admin.ForceOpenAIPrivacy(context.Background(), newAccount()))
}

// 刷新路径：id_token 已带 plan_type 时不再查询 accounts/check / subscriptions。
func TestOpenAIOAuth_RefreshSkipsAccountLookupWhenPlanKnown(t *testing.T) {
	calls := 0
	oauth := &OpenAIOAuthService{privacyClientFactory: func(string) (*req.Client, error) {
		calls++
		return nil, errors.New("factory failed")
	}}
	oauth.enrichTokenInfo(context.Background(), &OpenAITokenInfo{AccessToken: "t", PlanType: "plus"}, "", false)
	require.Equal(t, 0, calls)

	// id_token 缺 plan_type（Mobile RT）时仍回退查询。
	oauth.enrichTokenInfo(context.Background(), &OpenAITokenInfo{AccessToken: "t"}, "", false)
	require.Greater(t, calls, 0)
}
