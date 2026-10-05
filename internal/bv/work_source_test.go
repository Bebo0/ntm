package bv

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/ntm/internal/worksource"
)

func sourceGuardFixture(t *testing.T) (string, string) {
	t.Helper()
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, ".beads", "issues.jsonl")
	data := `{"id":"blocked","status":"open","issue_type":"task","dependencies":[{"depends_on_id":"busy","type":"blocks"}]}` + "\n" + `{"id":"busy","status":"in_progress","issue_type":"task"}` + "\n" + `{"id":"ready","status":"open","issue_type":"task"}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return project, path
}

func TestActionableSourceFiltersBeforeLimitAndDoesNotDispatchStaleWork(t *testing.T) {
	project, path := sourceGuardFixture(t)
	calls := 0
	collect := func(ctx context.Context, dir string, n int) ([]TriageRecommendation, error) {
		calls++
		if n != 0 {
			t.Fatalf("source filter received a prematurely truncated plan: %d", n)
		}
		return []TriageRecommendation{{ID: "blocked"}, {ID: "ready"}, {ID: "ready"}}, nil
	}
	got, err := actionableWithWorkSource(context.Background(), project, 1, collect)
	if err != nil || len(got) != 1 || got[0].ID != "ready" || calls != 1 {
		t.Fatalf("filtered plan: %+v %v calls=%d", got, err, calls)
	}
	dispatched := 0
	mutatingCollect := func(context.Context, string, int) ([]TriageRecommendation, error) {
		if err := os.WriteFile(path, []byte(`{"id":"ready","status":"closed"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return []TriageRecommendation{{ID: "ready"}}, nil
	}
	got, err = actionableWithWorkSource(context.Background(), project, 1, mutatingCollect)
	if err == nil {
		dispatched += len(got)
	}
	if !errors.Is(err, worksource.ErrStale) || len(got) != 0 || dispatched != 0 {
		t.Fatalf("mixed-source plan reached dispatch: %+v %v", got, err)
	}
}

func TestActionableSourceFailsBeforeToolReadsWhenExistingSourceIsInvalid(t *testing.T) {
	project, path := sourceGuardFixture(t)
	if err := os.WriteFile(path, []byte("invalid JSONL"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	collect := func(context.Context, string, int) ([]TriageRecommendation, error) { calls++; return nil, nil }
	if _, err := actionableWithWorkSource(context.Background(), project, 1, collect); !errors.Is(err, worksource.ErrStale) {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("unverified source triggered %d tool reads", calls)
	}
}

func TestActionableSourcePreservesDBOnlyOperation(t *testing.T) {
	calls := 0
	got, err := actionableWithWorkSource(context.Background(), t.TempDir(), 3, func(ctx context.Context, dir string, n int) ([]TriageRecommendation, error) {
		calls++
		if n != 3 {
			t.Fatalf("DB-only limit = %d", n)
		}
		return []TriageRecommendation{{ID: "database-only"}}, nil
	})
	if err != nil || calls != 1 || len(got) != 1 || got[0].ID != "database-only" {
		t.Fatalf("DB-only planning changed: %+v %v", got, err)
	}
}

func TestActionableSourceRetainsExclusionsAndNormalEmptyQueue(t *testing.T) {
	project, _ := sourceGuardFixture(t)
	got, err := actionableWithWorkSource(context.Background(), project, 0, func(context.Context, string, int) ([]TriageRecommendation, error) {
		return []TriageRecommendation{{ID: "blocked"}}, nil
	})
	var receipt *WorkEligibilityError
	if !errors.Is(err, ErrNoClaimableWork) || !errors.As(err, &receipt) || len(got) != 0 || len(receipt.Exclusions) != 1 || receipt.Exclusions[0].ID != "blocked" {
		t.Fatalf("missing exclusions: %+v %v", got, err)
	}
	got, err = actionableWithWorkSource(context.Background(), project, 0, func(context.Context, string, int) ([]TriageRecommendation, error) {
		return []TriageRecommendation{}, nil
	})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("normal queue exhaustion changed: %+v %v", got, err)
	}
}

func TestActionableSourcePublicPlannerExcludesBlockedJoin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake tools use a POSIX shell")
	}
	project, path := sourceGuardFixture(t)
	bin := t.TempDir()
	bvScript := `#!/bin/sh
case "$*" in
  *--robot-plan*) printf '%s\n' '{"plan":{"tracks":[{"track_id":"one","items":[{"id":"blocked","title":"Blocked join","status":"open","priority":1},{"id":"ready","title":"Ready task","status":"open","priority":2}]}],"summary":{"total_actionable":2}}}' ;;
  *--robot-triage*) printf '%s\n' '{"triage":{"recommendations":[{"id":"blocked","title":"Blocked join","status":"open","priority":1},{"id":"ready","title":"Ready task","status":"open","priority":2}]}}' ;;
  *) exit 1 ;;
esac
`
	brScript := `#!/bin/sh
case "$*" in
  *ready*|*list*) printf '%s\n' '[{"id":"blocked","status":"open","issue_type":"task","labels":[]},{"id":"ready","status":"open","issue_type":"task","labels":[]}]' ;;
  *) printf '%s\n' '{}' ;;
esac
`
	for name, script := range map[string]string{"bv": bvScript, "br": brScript} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	InvalidateTriageCache()
	t.Cleanup(InvalidateTriageCache)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := GetActionableRecommendationsContext(context.Background(), project, 1)
	if err != nil || len(got) != 1 || got[0].ID != "ready" {
		t.Fatalf("public planner returned the stale blocked join: %+v %v", got, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("read-only planning changed the tracker", err)
	}
}

