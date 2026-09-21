package admin

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSettingsCodexTicketPolicyWriteReadAndClear(t *testing.T) {
	loop := service.SettingKeyOpenAICodexTicketHarvestLoopEnabled
	models := service.SettingKeyOpenAICodexTicketRequiredModels
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{loop: "false", models: "gpt-6-astra"})
	cfg := config.OpenAICodexTicketConfig{FailClosed: true}
	require.False(t, h.settingService.ResolveOpenAICodexTicketConfig(context.Background(), cfg).HarvestLoopEnabled)
	rec := doUpdateSettings(t, h, map[string]any{loop: true, models: "gpt-6-astra\ngpt-6-astra"}, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "gpt-6-astra", repo.values[models])
	effective := h.settingService.ResolveOpenAICodexTicketConfig(context.Background(), cfg)
	require.True(t, effective.HarvestLoopEnabled)
	require.Equal(t, []string{"gpt-6-astra"}, effective.RequiredModels)
	rec = doUpdateSettings(t, h, map[string]any{"site_name": "preserve-policy"}, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "true", repo.values[loop])
	require.Equal(t, "gpt-6-astra", repo.values[models])
	rec = doUpdateSettings(t, h, map[string]any{loop: false, models: ""}, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	effective = h.settingService.ResolveOpenAICodexTicketConfig(context.Background(), cfg)
	require.False(t, effective.HarvestLoopEnabled)
	require.NotNil(t, effective.RequiredModels)
	require.Empty(t, effective.RequiredModels)
	require.Contains(t, rec.Body.String(), `"openai_codex_ticket_required_models":""`)
}

func TestSettingsCodexTicketOmittedPolicyPreservesUnset(t *testing.T) {
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{})
	rec := doUpdateSettings(t, h, map[string]any{"site_name": "unrelated"}, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, repo.values, service.SettingKeyOpenAICodexTicketRequiredModels)
	require.NotContains(t, repo.values, service.SettingKeyOpenAICodexTicketHarvestLoopEnabled)
}

func TestSettingsCodexTicketProxyWriteReadAndHotReload(t *testing.T) {
	key := service.SettingKeyOpenAICodexTicketHarvestProxyURL
	oldProxy := "http://user:old-secret@old.example.com:8080"
	newProxy := "socks5h://user:new-secret@new.example.com:1080"
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{key: oldProxy})
	require.Equal(t, oldProxy, h.settingService.GetOpenAICodexTicketHarvestProxyURL(context.Background()))
	rec := doUpdateSettings(t, h, map[string]any{key: newProxy}, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, newProxy, repo.values[key])
	require.Equal(t, newProxy, h.settingService.GetOpenAICodexTicketHarvestProxyURL(context.Background()))
	require.NotContains(t, rec.Body.String(), "new-secret")
	require.Contains(t, rec.Body.String(), `"openai_codex_ticket_harvest_proxy_configured":true`)
	// Omission, empty input and the masked GET value all preserve the real secret.
	for _, body := range []map[string]any{{"site_name": "updated"}, {key: ""}, {key: service.MaskProxyURL(newProxy)}} {
		rec = doUpdateSettings(t, h, body, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, newProxy, repo.values[key])
	}
	get := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(get)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)
	h.GetSettings(c)
	require.Equal(t, http.StatusOK, get.Code)
	require.NotContains(t, get.Body.String(), "new-secret")
	require.Contains(t, get.Body.String(), "new.example.com")
}

func TestSettingsCodexTicketRejectInvalidProxyWithoutLeakingPassword(t *testing.T) {
	key := service.SettingKeyOpenAICodexTicketHarvestProxyURL
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{key: "http://previous.example.com:8080"})
	rec := doUpdateSettings(t, h, map[string]any{key: "ftp://user:invalid-secret@proxy.example.com:21"}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "invalid-secret")
	require.Equal(t, "http://previous.example.com:8080", repo.values[key])
}
