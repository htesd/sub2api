package service

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func (s *OpenAIGatewayService) SetPluginManager(manager *PluginManager) {
	s.pluginManager = manager
}

// doOpenAIUpstream 只在 OpenAI OAuth 能力绑定已启用时把真实请求交给插件。
// 插件返回标准 http.Response，响应解析、错误映射、SSE 和计费仍由现有核心链处理。
func (s *OpenAIGatewayService) doOpenAIUpstream(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	markCodexCapacitySent(request.Context())
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	return s.httpUpstream.Do(request, proxyURL, account.ID, s.openAIUpstreamConcurrency(account))
}

// openAIUpstreamConcurrency 对 OAuth 账号施加上游并发天花板。API Key 账号是
// 用户自带额度，配额与风控由上游按 key 计量，不在此收敛。
func (s *OpenAIGatewayService) openAIUpstreamConcurrency(account *Account) int {
	if account == nil {
		return 0
	}
	if !isOpenAIOAuthAccount(account) {
		return account.Concurrency
	}
	ceiling := config.DefaultOpenAIOAuthConcurrencyCeiling
	if s != nil && s.cfg != nil {
		ceiling = s.cfg.Gateway.OpenAIOAuthConcurrencyCeiling
	}
	return account.EffectiveUpstreamConcurrency(ceiling)
}

// doOpenAIAccountTestUpstream 让 OpenAI OAuth 账号测试与真实转发使用同一插件路径。
// API Key 和未命中插件的账号保持各自原有的 HTTPUpstream 行为。
func (s *AccountTestService) doOpenAIAccountTestUpstream(
	request *http.Request,
	proxyURL string,
	account *Account,
	useTLSFallback bool,
) (*http.Response, error) {
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	if useTLSFallback {
		return s.httpUpstream.DoWithTLS(
			request,
			proxyURL,
			account.ID,
			account.Concurrency,
			s.tlsFPProfileService.ResolveTLSProfile(account),
		)
	}
	return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
}
