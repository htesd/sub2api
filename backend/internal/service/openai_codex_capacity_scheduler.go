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
	passes := 2
	if owner != "" && state.input.Previous == "" {
		// Exhaust the preferred account (including overflow) before rebuilding
		// a request without server-side continuation on another capacity account.
		passes = 4
	}
	for pass := 0; pass < passes; pass++ {
		failover := pass >= 2
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
			if !enabled && owner == "" {
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
				lease, err = s.codexCapacityRegistry().reserveWithFailover(identity, policy, state.input, pass%2 == 1, failover, time.Now())
			}
			if err == nil {
				release := lease.release
				state.stage(account.ID, lease, release)
				if failover && owner != identity {
					slog.Info("codex_capacity_failover", "account_id", account.ID)
				}
				// The request context owns the lease, including wait-plan and streaming paths.
				// The concurrency slot keeps its existing, independent lifetime.
				return selection, decision, nil
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
	return nil, lastDecision, lastErr
}
