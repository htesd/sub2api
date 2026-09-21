package repository

import (
	"context"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func migrationRepo(t *testing.T) (*accountRepository, sqlmock.Sqlmock) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { client.Close() })
	return &accountRepository{client: client}, mock
}

func TestCodexMigrationTransactionCancelsOnConcurrentChange(t *testing.T) {
	for _, scenario := range []string{"proxy_edited", "account_edited"} {
		t.Run(scenario, func(t *testing.T) {
			r, mock := migrationRepo(t)
			now := time.Now().UTC()
			p := &service.Proxy{ID: 2, UpdatedAt: now}
			a := &service.Account{ID: 41, ProxyID: new(int64(1)), Proxy: &service.Proxy{ID: 1, UpdatedAt: now}, Extra: map[string]any{}, Credentials: map[string]any{}}
			mock.ExpectBegin()
			rows := sqlmock.NewRows([]string{"id"}).AddRow(1)
			if scenario == "account_edited" {
				rows.AddRow(2)
			}
			mock.ExpectQuery(`SELECT id FROM proxies.*FOR SHARE`).WithArgs(int64(2), now, int64(1), now).WillReturnRows(rows)
			if scenario == "account_edited" {
				mock.ExpectExec(`(?s)UPDATE accounts SET proxy_id=.*codex_ticket_auto_migrate.*codex_ticket_active_probe`).WillReturnResult(sqlmock.NewResult(0, 0))
			}
			mock.ExpectRollback()
			ok, err := r.CommitCodexMigration(context.Background(), a, p, map[string]any{"route_epoch": "next"}, service.CodexMigrationState{Result: "migrated"})
			require.NoError(t, err)
			require.False(t, ok)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestCodexMigrationLateTicketCAS(t *testing.T) {
	r, mock := migrationRepo(t)
	a := &service.Account{ID: 41, ProxyID: new(int64(1)), Extra: map[string]any{service.CodexMigrationEpochKey: "old"}}
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT COALESCE.*proxy_id IS NOT DISTINCT FROM.*codex_ticket_migration_epoch.*FOR NO KEY UPDATE`).WithArgs("codex_turn_ticket:gpt-6-astra", int64(41), int64(1), "old").WillReturnRows(sqlmock.NewRows([]string{"ticket"}))
	mock.ExpectRollback()
	ok, err := r.StoreCodexTicketForRoute(context.Background(), a, "codex_turn_ticket:gpt-6-astra", map[string]any{"state": "old-test-ticket"})
	require.NoError(t, err)
	require.False(t, ok)
	require.NoError(t, mock.ExpectationsWereMet())
}