// Operator-gated candidates reach the callers, which report them as skipped
// and treat gated-only queues as drained; eligibility here must not drop them.
func TestActionableSourceLeavesOperatorGatesToCallers(t *testing.T) {
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	data := `{"id":"gated","status":"open","issue_type":"task","labels":["needs-approval"]}` + "\n"
	if err := os.WriteFile(filepath.Join(project, ".beads", "issues.jsonl"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ConfigureProjectOperatorGatedLabels(project, []string{"needs-approval"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ConfigureProjectOperatorGatedLabels(project, nil) })
	got, err := actionableWithWorkSource(context.Background(), project, 0, func(context.Context, string, int) ([]TriageRecommendation, error) {
		return []TriageRecommendation{{ID: "gated", Labels: []string{"needs-approval"}}}, nil
	})
	if err != nil || len(got) != 1 || got[0].ID != "gated" {
		t.Fatalf("operator-gated candidate was dropped before caller classification: %+v %v", got, err)
	}
}

// Through the public entry point an all-ineligible plan is an empty queue, not
// a failed read: assign/watch/spawn report it as nothing to do.
func TestActionablePublicEntryTreatsAllIneligibleAsEmptyQueue(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake tools use a POSIX shell")
	}
	project, _ := sourceGuardFixture(t)
	bin := t.TempDir()
	bvScript := `#!/bin/sh
case "$*" in
  *--robot-plan*) printf '%s\n' '{"plan":{"tracks":[{"track_id":"one","items":[{"id":"blocked","title":"Blocked join","status":"open","priority":1}]}],"summary":{"total_actionable":1}}}' ;;
  *--robot-triage*) printf '%s\n' '{"triage":{"recommendations":[{"id":"blocked","title":"Blocked join","status":"open","priority":1}]}}' ;;
  *) exit 1 ;;
esac
`
	brScript := `#!/bin/sh
case "$*" in
  *ready*|*list*) printf '%s\n' '[{"id":"blocked","status":"open","issue_type":"task","labels":[]}]' ;;
  *) printf '%s\n' '{}' ;;
esac
`
	for name, script := range map[string]string{"bv": bvScript, "br": brScript} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	InvalidateTriageCache()
	t.Cleanup(InvalidateTriageCache)
	got, err := GetActionableRecommendationsContext(context.Background(), project, 0)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("all-ineligible plan was not an empty queue: %+v %v", got, err)
	}
}

