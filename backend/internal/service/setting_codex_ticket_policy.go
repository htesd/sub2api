package service

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// NormalizeCodexTicketRequiredModels 支持逗号与换行，精确匹配出站模型，不支持通配符。
func NormalizeCodexTicketRequiredModels(raw string) []string {
	result := []string{}
	seen := map[string]bool{}
	for _, model := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		model = strings.TrimSpace(model)
		if model != "" && !seen[model] {
			result = append(result, model)
			seen[model] = true
		}
	}
	return result
}

type cachedCodexTicketPolicy struct {
	values    map[string]string
	expiresAt time.Time
}

// ResolveOpenAICodexTicketConfig 是调度、采集和管理端共用的有效配置入口。
// 后台名单显式为空时不能回退到启动配置，否则无法热更新清空名单。
func (s *SettingService) ResolveOpenAICodexTicketConfig(ctx context.Context, cfg config.OpenAICodexTicketConfig) config.OpenAICodexTicketConfig {
	cfg = NormalizeOpenAICodexTicketConfig(cfg)
	if s == nil {
		return cfg
	}
	cfg.Enabled = s.GetOpenAICodexTicketEnabled(ctx, cfg.Enabled)
	if proxy := s.GetOpenAICodexTicketHarvestProxyURL(ctx); proxy != "" {
		cfg.HarvestProxyURL = proxy
	}
	if s.settingRepo == nil || ctx.Err() != nil {
		return cfg
	}
	cached, _ := s.openAICodexTicketPolicyCache.Load().(*cachedCodexTicketPolicy)
	if cached == nil || time.Now().After(cached.expiresAt) {
		result := s.openAICodexTicketPolicySF.DoChan("policy", func() (any, error) {
			s.openAICodexTicketPolicyMu.Lock()
			generation := s.openAICodexTicketPolicyGeneration
			s.openAICodexTicketPolicyMu.Unlock()
			if c, _ := s.openAICodexTicketPolicyCache.Load().(*cachedCodexTicketPolicy); c != nil && time.Now().Before(c.expiresAt) {
				return c, nil
			}
			dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			values, err := s.settingRepo.GetMultiple(dbCtx, []string{SettingKeyOpenAICodexTicketHarvestLoopEnabled, SettingKeyOpenAICodexTicketRequiredModels})
			if err != nil {
				return nil, err
			}
			c := &cachedCodexTicketPolicy{values: values, expiresAt: time.Now().Add(5 * time.Second)}
			s.openAICodexTicketPolicyMu.Lock()
			defer s.openAICodexTicketPolicyMu.Unlock()
			if generation != s.openAICodexTicketPolicyGeneration {
				return s.openAICodexTicketPolicyCache.Load().(*cachedCodexTicketPolicy), nil
			}
			s.openAICodexTicketPolicyCache.Store(c)
			return c, nil
		})
		select {
		case <-ctx.Done():
			cfg.HarvestLoopEnabled = false
		case loaded := <-result:
			if loaded.Err == nil {
				cached, _ = loaded.Val.(*cachedCodexTicketPolicy)
			} else if cached == nil {
				// 冷启动无法确认后台策略时，不额外探测，不放行受控模型。
				cfg.HarvestLoopEnabled = false
				cfg.FailClosed = true
				cfg.RequiredModels = append([]string{}, cfg.Models...)
			}
		}
	}
	if cached != nil {
		if raw, ok := cached.values[SettingKeyOpenAICodexTicketHarvestLoopEnabled]; ok {
			cfg.HarvestLoopEnabled = raw == "true"
		}
		if raw, ok := cached.values[SettingKeyOpenAICodexTicketRequiredModels]; ok {
			cfg.RequiredModels = NormalizeCodexTicketRequiredModels(raw)
			cfg.FailClosed = true // 后台名单是显式策略，优先于旧版 fail_closed。
		}
	}
	// 名单模型同时进入采集范围，避免填入新模型后只有门禁、没有票源。
	cfg.Models = NormalizeCodexTicketRequiredModels(strings.Join(append(append([]string{}, cfg.Models...), cfg.RequiredModels...), ","))
	return cfg
}

