//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestCodexMigrationAtomicProxyTicketAndLateWrite(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	r := newAccountRepositoryWithSQL(client, integrationDB, nil)
	makeProxy := func(name string) *service.Proxy {
		p, e := client.Proxy.Create().SetName(name).SetProtocol("http").SetHost(name + ".example").SetPort(8080).SetStatus("active").Save(ctx)
		require.NoError(t, e)
		return proxyEntityToService(p)
	}
	from, to := makeProxy("migration-from"), makeProxy("migration-to")
	a := mustCreateAccount(t, client, &service.Account{Name: "migration-real-db", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, ProxyID: &from.ID, Credentials: map[string]any{"access_token": "test", "plan_type": "pro"}, Extra: map[string]any{service.CodexAutoMigrateKey: true, "codex_ticket_active_probe": true, "codex_turn_ticket:gpt-5.6-sol": map[string]any{"state": "old-sol"}}})
	var err error
	a, err = r.GetByID(ctx, a.ID)
	require.NoError(t, err)
	old := *a
	ticket := map[string]any{"state": "test-new-ticket", "route_epoch": "new-epoch", "issued_at": time.Now().UTC().Format(time.RFC3339), "source_proxy_id": to.ID}
	ok, err := r.CommitCodexMigration(ctx, a, to, ticket, service.CodexMigrationState{Result: "migrated", CheckedAt: time.Now(), FromProxyID: from.ID, ToProxyID: to.ID})
	require.NoError(t, err)
	require.True(t, ok)
	updated, err := r.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, to.ID, *updated.ProxyID)
	require.Equal(t, "new-epoch", updated.Extra[service.CodexMigrationEpochKey])
	require.NotContains(t, updated.Extra, "codex_turn_ticket:gpt-5.6-sol")
	ok, err = r.StoreCodexTicketForRoute(ctx, &old, "codex_turn_ticket:gpt-6-astra", map[string]any{"state": "late", "issued_at": time.Now().Add(time.Second).UTC().Format(time.RFC3339)})
	require.NoError(t, err)
	require.False(t, ok)
	ok, err = r.StoreCodexTicketForRoute(ctx, updated, "codex_turn_ticket:gpt-6-astra", ticket)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, r.RecordCodexMigrationState(ctx, updated, service.CodexMigrationState{Result: "current_proxy_ok", CheckedAt: time.Now()}))
	base := time.Now().UTC().Truncate(time.Second)
	oldTime := base.In(time.FixedZone("UTC+8", 8*3600))
	ticket["issued_at"] = oldTime.Format(time.RFC3339Nano)
	ticket["expires_at"] = base.Add(time.Hour).Format(time.RFC3339)
	require.NoError(t, r.UpdateExtra(ctx, a.ID, map[string]any{"codex_turn_ticket:gpt-6-astra": ticket}))
	ticket["issued_at"] = base.Add(500 * time.Millisecond).Format(time.RFC3339Nano)
	ok, err = r.StoreCodexTicketForRoute(ctx, updated, "codex_turn_ticket:gpt-6-astra", ticket)
	require.NoError(t, err)
	require.True(t, ok, "混合时区及纳秒的新票应成功")
	ticket["issued_at"] = base.Add(-time.Second).Format(time.RFC3339Nano)
	ok, err = r.StoreCodexTicketForRoute(ctx, updated, "codex_turn_ticket:gpt-6-astra", ticket)
	require.NoError(t, err)
	require.False(t, ok, "真实更旧票不得覆盖")
}