// Gated rows are reported to callers but must not use up a capped planner's
// limit (spawn --assign asks for 100), or a gated head of the queue hides the
// eligible work below it.
func TestActionableSourceGatedRowsDoNotConsumeLimit(t *testing.T) {
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	data := `{"id":"gated-a","status":"open","issue_type":"task","labels":["needs-approval"]}` + "\n" +
		`{"id":"gated-b","status":"open","issue_type":"task","labels":["needs-approval"]}` + "\n" +
		`{"id":"ready","status":"open","issue_type":"task"}` + "\n" +
		`{"id":"later","status":"open","issue_type":"task"}` + "\n"
	if err := os.WriteFile(filepath.Join(project, ".beads", "issues.jsonl"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ConfigureProjectOperatorGatedLabels(project, []string{"needs-approval"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ConfigureProjectOperatorGatedLabels(project, nil) })
	got, err := actionableWithWorkSource(context.Background(), project, 1, func(context.Context, string, int) ([]TriageRecommendation, error) {
		return []TriageRecommendation{
			{ID: "gated-a", Labels: []string{"needs-approval"}},
			{ID: "gated-b", Labels: []string{"Needs-Approval"}},
			{ID: "ready"},
			{ID: "later"},
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(got))
	for _, recommendation := range got {
		ids = append(ids, recommendation.ID)
	}
	if strings.Join(ids, ",") != "gated-a,gated-b,ready" {
		t.Fatalf("capped plan = %v, want gated rows reported plus one claimable row", ids)
	}
}

// A project's [assign.work_source] policy reaches the dispatch reader that
// every assignment path shares (GH #283, bd-3vgvw).
func TestActionableSourceAppliesProjectWorkSourcePolicy(t *testing.T) {
	project := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", project}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Skipf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "test@test.com")
	git("config", "user.name", "Test")
	if err := os.Mkdir(filepath.Join(project, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	data := `{"id":"core","status":"open","issue_type":"task"}` + "\n" +
		`{"id":"lms","status":"open","issue_type":"task","labels":["program:lms"]}` + "\n"
	if err := os.WriteFile(filepath.Join(project, ".beads", "issues.jsonl"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", ".beads/issues.jsonl")
	git("commit", "-q", "-m", "tracker")
	git("update-ref", "refs/remotes/origin/main", "HEAD")
	t.Cleanup(func() { _ = ConfigureProjectWorkSourcePolicy(project, worksource.ProjectPolicy{}) })
	collect := func(context.Context, string, int) ([]TriageRecommendation, error) {
		return []TriageRecommendation{{ID: "core"}, {ID: "lms"}}, nil
	}
	read := func(policy worksource.ProjectPolicy) ([]string, error) {
		t.Helper()
		if err := ConfigureProjectWorkSourcePolicy(project, policy); err != nil {
			t.Fatal(err)
		}
		got, err := actionableWithWorkSource(context.Background(), project, 0, collect)
		ids := make([]string, 0, len(got))
		for _, recommendation := range got {
			ids = append(ids, recommendation.ID)
		}
		return ids, err
	}

	if ids, err := read(worksource.ProjectPolicy{}); err != nil || strings.Join(ids, ",") != "core,lms" {
		t.Fatalf("default policy = %v, %v; want both beads", ids, err)
	}
	if ids, err := read(worksource.ProjectPolicy{ProgramLabels: []string{"program:lms"}}); err != nil || strings.Join(ids, ",") != "lms" {
		t.Fatalf("program scope = %v, %v; want only the program:lms bead", ids, err)
	}
	strict := worksource.ProjectPolicy{RequiredRef: "refs/remotes/origin/main", RequireClean: true}
	if ids, err := read(strict); err != nil || len(ids) != 2 {
		t.Fatalf("clean checkout at the required ref = %v, %v; want both beads", ids, err)
	}

	// A local commit the required ref does not have yet.
	git("commit", "-q", "--allow-empty", "-m", "unpushed")
	if ids, err := read(worksource.ProjectPolicy{RequiredRef: "refs/remotes/origin/main"}); !errors.Is(err, worksource.ErrStale) || len(ids) != 0 {
		t.Fatalf("HEAD ahead of the required ref = %v, %v; want STALE_WORK_COORDINATION", ids, err)
	}
	// An uncommitted change.
	if err := os.WriteFile(filepath.Join(project, "scratch.txt"), []byte("wip\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ids, err := read(worksource.ProjectPolicy{RequireClean: true}); !errors.Is(err, worksource.ErrStale) || len(ids) != 0 {
		t.Fatalf("dirty checkout = %v, %v; want STALE_WORK_COORDINATION", ids, err)
	}

	// A strict policy cannot be met by a workspace with no JSONL export.
	dbOnly := t.TempDir()
	t.Cleanup(func() { _ = ConfigureProjectWorkSourcePolicy(dbOnly, worksource.ProjectPolicy{}) })
	if err := ConfigureProjectWorkSourcePolicy(dbOnly, worksource.ProjectPolicy{RequireClean: true}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	_, err := actionableWithWorkSource(context.Background(), dbOnly, 0, func(context.Context, string, int) ([]TriageRecommendation, error) {
		calls++
		return []TriageRecommendation{{ID: "database-only"}}, nil
	})
	if !errors.Is(err, worksource.ErrStale) || calls != 0 {
		t.Fatalf("strict policy over a DB-only workspace = %v after %d tool reads; want STALE before any read", err, calls)
	}
}
