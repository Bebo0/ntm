package cli

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/agents"
	"github.com/Dicklesworthstone/ntm/internal/assignment"
)

// `ntm agents stats` and the recommend performance bonus read recorded
// assignment outcomes. Nothing used to record them, so stats always showed
// "-" and the bonus ran on a seeded prior.
func TestAgentsStatsReportsRecordedAssignmentOutcomes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	done := base.Add(45 * time.Minute)
	failed := base.Add(2 * time.Hour)

	store := assignment.NewStore("stats-session")
	store.Assignments["bd-1"] = &assignment.Assignment{BeadID: "bd-1", AgentType: "codex", Status: assignment.StatusCompleted, AssignedAt: base, CompletedAt: &done}
	store.Assignments["bd-2"] = &assignment.Assignment{BeadID: "bd-2", AgentType: "codex", Status: assignment.StatusFailed, AssignedAt: base, FailedAt: &failed}
	store.Assignments["bd-3"] = &assignment.Assignment{BeadID: "bd-3", AgentType: "codex", Status: assignment.StatusFailed, AssignedAt: base, FailedAt: &failed}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}

	oldJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJSON })
	out, err := captureStdout(t, runAgentsStats)
	if err != nil {
		t.Fatalf("agents stats: %v", err)
	}
	var stats map[string]agents.Performance
	if err := json.Unmarshal([]byte(out), &stats); err != nil {
		t.Fatalf("agents stats JSON: %v\n%s", err, out)
	}
	codex := stats["codex"]
	if codex.TasksCompleted != 1 || codex.SuccessRate < 0.33 || codex.SuccessRate > 0.34 ||
		codex.AvgCompletionTime != 45*time.Minute || !codex.LastUpdated.Equal(failed) {
		t.Fatalf("codex stats = %+v, want 1 completed, 1/3 success, 45m avg, last activity at the latest failure", codex)
	}
	if claude := stats["claude"]; claude.Measured() {
		t.Fatalf("claude has no outcomes but reports measured stats: %+v", claude)
	}

	// A 1/3 success rate is below the 0.7 bar, so recommend now penalizes it.
	result := newObservedProfileMatcher().ScoreAssignment(agents.AgentTypeCodex, agents.TaskInfo{Title: "fix the parser"})
	if result.PerformanceBonus != -0.1 {
		t.Fatalf("codex performance bonus = %v, want -0.1 from its observed 1/3 success rate", result.PerformanceBonus)
	}
}
