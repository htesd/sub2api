package service

import (
	"context"
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
	for pass := 0; pass < 2; pass++ {
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
			var lease *codexCapacityLease
			switch {
			case err != nil:
			case !enabled || owner != "" && owner != identity:
				err = capacityError("session_owner_unavailable", 409, 1)
			case state != nil && !state.websocket && state.input.Previous != "":
				err = capacityError("session_continuation_requires_websocket", 400, 1)
			case state == nil || !state.supported:
				err = capacityError("session_capacity_requires_responses", 400, 1)
			default:
				lease, err = s.codexCapacityRegistry().reserve(identity, policy, state.input, pass == 1, time.Now())
			}
			if err == nil {
				release := lease.release
				state.stage(account.ID, lease, release)
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
	return nil, lastDecision, lastErr
}
