package service

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCodexCapacityFailoverStripsForeignTurnStateAcrossRequests(t *testing.T) {
	body := []byte(`{"prompt_cache_key":"root","client_metadata":{"x-codex-turn-state":"opaque-old"}}`)
	c, a, svc := capacityContext(t, body)
	registry := svc.codexCapacityRegistry()
	in := codexCapacityFromGin(c).input
	old, err := registry.reserve(codexAccountIdentityNamespace(a), defaultCodexCapacityPolicy(), in, false, time.Now())
	require.NoError(t, err)
	old.sent(time.Now())
	old.release()
	second := *a
	second.ID = 2
	second.Credentials = map[string]any{"chatgpt_account_id": "new"}
	for i := 0; i < 2; i++ {
		if i > 0 {
			c, _, _ = capacityContext(t, body)
		}
		state := codexCapacityFromGin(c)
		lease, err := registry.reserve(codexAccountIdentityNamespace(&second), defaultCodexCapacityPolicy(), state.input, false, time.Now())
		require.NoError(t, err)
		state.stage(second.ID, lease, lease.release)
		headers := http.Header{}
		headers.Set(openAICodexTurnStateHeader, "opaque-old")
		projected, err := projectCodexCapacity(c, &second, body, headers)
		require.NoError(t, err)
		require.Empty(t, headers.Get(openAICodexTurnStateHeader))
		require.False(t, gjson.GetBytes(projected, "client_metadata.x-codex-turn-state").Exists())
		lease.sent(time.Now())
		ReleaseCodexSessionCapacity(c)
	}
	require.False(t, registry.foreignTurnState(1, "opaque-new", "new", "new"))
	require.False(t, registry.foreignTurnState(2, "opaque-old", "new", "new"), "keys have separate token ownership")
}

func TestCodexCapacityTurnStateTracksEmitterBeforeFirstEcho(t *testing.T) {
	c, a, svc := capacityContext(t, []byte(`{"prompt_cache_key":"root"}`))
	state := codexCapacityFromGin(c)
	registry := svc.codexCapacityRegistry()
	identity := codexAccountIdentityNamespace(a)
	lease, err := registry.reserve(identity, defaultCodexCapacityPolicy(), state.input, false, time.Now())
	require.NoError(t, err)
	state.stage(a.ID, lease, lease.release)
	headers := http.Header{}
	headers.Set(openAICodexTurnStateHeader, "emitted-by-A")
	svc.relayOpenAICodexTurnState(c, a, headers)
	require.True(t, registry.foreignTurnState(1, "emitted-by-A", "B", "B"))
	require.False(t, registry.foreignTurnState(1, "emitted-by-A", identity, "B"))
	// Authoritative emission corrects an earlier heuristic cache entry.
	registry.foreignTurnState(1, "corrected", "B", "B")
	headers.Set(openAICodexTurnStateHeader, "corrected")
	svc.noteStagedOpenAICodexTurnStateCommitted(c, a, headers)
	require.True(t, registry.foreignTurnState(1, "corrected", "B", "B"))
	// A later legacy session origin must not strip a correctly owned token.
	svc.noteOpenAICodexTurnStateProvenance(c, &Account{ID: 2})
	headers.Set(openAICodexTurnStateHeader, "emitted-by-A")
	svc.guardOpenAICodexTurnStateEcho(c, a, headers)
	require.Equal(t, "emitted-by-A", headers.Get(openAICodexTurnStateHeader))
}