// 保存成功后直接发布新策略，旧查询不得覆盖新值；读取失败时保留上次成功值。
func (s *SettingService) publishCodexTicketPolicy(settings *SystemSettings) {
	s.openAICodexTicketPolicyMu.Lock()
	defer s.openAICodexTicketPolicyMu.Unlock()
	s.openAICodexTicketPolicyGeneration++
	values := map[string]string{
		SettingKeyOpenAICodexTicketHarvestLoopEnabled: strconv.FormatBool(settings.OpenAICodexTicketHarvestLoopEnabled),
		SettingKeyOpenAICodexTicketRequiredModels:     settings.OpenAICodexTicketRequiredModels,
	}
	if settings.OpenAICodexTicketPolicyLoaded {
		if !settings.OpenAICodexTicketPolicyModelsPresent {
			delete(values, SettingKeyOpenAICodexTicketRequiredModels)
		}
		if !settings.OpenAICodexTicketPolicyLoopPresent {
			delete(values, SettingKeyOpenAICodexTicketHarvestLoopEnabled)
		}
	}
	s.openAICodexTicketPolicyCache.Store(&cachedCodexTicketPolicy{values: values, expiresAt: time.Now().Add(5 * time.Second)})
	s.openAICodexTicketPolicySF.Forget("policy")
}

// 仅发布已经提交的字段，禁止用无关设置读取的旧快照覆盖策略。
// 外层 settingsWriteMu 将数据库写入与发布顺序串行化。
func (s *SettingService) publishCommittedCodexTicketPolicy(updates map[string]string) {
	keys := []string{SettingKeyOpenAICodexTicketHarvestLoopEnabled, SettingKeyOpenAICodexTicketRequiredModels}
	changed := false
	for _, key := range keys {
		if _, ok := updates[key]; ok {
			changed = true
		}
	}
	if !changed {
		return
	}
	s.openAICodexTicketPolicyMu.Lock()
	defer s.openAICodexTicketPolicyMu.Unlock()
	values := map[string]string{}
	for _, key := range keys {
		if value, ok := updates[key]; ok {
			values[key] = value
		}
	}
	s.openAICodexTicketPolicyGeneration++
	s.openAICodexTicketPolicyCache.Store(&cachedCodexTicketPolicy{values: values, expiresAt: time.Now().Add(5 * time.Second)})
	s.openAICodexTicketPolicySF.Forget("policy")
}

func (s *SettingService) prepareCodexTicketPolicy(ctx context.Context, updates map[string]string) (map[string]string, error) {
	keys := []string{SettingKeyOpenAICodexTicketHarvestLoopEnabled, SettingKeyOpenAICodexTicketRequiredModels}
	touched := false
	for _, key := range keys {
		if _, ok := updates[key]; ok {
			touched = true
		}
	}
	if !touched {
		return nil, nil
	}
	values, err := s.settingRepo.GetMultiple(ctx, keys)
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	for k, v := range values {
		result[k] = v
	}
	for _, key := range keys {
		if value, ok := updates[key]; ok {
			result[key] = value
		}
	}
	return result, nil
}

// 无票拒发与主动探测许可解耦：名单内模型必须有票，名单外允许被动采集。
func openAICodexTicketModelRequired(cfg config.OpenAICodexTicketConfig, model string) bool {
	if !cfg.FailClosed {
		return false
	}
	models := cfg.RequiredModels
	// 未配置名单的旧部署保持原策略；显式空名单则全部放行。
	if models == nil {
		models = cfg.Models
	}
	for _, item := range models {
		if item == strings.TrimSpace(model) {
			return true
		}
	}
	return false
}

func openAICodexTicketPolicyBlocks(account *Account, cfg config.OpenAICodexTicketConfig, model string, now time.Time) bool {
	if !openAICodexTicketAccountEnabled(account) || !openAICodexTicketModelRequired(cfg, model) {
		return false
	}
	if cfg.RequiredModels == nil {
		return openAICodexTicketFailClosedEffective(account, cfg, len(SplitOpenAICodexTicketHarvestProxyPool(cfg.HarvestProxyURL)) > 0, now)
	}
	return true
}
