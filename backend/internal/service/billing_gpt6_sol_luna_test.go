//go:build unit

package service

import (
	"os"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// 三个价源（billing 兜底价、pricing 静态价卡、LiteLLM 价卡 JSON）对 GPT-6 Sol/Luna 必须给出同一套官方价，
// 且 272K 边界按"严格大于才整条请求加价"处理。移植自上游 PR #7509，去掉了 Opus 5.5 部分。
func TestGPT6SolLunaPricingSourcesAndLongContext(t *testing.T) {
	data, err := os.ReadFile("../../resources/model-pricing/model_prices_and_context_window.json")
	require.NoError(t, err)
	catalog := &PricingService{}
	catalog.pricingData, err = catalog.parsePricingData(data)
	require.NoError(t, err)
	sources := map[string]*BillingService{
		"billing fallback": newTestBillingService(),
		"pricing fallback": NewBillingService(&config.Config{}, &PricingService{pricingData: map[string]*LiteLLMModelPricing{
			"gpt-6": {InputCostPerToken: 10e-6, OutputCostPerToken: 50e-6},
		}}),
		"catalog": NewBillingService(&config.Config{}, catalog),
	}
	cases := []struct {
		model                      string
		input, output, write, read float64
	}{
		{"gpt-6-sol", 2e-6, 10e-6, 2.5e-6, 0.2e-6},
		{"gpt-6-luna", 0.1e-6, 0.5e-6, 0.125e-6, 0.01e-6},
	}
	tiers := map[string]float64{"": 1, "priority": 2, "fast": 2, "flex": 0.5}
	for source, svc := range sources {
		for _, tc := range cases {
			t.Run(source+"/"+tc.model, func(t *testing.T) {
				for _, n := range []int{271999, 272000, 272001} {
					for tier, mult := range tiers {
						tokens := UsageTokens{InputTokens: n - 3000, CacheReadTokens: 2000, CacheCreationTokens: 1000, OutputTokens: 500}
						cost, err := svc.CalculateCostWithServiceTier(tc.model, tokens, 1, tier)
						require.NoError(t, err)
						im, om := 1.0, 1.0
						if n > 272000 {
							im = 2
							om = 1.5
						}
						require.InDelta(t, float64(tokens.InputTokens)*tc.input*im*mult, cost.InputCost, 1e-10)
						require.InDelta(t, 1000*tc.write*im*mult, cost.CacheCreationCost, 1e-10)
						require.InDelta(t, 2000*tc.read*im*mult, cost.CacheReadCost, 1e-10)
						require.InDelta(t, 500*tc.output*om*mult, cost.OutputCost, 1e-10)
						require.InDelta(t, cost.InputCost+cost.OutputCost+cost.CacheCreationCost+cost.CacheReadCost, cost.TotalCost, 1e-10)
						require.Equal(t, n > 272000, cost.LongContextBillingApplied)
					}
				}
			})
		}
	}
}

// 渠道把价格显式设为 0 时要生效，且不能污染后续按模型查到的价；Sol/Luna 的新分支不能改动 gpt-6（Astra）的价。
func TestGPT6SolLunaChannelOverridesAndAstraIsolation(t *testing.T) {
	svc := newTestBillingService()
	for _, model := range []string{"gpt-6-sol", "gpt-6-luna"} {
		t.Run(model, func(t *testing.T) {
			zero := 0.0
			prices, err := svc.GetModelPricingWithChannel(model, &ChannelModelPricing{InputPrice: &zero, OutputPrice: &zero, CacheWritePrice: &zero, CacheReadPrice: &zero})
			require.NoError(t, err)
			cost := svc.computeTokenBreakdown(prices, UsageTokens{InputTokens: 300000, CacheReadTokens: 1000, CacheCreationTokens: 1000, OutputTokens: 1000}, 1, "priority", true)
			require.Zero(t, cost.TotalCost)
			fresh, err := svc.GetModelPricing(model)
			require.NoError(t, err)
			require.Positive(t, fresh.InputPricePerToken)
		})
	}
	prices, err := svc.GetModelPricing("gpt-6")
	require.NoError(t, err)
	require.Equal(t, 10e-6, prices.InputPricePerToken)
	require.Equal(t, "gpt-6-sol", normalizeKnownOpenAICodexModel("openai/gpt-6-sol-max"))
	require.Equal(t, "gpt-6-luna", normalizeKnownOpenAICodexModel("gpt-6-luna-openai-compact"))
}

// 价卡里显式写 0 的缓存写入价要按 0 计，不能被"缓存写入 = 输入 ×1.25"的兜底规则覆盖。
func TestGPT6SolExplicitZeroCacheWrite(t *testing.T) {
	svc := &PricingService{}
	var err error
	svc.pricingData, err = svc.parsePricingData([]byte(`{"gpt-6-sol":{"litellm_provider":"openai","input_cost_per_token":0.000002,"output_cost_per_token":0.00001,"input_cost_per_token_flex":0.000001,"cache_creation_input_token_cost":0}}`))
	require.NoError(t, err)
	billing := NewBillingService(&config.Config{}, svc)
	for _, tier := range []string{"", "priority", "flex"} {
		cost, err := billing.CalculateCostWithServiceTier("gpt-6-sol", UsageTokens{CacheCreationTokens: 1000}, 1, tier)
		require.NoError(t, err)
		require.Zero(t, cost.CacheCreationCost)
	}
}

// 带档位后缀、带 openai/ 前缀、大写写法都要命中本模型的价，Sol 不能落到 Luna 的价。
func TestGPT6SolLunaAliasesUseOwnPricing(t *testing.T) {
	sol := &LiteLLMModelPricing{InputCostPerToken: 2e-6}
	luna := &LiteLLMModelPricing{InputCostPerToken: 0.1e-6}
	svc := &PricingService{pricingData: map[string]*LiteLLMModelPricing{"gpt-6-sol": sol, "gpt-6-luna": luna}}
	for _, model := range []string{"gpt-6-sol-max", "GPT-6-Sol-High", "openai/gpt-6-sol"} {
		require.Same(t, sol, svc.GetModelPricing(model), model)
	}
	for _, model := range []string{"openai/gpt-6-luna-openai-compact", "gpt-6-luna-low"} {
		require.Same(t, luna, svc.GetModelPricing(model), model)
	}
	fallbackOnly := &PricingService{pricingData: map[string]*LiteLLMModelPricing{}}
	require.Same(t, openAIGPT6SolFallbackPricing, fallbackOnly.GetModelPricing("GPT-6-Sol-High"))
	require.Same(t, openAIGPT6LunaFallbackPricing, fallbackOnly.GetModelPricing("gpt-6-luna"))
}
