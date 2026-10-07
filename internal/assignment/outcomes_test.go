package assignment

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Completed and failed assignments feed agent statistics whether they are
// still in a session's ledger or were already cleared from it: CompleteClear
// archives a finished assignment's outcome before the ledger forgets it.
func TestAggregateOutcomesCountsLiveAndClearedAssignments(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	at := func(minutes int) *time.Time {
		v := base.Add(time.Duration(minutes) * time.Minute)
		return &v
	}

	alpha := NewStore("alpha")
	alpha.Assignments["bd-1"] = &Assignment{BeadID: "bd-1", AgentType: "cc", Status: StatusCompleted,
		AssignedAt: base, StartedAt: at(10), CompletedAt: at(40)}
	alpha.Assignments["bd-2"] = &Assignment{BeadID: "bd-2", AgentType: "claude", Status: StatusFailed,
		AssignedAt: base, FailedAt: at(50)}
	alpha.Assignments["bd-3"] = &Assignment{BeadID: "bd-3", AgentType: "codex", Status: StatusWorking, AssignedAt: base}
	alpha.Assignments["bd-4"] = &Assignment{BeadID: "bd-4", AgentType: "codex", Status: StatusReassigned, AssignedAt: base}
	// A finished assignment whose leases are released: clearing it removes it
	// from the ledger, and its outcome must survive.
	alpha.Assignments["bd-5"] = &Assignment{BeadID: "bd-5", AgentType: "codex", Status: StatusCompleted,
		AssignedAt: base, CompletedAt: at(20), ClearState: ClearStateLeasesReleased, ClearStartedAt: at(21)}
	if err := alpha.Save(); err != nil {
		t.Fatal(err)
	}
	if err := alpha.CompleteClear(t.Context(), "bd-5"); err != nil {
		t.Fatalf("CompleteClear: %v", err)
	}
	if got := alpha.Get("bd-5"); got != nil {
		t.Fatalf("cleared assignment still in the ledger: %+v", got)
	}

	beta := NewStore("beta")
	beta.Assignments["bd-9"] = &Assignment{BeadID: "bd-9", AgentType: "claude", Status: StatusCompleted,
		AssignedAt: base, CompletedAt: at(90)}
	if err := beta.Save(); err != nil {
		t.Fatal(err)
	}

	stats, unreadable, err := AggregateOutcomes()
	if err != nil || unreadable != 0 {
		t.Fatalf("AggregateOutcomes: unreadable=%d err=%v", unreadable, err)
	}
	claude := stats["claude"]
	if claude.Completed != 2 || claude.Failed != 1 {
		t.Fatalf("claude = %+v, want 2 completed (cc and claude spellings, two sessions) and 1 failed", claude)
	}
	if rate, ok := claude.SuccessRate(); !ok || rate < 0.66 || rate > 0.67 {
		t.Fatalf("claude success rate = %v, %v; want 2/3", rate, ok)
	}
	// bd-1: started 10 -> completed 40 = 30m; bd-9: assigned 0 -> 90 = 90m.
	if avg := claude.AvgCompletionTime(); avg != time.Hour {
		t.Fatalf("claude avg completion = %v, want 1h", avg)
	}
	if !claude.LastActivity.Equal(*at(90)) {
		t.Fatalf("claude last activity = %v, want %v", claude.LastActivity, *at(90))
	}
	codex := stats["codex"]
	if codex.Completed != 1 || codex.Failed != 0 || codex.AvgCompletionTime() != 20*time.Minute {
		t.Fatalf("codex = %+v, want the cleared completion only (working and reassigned are not outcomes)", codex)
	}

	// The archive holds exactly the cleared assignment.
	data, err := os.ReadFile(filepath.Join(StorageDir(), "alpha", outcomeArchiveName))
	if err != nil {
		t.Fatalf("outcome archive: %v", err)
	}
	if lines := len(splitLines(string(data))); lines != 1 {
		t.Fatalf("archive has %d records, want 1:\n%s", lines, data)
	}
}

// A torn final append or garbage line does not hide the rest of the archive.
func TestAggregateOutcomesSkipsMalformedArchiveLines(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(StorageDir(), "gamma")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	archive := `{"bead_id":"bd-1","agent_type":"codex","status":"completed","assigned_at":"2026-10-01T12:00:00Z","completed_at":"2026-10-01T12:30:00Z"}
not json
{"bead_id":"bd-2","agent_type":"codex","status":"failed","assigned_at":"2026-10-01T12:00:00Z","failed_at":"2026-10-01T13:00:00Z"}
{"bead_id":"bd-3","agent_t`
	if err := os.WriteFile(filepath.Join(dir, outcomeArchiveName), []byte(archive), 0o600); err != nil {
		t.Fatal(err)
	}
	stats, unreadable, err := AggregateOutcomes()
	if err != nil || unreadable != 0 {
		t.Fatalf("AggregateOutcomes: unreadable=%d err=%v", unreadable, err)
	}
	if got := stats["codex"]; got.Completed != 1 || got.Failed != 1 || got.AvgCompletionTime() != 30*time.Minute {
		t.Fatalf("codex = %+v, want 1 completed (30m) and 1 failed", got)
	}
}

func TestAggregateOutcomesWithNoSessions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stats, unreadable, err := AggregateOutcomes()
	if err != nil || unreadable != 0 || len(stats) != 0 {
		t.Fatalf("AggregateOutcomes on an empty home = %v, %d, %v", stats, unreadable, err)
	}
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if i > start {
				lines = append(lines, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
