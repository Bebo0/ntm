package bv

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"

	"github.com/Dicklesworthstone/ntm/internal/worksource"
)

var ErrNoClaimableWork = errors.New(worksource.NoClaimableCode)

var (
	workSourcePolicyMu sync.RWMutex
	workSourcePolicy   worksource.ProjectPolicy
	// projectWorkSourcePolicies holds each authoritative project's strictly
	// loaded [assign.work_source] policy, registered beside its operator
	// gates, so projects sharing a process cannot overwrite one another's.
	projectWorkSourcePolicies sync.Map // map[string]worksource.ProjectPolicy
)

// ConfigureWorkSourcePolicy installs the process-wide [assign.work_source]
// policy, used for any project that has not registered its own.
func ConfigureWorkSourcePolicy(policy worksource.ProjectPolicy) {
	policy.ProgramLabels = append([]string(nil), policy.ProgramLabels...)
	workSourcePolicyMu.Lock()
	workSourcePolicy = policy
	workSourcePolicyMu.Unlock()
}

// ConfigureProjectWorkSourcePolicy installs one authoritative project's
// [assign.work_source] policy.
func ConfigureProjectWorkSourcePolicy(projectDir string, policy worksource.ProjectPolicy) error {
	key, err := operatorGateProjectKey(projectDir)
	if err != nil {
		return err
	}
	policy.ProgramLabels = append([]string(nil), policy.ProgramLabels...)
	projectWorkSourcePolicies.Store(key, policy)
	return nil
}

// WorkSourcePolicyForProject returns the [assign.work_source] policy that
// dispatch and work snapshots apply to projectDir.
func WorkSourcePolicyForProject(projectDir string) worksource.ProjectPolicy {
	if key, err := operatorGateProjectKey(projectDir); err == nil {
		if stored, ok := projectWorkSourcePolicies.Load(key); ok {
			if policy, valid := stored.(worksource.ProjectPolicy); valid {
				policy.ProgramLabels = append([]string(nil), policy.ProgramLabels...)
				return policy
			}
		}
	}
	workSourcePolicyMu.RLock()
	defer workSourcePolicyMu.RUnlock()
	policy := workSourcePolicy
	policy.ProgramLabels = append([]string(nil), policy.ProgramLabels...)
	return policy
}

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
	policy := WorkSourcePolicyForProject(dir)
	if !identity.Bound() && policy.Strict() {
		return nil, &worksource.StaleError{Observed: &identity, Reason: "[assign.work_source] policy requires a canonical Beads JSONL export"}
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
	source, err := worksource.Read(ctx, dir, worksource.Policy{Expected: &identity, RequiredRef: policy.RequiredRef, RequireClean: policy.RequireClean})
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
	// into a no-claimable failure. Program scope is the project's policy, not
	// an operator decision, so it does exclude here.
	eligibility := source.Filter(ids, worksource.EligibilityPolicy{ProgramLabels: policy.ProgramLabels})
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
