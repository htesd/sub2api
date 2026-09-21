package repository

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/proxy"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *accountRepository) ListCodexMigrationProxies(ctx context.Context) ([]service.Proxy, error) {
	rows, err := r.client.Proxy.Query().Where(proxy.StatusEQ(service.StatusActive)).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]service.Proxy, 0, len(rows))
	for _, p := range rows {
		out = append(out, *proxyEntityToService(p))
	}
	return out, nil
}

// 锁定并比较代理、代际、身份及开关；代理与票同事务落库，不覆盖并发管理操作。
func (r *accountRepository) CommitCodexMigration(ctx context.Context, a *service.Account, p *service.Proxy, ticket any, state service.CodexMigrationState) (bool, error) {
	if a == nil || a.ProxyID == nil || a.Proxy == nil || p == nil {
		return false, service.ErrAccountNilInput
	}
	t, err := json.Marshal(ticket)
	if err != nil {
		return false, err
	}
	st, err := json.Marshal(state)
	if err != nil {
		return false, err
	}
	var obj map[string]any
	if json.Unmarshal(t, &obj) != nil {
		return false, service.ErrAccountNilInput
	}
	epoch, _ := obj["route_epoch"].(string)
	if epoch == "" {
		return false, service.ErrAccountNilInput
	}
	oldEpoch, _ := a.Extra[service.CodexMigrationEpochKey].(string)
	identity, err := json.Marshal(a.Credentials)
	if err != nil {
		return false, err
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	txCtx := dbent.NewTxContext(ctx, tx)
	// share 锁避免候选代理在验证后被改密/改出口；原代理也必须保持验证时版本。
	rows, err := tx.Client().QueryContext(txCtx, `SELECT id FROM proxies WHERE deleted_at IS NULL AND status='active' AND (expires_at IS NULL OR expires_at>NOW()) AND ((id=$1 AND updated_at=$2) OR (id=$3 AND updated_at=$4)) FOR SHARE`, p.ID, p.UpdatedAt, *a.ProxyID, a.Proxy.UpdatedAt)
	if err != nil {
		return false, err
	}
	count := 0
	for rows.Next() {
		count++
	}
	err = rows.Err()
	rows.Close()
	if err != nil || count != 2 {
		return false, err
	}
	res, err := tx.Client().ExecContext(txCtx, `UPDATE accounts SET proxy_id=$1, extra=(SELECT COALESCE(jsonb_object_agg(key,value),'{}'::jsonb) FROM jsonb_each(COALESCE(extra,'{}'::jsonb)) WHERE left(key,18)<>'codex_turn_ticket:') || jsonb_build_object('codex_turn_ticket:gpt-6-astra',$2::jsonb,'codex_ticket_migration_state',$3::jsonb,'codex_ticket_migration_epoch',$4::text), updated_at=NOW()
WHERE id=$5 AND deleted_at IS NULL AND platform='openai' AND type IN ('oauth','setup-token') AND status='active' AND schedulable=true
AND proxy_id=$6 AND COALESCE(extra->>'codex_ticket_migration_epoch','')=$7 AND credentials=$8::jsonb
AND extra->>'codex_ticket_auto_migrate'='true' AND COALESCE(extra->>'codex_ticket_enabled','true') NOT IN ('false','0','off','no') AND extra->>'codex_ticket_active_probe' IN ('true','1','on','yes')
AND (rate_limit_reset_at IS NULL OR rate_limit_reset_at<=NOW()) AND (overload_until IS NULL OR overload_until<=NOW()) AND (temp_unschedulable_until IS NULL OR temp_unschedulable_until<=NOW())
AND (expires_at IS NULL OR expires_at>NOW() OR auto_pause_on_expired=false)`, p.ID, string(t), string(st), epoch, a.ID, *a.ProxyID, oldEpoch, string(identity))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return false, err
	}
	if err = enqueueSchedulerOutbox(txCtx, tx.Client(), service.SchedulerOutboxEventAccountChanged, &a.ID, nil, nil); err != nil {
		return false, err
	}
	// 影子账号沿用母账号出口，必须与母账号一并提交并失效调度缓存。
	shadowRows, err := tx.Client().QueryContext(txCtx, `UPDATE accounts SET proxy_id=$1,updated_at=NOW() WHERE parent_account_id=$2 AND quota_dimension='spark' AND deleted_at IS NULL RETURNING id`, p.ID, a.ID)
	if err != nil {
		return false, err
	}
	var shadowIDs []int64
	for shadowRows.Next() {
		var id int64
		if err = shadowRows.Scan(&id); err != nil {
			shadowRows.Close()
			return false, err
		}
		shadowIDs = append(shadowIDs, id)
	}
	err = shadowRows.Err()
	shadowRows.Close()
	if err != nil {
		return false, err
	}
	for _, id := range shadowIDs {
		if err = enqueueSchedulerOutbox(txCtx, tx.Client(), service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
			return false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, a.ID)
	for _, id := range shadowIDs {
		r.syncSchedulerAccountSnapshotDetached(ctx, id)
	}
	return true, nil
}

// 即使账号关闭自动迁移，已有代际仍约束迟到回包；旧出口无法覆盖新出口库存。
func (r *accountRepository) StoreCodexTicketForRoute(ctx context.Context, a *service.Account, key string, ticket any) (bool, error) {
	if a == nil || !strings.HasPrefix(key, "codex_turn_ticket:") {
		return false, service.ErrAccountNilInput
	}
	payload, err := json.Marshal(ticket)
	if err != nil {
		return false, err
	}
	epoch, _ := a.Extra[service.CodexMigrationEpochKey].(string)
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	txCtx := dbent.NewTxContext(ctx, tx)
	rows, err := tx.Client().QueryContext(txCtx, `SELECT COALESCE(extra->$1::text,'null'::jsonb) FROM accounts WHERE id=$2 AND deleted_at IS NULL AND proxy_id IS NOT DISTINCT FROM $3::bigint AND COALESCE(extra->>'codex_ticket_migration_epoch','')=$4 AND lower(trim(COALESCE(extra->>'codex_ticket_enabled','true'))) NOT IN ('false','0','off','no') FOR NO KEY UPDATE`, key, a.ID, a.ProxyID, epoch)
	if err != nil {
		return false, err
	}
	if !rows.Next() {
		err = rows.Err()
		rows.Close()
		return false, err
	}
	var old []byte
	err = rows.Scan(&old)
	rows.Close()
	if err != nil {
		return false, err
	}
	// 真实时间比较兼容UTC、本地时区、纳秒及历史损坏值，不能按JSON文本排序。
	var previous, incoming struct {
		IssuedAt  time.Time `json:"issued_at"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if json.Unmarshal(payload, &incoming) != nil {
		return false, service.ErrAccountNilInput
	}
	if json.Unmarshal(old, &previous) == nil && previous.ExpiresAt.After(time.Now()) && previous.IssuedAt.After(incoming.IssuedAt) {
		return false, nil
	}
	res, err := tx.Client().ExecContext(txCtx, `UPDATE accounts SET extra=jsonb_set(COALESCE(extra,'{}'::jsonb),ARRAY[$1::text],$2::jsonb,true),updated_at=NOW() WHERE id=$3`, key, string(payload), a.ID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, a.ID)
	return true, nil
}

func (r *accountRepository) RecordCodexMigrationState(ctx context.Context, a *service.Account, state service.CodexMigrationState) error {
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	epoch, _ := a.Extra[service.CodexMigrationEpochKey].(string)
	_, err = r.client.ExecContext(ctx, `UPDATE accounts SET extra=jsonb_set(COALESCE(extra,'{}'::jsonb),'{codex_ticket_migration_state}',$1::jsonb,true) WHERE id=$2 AND deleted_at IS NULL AND proxy_id IS NOT DISTINCT FROM $3::bigint AND COALESCE(extra->>'codex_ticket_migration_epoch','')=$4 AND extra->>'codex_ticket_auto_migrate'='true'`, string(b), a.ID, a.ProxyID, epoch)
	if err == nil {
		r.syncSchedulerAccountSnapshotDetached(ctx, a.ID)
	}
	return err
}
