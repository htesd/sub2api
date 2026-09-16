package service

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Capacity admission wraps the existing health, billing and concurrency scheduler.
// Exhaust ordinary candidates before considering synthetic children.
func (s *OpenAIGatewayService) selectAccountWithScheduler(
	ctx context.Context,
	groupID *int64,
	previousResponseID string,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredTransport OpenAIUpstreamTransport,
	requiredCapability OpenAIEndpointCapability,
	requiredImageCapability OpenAIImagesCapability,
	requireCompact bool,
	platform string,
	previousResponseCanMove bool,
	useUpstreamTokenCost bool,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	state := codexCapacityFromContext(ctx)
	if state != nil {
		state.clear()
	}
	var lastErr error
	var lastDecision OpenAIAccountScheduleDecision
	owner := ""
	if state != nil {
		var err error
		owner, err = s.codexCapacityRegistry().continuationOwner(state.input, time.Now())
		if err != nil {
			return nil, lastDecision, err
		}
		if owner == "" {
			owner = s.codexCapacityRegistry().boundOwner(state.input)
		}
	}
	// 显式的准入梯度，取代原来的 pass 位运算：
	// 1) 优先账号，仅常规容量  2) 优先账号，允许 overflow 挂载
	// 3) 其他 capacity 账号     4) 其他 capacity 账号 + overflow
	// 5) 最后兜底：允许非 capacity 账号（affinity 是偏好，不是排他约束）
	type capacityPass struct{ failover, overflow, plain bool }
	plan := []capacityPass{{}, {overflow: true}}
	if owner != "" && state.input.Previous == "" {
		plan = append(plan, capacityPass{failover: true}, capacityPass{failover: true, overflow: true})
		// 严格续接必须留在真正生成响应的账号上，其余请求宁可退回普通账号，
		// 也不能因为本地容量策略把请求打成 429/503。
		plan = append(plan, capacityPass{failover: true, overflow: true, plain: true})
	}
	for pass := 0; pass < len(plan); pass++ {
		failover, overflow, plain := plan[pass].failover, plan[pass].overflow, plan[pass].plain
		excluded := make(map[int64]struct{}, len(excludedIDs))
		for id := range excludedIDs {
			excluded[id] = struct{}{}
		}
		for {
			if err := ctx.Err(); err != nil {
				return nil, lastDecision, err
			}
			selection, decision, err := s.selectAccountWithoutCodexCapacity(ctx, groupID, previousResponseID, sessionHash, requestedModel, excluded, requiredTransport, requiredCapability, requiredImageCapability, requireCompact, platform, previousResponseCanMove, useUpstreamTokenCost)
			lastDecision = decision
			if err != nil || selection == nil || selection.Account == nil {
				if lastErr == nil {
					lastErr = err
				}
				break
			}
			account := selection.Account
			policy, enabled := account.codexCapacityPolicy()
			if !enabled && (owner == "" || plain) {
				if owner != "" {
					slog.Info("codex_capacity_plain_fallback",
						"account_id", account.ID, "reason", "capacity_candidates_exhausted")
				}
				return selection, decision, nil
			}
			source := account
			if account.IsShadow() {
				source, err = resolveCredentialAccount(ctx, s.accountRepo, account)
			}
			identity := codexAccountIdentityNamespace(source)
			if err == nil && (!enabled || !failover && owner != "" && owner != identity) {
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
				if _, exists := excluded[account.ID]; exists {
					break
				}
				excluded[account.ID] = struct{}{}
				// A skipped non-owner is not itself a continuation conflict or
				// evidence that capacity failed on the eligible owner.
				continue
			}
			var lease *codexCapacityLease
			switch {
			case err != nil:
			case state != nil && !state.websocket && state.input.Previous != "":
				err = capacityError("session_continuation_requires_websocket", 400, 1)
			case state == nil || !state.supported:
				err = capacityError("session_capacity_requires_responses", 400, 1)
			default:
				lease, err = s.codexCapacityRegistry().reserve(identity, policy, state.input, overflow, time.Now())
			}
			if err == nil {
				release := lease.release
				state.stage(account.ID, lease, release)
				if failover && owner != identity {
					slog.Info("codex_capacity_failover", "account_id", account.ID)
				}
				if lease.Assignment.Synthetic {
					occupancy := s.codexCapacityRegistry().occupancy(identity, state.input.Key, policy, time.Now())
					slog.Info("codex_capacity_mounted",
						"account_id", account.ID, "api_key_id", state.input.Key,
						"roots", occupancy.Roots, "root_limit", occupancy.RootLimit,
						"new_children_in_window", occupancy.NewChildren)
				}
				// The request context owns the lease, including wait-plan and streaming paths.
				// The concurrency slot keeps its existing, independent lifetime.
				return selection, decision, nil
			}
			if capacityErr := AsCodexSessionCapacityError(err); capacityErr != nil && state != nil {
				occupancy := s.codexCapacityRegistry().occupancy(identity, state.input.Key, policy, time.Now())
				slog.Info("codex_capacity_rejected",
					"account_id", account.ID, "api_key_id", state.input.Key,
					"code", capacityErr.Code, "status", capacityErr.Status,
					"pass", pass, "overflow", overflow, "plain", plain,
					"roots", occupancy.Roots, "root_limit", occupancy.RootLimit,
					"bindings", occupancy.Bindings,
					"new_children_in_window", occupancy.NewChildren,
					"admission_enabled", policy.admissionEnabled(),
					"ephemeral_identity", state.input.Ephemeral)
			}
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
			if _, exists := excluded[account.ID]; exists {
				return nil, decision, err
			}
			excluded[account.ID] = struct{}{}
			lastErr = err
		}
		if state == nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = ErrNoAvailableAccounts
	}
	if owner != "" && state.input.Previous != "" && errors.Is(lastErr, ErrNoAvailableAccounts) {
		lastErr = capacityError("session_owner_unavailable", 409, 1)
	}
	if capacityErr := AsCodexSessionCapacityError(lastErr); capacityErr != nil {
		slog.Warn("codex_capacity_selection_failed", "code", capacityErr.Code,
			"status", capacityErr.Status, "retry_after", capacityErr.RetryAfter,
			"passes", len(plan), "owner_bound", owner != "")
	}
	return nil, lastDecision, lastErr
}
