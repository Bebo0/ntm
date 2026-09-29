package bv

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/Dicklesworthstone/ntm/internal/worksource"
)

var ErrNoClaimableWork = errors.New(worksource.NoClaimableCode)

// WorkEligibilityError retains canonical exclusion reasons instead of reporting
// a healthy, empty queue when a tool supplied only ineligible candidates.
type WorkEligibilityError struct {
	Source     worksource.Identity    `json:"source"`
	Exclusions []worksource.Exclusion `json:"exclusions"`
}

func (e *WorkEligibilityError) Error() string {
	return worksource.NoClaimableCode + ": collected work candidates failed canonical source eligibility checks"
}
func (e *WorkEligibilityError) Unwrap() error { return ErrNoClaimableWork }

// GetActionableRecommendationsContext adds canonical eligibility to the
// existing source-bound plan/lifecycle reconciliation. Keep DB-only workspaces
// operational without claiming that their candidates were JSONL-verified.
//
// When every collected candidate fails canonical eligibility, the actionable
// queue is empty: assign, watch, spawn and the coordinator report a normal
// empty/drained queue, exactly as when bv returns no candidates. The typed
// WorkEligibilityError stays available from actionableWithWorkSource for
// evidence consumers; a failed or mismatched source read is still an error.
func GetActionableRecommendationsContext(ctx context.Context, dir string, n int) ([]TriageRecommendation, error) {
	recommendations, err := actionableWithWorkSource(ctx, dir, n, getActionableRecommendationsFromToolsContext)
	var ineligible *WorkEligibilityError
	if errors.As(err, &ineligible) {
		slog.Debug("no claimable work after canonical eligibility checks", "dir", dir, "excluded", len(ineligible.Exclusions))
		return []TriageRecommendation{}, nil
	}
	return recommendations, err
}

func actionableWithWorkSource(ctx context.Context, dir string, n int, collect func(context.Context, string, int) ([]TriageRecommendation, error)) ([]TriageRecommendation, error) {
	if ctx == nil {
		return nil, errors.New("actionable recommendations context is required")
	}
	dir, err := normalizeTriageDir(dir)
	if err != nil {
		return nil, err
	}
	identity, err := captureTriageSource(ctx, dir)
	if err != nil {
		return nil, err
	}
	if !identity.Bound() {
		candidates, err := collect(ctx, dir, n)
		if err != nil {
			return nil, err
		}
		current, err := captureTriageSource(ctx, dir)
		if err != nil {
			return nil, err
		}
		if err := identity.Verify(current); err != nil {
			return nil, err
		}
		return candidates, nil
	}
	source, err := worksource.Read(ctx, dir, worksource.Policy{Expected: &identity})
	if err != nil {
		return nil, err
	}
	// Apply the caller's limit AFTER exclusion, or blocked top-ranked rows can
	// hide eligible work below the cutoff and make the assigner report dry.
	candidates, err := collect(ctx, dir, 0)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, candidate.ID)
	}
	// Operator gates are deliberately NOT applied here. Every caller classifies
	// gated candidates itself and reports them (skipped "operator-gated label",
	// gated queues treated as drained), and claims re-check the gate atomically.
	// Dropping them here would hide that evidence and turn a gated-only queue
	// into a no-claimable failure.
	eligibility := source.Filter(ids, worksource.EligibilityPolicy{})
	if err := worksource.Validate(ctx, source.Identity); err != nil {
		return nil, err
	}
	if len(candidates) > 0 && len(eligibility.EligibleIDs) == 0 {
		return nil, &WorkEligibilityError{Source: source.Identity, Exclusions: eligibility.Excluded}
	}
	allowed := make(map[string]bool, len(eligibility.EligibleIDs))
	for _, id := range eligibility.EligibleIDs {
		allowed[id] = true
	}
	// Gated rows pass through for the callers to report, but they must not
	// consume the caller's limit: a capped planner (spawn --assign asks for
	// 100) would otherwise see only gated rows on a heavily gated backlog and
	// report nothing to do while eligible work sits below the cutoff.
	gatedLabels := operatorGatedLabelsForProject(dir)
	result := make([]TriageRecommendation, 0, len(eligibility.EligibleIDs))
	claimable := 0
	for _, candidate := range candidates {
		id := strings.TrimSpace(candidate.ID)
		if !allowed[id] {
			continue
		}
		delete(allowed, id)
		candidate.ID = id
		result = append(result, candidate)
		if recommendationCarriesOperatorGate(gatedLabels, candidate) {
			continue
		}
		claimable++
		if n > 0 && claimable >= n {
			break
		}
	}
	return result, nil
}

func recommendationCarriesOperatorGate(gatedLabels map[string]struct{}, recommendation TriageRecommendation) bool {
	for _, label := range recommendation.Labels {
		if _, gated := gatedLabels[strings.ToLower(strings.TrimSpace(label))]; gated {
			return true
		}
	}
	return false
}
