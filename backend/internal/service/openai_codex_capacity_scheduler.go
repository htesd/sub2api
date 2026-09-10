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
	var deadline time.Time
	var queueDecision OpenAIAccountScheduleDecision
	var queueRetryAfter int
	queued := false
	defer func() {
		if queued {
			rt := s.codexControls()
			rt.mu.Lock()
			rt.waiters--
			rt.mu.Unlock()
		}
	}()
	for {
		if queued && !time.Now().Before(deadline) {
			return nil, queueDecision, capacityError("session_queue_timeout", 429, queueRetryAfter)
		}
		selection, decision, err := s.selectAccountWithCapacityAdmissionScan(ctx, groupID, previousResponseID, sessionHash, requestedModel, excludedIDs, requiredTransport, requiredCapability, requiredImageCapability, requireCompact, platform, previousResponseCanMove, useUpstreamTokenCost)
		capacityErr := AsCodexSessionCapacityError(err)
		if capacityErr == nil || capacityErr.QueueSeconds <= 0 {
			return selection, decision, err
		}
		if !queued {
			rt := s.codexControls()
			rt.mu.Lock()
			if rt.waiters >= 128 {
				rt.mu.Unlock()
				return nil, decision, capacityError("session_queue_full", 429, 1)
			}
			rt.waiters++
			rt.mu.Unlock()
			queued = true
			deadline = time.Now().Add(time.Duration(capacityErr.QueueSeconds) * time.Second)
			queueDecision, queueRetryAfter = decision, capacityErr.RetryAfter
			if r := codexControl(ctx); r != nil {
				r.mu.Lock()
				budgetDeadline := r.started.Add(time.Duration(r.policy.RetryWindowSeconds) * time.Second)
				if r.configured && budgetDeadline.Before(deadline) {
					deadline = budgetDeadline
				}
				r.mu.Unlock()
			}
		}
		if time.Now().After(deadline) {
			return nil, decision, capacityError("session_queue_timeout", 429, capacityErr.RetryAfter)
		}
		delay := min(time.Until(deadline), time.Second)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, decision, ctx.Err()
		case <-timer.C:
		}
		if r := codexControl(ctx); r != nil {
			r.mu.Lock()
			r.wait += delay
			r.totalWait += delay
			r.mu.Unlock()
		}
	}
}

func (s *OpenAIGatewayService) selectAccountWithCapacityAdmissionScan(
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
	var queueErr *CodexSessionCapacityError
	var queueAccount *Account
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
				lease, err = s.codexCapacityRegistry().reserve(identity, policy, state.input, pass%2 == 1, time.Now())
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
			if e := AsCodexSessionCapacityError(err); e != nil && e.Code == "session_roots_full" && state != nil && state.input.Previous == "" {
				p := account.codexRequestPolicy()
				if p.Enabled && p.CapacityMode == "queue" && p.CapacityWaitSeconds > 0 {
					queueAccount = account
					copy := *e
					copy.QueueSeconds = p.CapacityWaitSeconds
					queueErr = &copy
				}
			}
		}
		if state == nil {
			break
		}
	}
	if queueErr != nil {
		configureCodexControl(ctx, queueAccount)
		return nil, lastDecision, queueErr
	}
	if lastErr == nil {
		lastErr = ErrNoAvailableAccounts
	}
	if owner != "" && state.input.Previous != "" && errors.Is(lastErr, ErrNoAvailableAccounts) {
		lastErr = capacityError("session_owner_unavailable", 409, 1)
	}
	return nil, lastDecision, lastErr
}
