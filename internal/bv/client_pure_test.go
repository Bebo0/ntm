package bv

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBVClientEstimateSize(t *testing.T) {
	t.Parallel()

	client := NewBVClient()

	tests := []struct {
		name string
		rec  TriageRecommendation
		want string
	}{
		{
			name: "epic_is_large",
			rec: TriageRecommendation{
				Type: "epic",
			},
			want: "large",
		},
		{
			name: "high_betweenness_is_large",
			rec: TriageRecommendation{
				Type: "task",
				Breakdown: &ScoreBreakdown{
					Betweenness: 0.1001,
				},
			},
			want: "large",
		},
		{
			name: "unblocks_many_is_large",
			rec: TriageRecommendation{
				Type:        "task",
				UnblocksIDs: []string{"a", "b", "c", "d"},
				Breakdown: &ScoreBreakdown{
					Betweenness: 0,
				},
			},
			want: "large",
		},
		{
			name: "leaf_low_betweenness_is_small",
			rec: TriageRecommendation{
				Type:        "task",
				UnblocksIDs: nil,
				Breakdown: &ScoreBreakdown{
					Betweenness: 0.01,
				},
			},
			want: "small",
		},
		{
			name: "leaf_without_breakdown_is_medium",
			rec: TriageRecommendation{
				Type:        "task",
				UnblocksIDs: nil,
				Breakdown:   nil,
			},
			want: "medium",
		},
		{
			name: "default_is_medium",
			rec: TriageRecommendation{
				Type:        "task",
				UnblocksIDs: []string{"a"},
				Breakdown: &ScoreBreakdown{
					Betweenness: 0.01,
				},
			},
			want: "medium",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := client.estimateSize(tt.rec); got != tt.want {
				t.Fatalf("estimateSize() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBVClientConvertRecommendation(t *testing.T) {
	t.Parallel()

	client := NewBVClient()

	t.Run("maps_fields_and_infers_actionable", func(t *testing.T) {
		t.Parallel()

		in := TriageRecommendation{
			ID:          "bd-1",
			Title:       "Test task",
			Type:        "task",
			Priority:    2,
			Labels:      []string{"go", "backend"},
			Score:       0.42,
			Action:      "Do the thing",
			Reasons:     []string{"because"},
			UnblocksIDs: []string{"x", "y"},
			BlockedBy:   []string{"b1"},
			Breakdown: &ScoreBreakdown{
				Pagerank:    0.123,
				Betweenness: 0.2, // large (critical path)
			},
		}

		out := client.convertRecommendation(in)

		if out.ID != in.ID {
			t.Fatalf("ID = %q, want %q", out.ID, in.ID)
		}
		if out.Title != in.Title {
			t.Fatalf("Title = %q, want %q", out.Title, in.Title)
		}
		if out.Priority != in.Priority {
			t.Fatalf("Priority = %d, want %d", out.Priority, in.Priority)
		}
		if out.UnblocksCount != 2 {
			t.Fatalf("UnblocksCount = %d, want 2", out.UnblocksCount)
		}
		if out.IsActionable {
			t.Fatalf("IsActionable = true, want false")
		}
		if out.PageRank != 0.123 {
			t.Fatalf("PageRank = %v, want %v", out.PageRank, 0.123)
		}
		if out.Betweenness != 0.2 {
			t.Fatalf("Betweenness = %v, want %v", out.Betweenness, 0.2)
		}
		if out.EstimatedSize != "large" {
			t.Fatalf("EstimatedSize = %q, want %q", out.EstimatedSize, "large")
		}
	})

	t.Run("leaf_with_low_betweenness_is_small_and_actionable", func(t *testing.T) {
		t.Parallel()

		out := client.convertRecommendation(TriageRecommendation{
			ID:          "bd-2",
			Title:       "Leaf",
			Type:        "task",
			UnblocksIDs: nil,
			BlockedBy:   nil,
			Breakdown: &ScoreBreakdown{
				Betweenness: 0.01,
			},
		})

		if !out.IsActionable {
			t.Fatalf("IsActionable = false, want true")
		}
		if out.EstimatedSize != "small" {
			t.Fatalf("EstimatedSize = %q, want %q", out.EstimatedSize, "small")
		}
	})

	t.Run("leaf_without_breakdown_defaults_to_medium", func(t *testing.T) {
		t.Parallel()

		out := client.convertRecommendation(TriageRecommendation{
			ID:          "bd-3",
			Title:       "Leaf without breakdown",
			Type:        "task",
			UnblocksIDs: nil,
			BlockedBy:   nil,
			Breakdown:   nil,
		})

		if out.EstimatedSize != "medium" {
			t.Fatalf("EstimatedSize = %q, want %q", out.EstimatedSize, "medium")
		}
	})
}

func TestBVClientBuildInsightsFromResponse(t *testing.T) {
	t.Parallel()

	client := NewBVClient()

	resp := &InsightsResponse{
		Cycles: [][]string{
			{"A", "B", "C"},
		},
	}

	insights := client.buildInsightsFromResponse(resp, "")

	if insights == nil {
		t.Fatal("expected non-nil insights")
	}
}

// bvCycleInsights is from real `bv --robot-insights` output (bv v0.25.2) on a
// two-issue graph where each issue blocks the other: bv's Insights.Cycles is
// a list of ID lists.
const bvCycleInsights = `{"Bottlenecks":[{"ID":"cy-a","Value":0}],"Cycles":[["cy-a","cy-b","cy-a"]]}`

// ntm decoded Cycles as objects, so --robot-insights failed to parse whenever
// the graph had a cycle: --robot-graph errored and the cycle alert never fired.
func TestInsightsResponseDecodesBVCycles(t *testing.T) {
	var resp InsightsResponse
	if err := json.Unmarshal([]byte(bvCycleInsights), &resp); err != nil {
		t.Fatalf("decode bv insights: %v", err)
	}
	if len(resp.Cycles) != 1 || strings.Join(resp.Cycles[0], ",") != "cy-a,cy-b,cy-a" {
		t.Fatalf("Cycles = %v, want [[cy-a cy-b cy-a]]", resp.Cycles)
	}
	if insights := NewBVClient().buildInsightsFromResponse(&resp, ""); len(insights.Cycles) != 1 {
		t.Fatalf("insights cycles = %v, want the bv cycle", insights.Cycles)
	}
}

// The live contract: run the installed bv on a workspace with a cycle.
func TestGetInsightsReadsCyclesFromInstalledBV(t *testing.T) {
	if _, err := exec.LookPath("bv"); err != nil {
		t.Skip("bv not installed")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	issue := func(id, blockedBy string) string {
		return `{"id":"` + id + `","title":"` + id + `","status":"open","priority":2,"issue_type":"task","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z","dependencies":[{"issue_id":"` + id + `","depends_on_id":"` + blockedBy + `","type":"blocks","created_at":"2026-10-01T00:00:00Z"}]}`
	}
	jsonl := issue("cy-a", "cy-b") + "\n" + issue("cy-b", "cy-a") + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".beads", "issues.jsonl"), []byte(jsonl), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, err := GetInsights(dir)
	if err != nil {
		t.Fatalf("GetInsights with a cyclic graph: %v", err)
	}
	if len(resp.Cycles) == 0 {
		t.Fatalf("Cycles = %v, want the cy-a/cy-b cycle", resp.Cycles)
	}
}

func TestBVClientBuildInsightsFromTriage(t *testing.T) {
	t.Parallel()

	client := NewBVClient()

	triage := &TriageResponse{
		Triage: TriageData{
			QuickRef: TriageQuickRef{
				ActionableCount: 7,
			},
			ProjectHealth: &ProjectHealth{
				StatusDistribution: map[string]int{"total": 123},
				GraphMetrics: &GraphMetrics{
					CycleCount: 2,
				},
			},
			Recommendations: []TriageRecommendation{
				{ID: "bd-a", Breakdown: &ScoreBreakdown{Betweenness: 0.06}},
				{ID: "bd-b", Breakdown: &ScoreBreakdown{Betweenness: 0.05}}, // threshold is strictly greater
				{ID: "bd-c", Breakdown: &ScoreBreakdown{Betweenness: 0.2}},
				{ID: "bd-d"},
			},
		},
	}

	insights := client.buildInsightsFromTriage(triage)

	if insights.TotalCount != 123 {
		t.Fatalf("TotalCount = %d, want 123", insights.TotalCount)
	}
	if insights.ReadyCount != 7 {
		t.Fatalf("ReadyCount = %d, want 7", insights.ReadyCount)
	}
	if len(insights.Cycles) != 2 {
		t.Fatalf("Cycles len = %d, want 2", len(insights.Cycles))
	}

	// Should include only bd-a and bd-c (strictly > 0.05)
	if len(insights.Bottlenecks) != 2 {
		t.Fatalf("Bottlenecks len = %d, want 2", len(insights.Bottlenecks))
	}
	if insights.Bottlenecks[0].ID != "bd-a" || insights.Bottlenecks[0].Betweenness != 0.06 {
		t.Fatalf("Bottlenecks[0] = %#v, want id=bd-a betweenness=0.06", insights.Bottlenecks[0])
	}
	if insights.Bottlenecks[1].ID != "bd-c" || insights.Bottlenecks[1].Betweenness != 0.2 {
		t.Fatalf("Bottlenecks[1] = %#v, want id=bd-c betweenness=0.2", insights.Bottlenecks[1])
	}
}

func TestBVClientBuildInsightsFromTriage_NoHealthMetrics(t *testing.T) {
	t.Parallel()

	client := NewBVClient()
	triage := &TriageResponse{
		Triage: TriageData{
			QuickRef: TriageQuickRef{
				ActionableCount: 3,
			},
			ProjectHealth: nil,
			Recommendations: []TriageRecommendation{
				{ID: "bd-x", Breakdown: &ScoreBreakdown{Betweenness: 0.01}},
			},
		},
	}

	insights := client.buildInsightsFromTriage(triage)

	if insights.TotalCount != 0 {
		t.Fatalf("TotalCount = %d, want 0 when no project health", insights.TotalCount)
	}
	if insights.ReadyCount != 3 {
		t.Fatalf("ReadyCount = %d, want 3", insights.ReadyCount)
	}
	if len(insights.Cycles) != 0 {
		t.Fatalf("Cycles len = %d, want 0", len(insights.Cycles))
	}
	if len(insights.Bottlenecks) != 0 {
		t.Fatalf("Bottlenecks len = %d, want 0", len(insights.Bottlenecks))
	}
}

func TestBVClientBuildInsightsFromTriage_NoRecommendations(t *testing.T) {
	t.Parallel()

	client := NewBVClient()
	triage := &TriageResponse{
		Triage: TriageData{
			QuickRef: TriageQuickRef{
				ActionableCount: 2,
			},
			ProjectHealth: &ProjectHealth{
				StatusDistribution: map[string]int{"total": 42},
				GraphMetrics:       &GraphMetrics{CycleCount: 0},
			},
			Recommendations: nil,
		},
	}

	insights := client.buildInsightsFromTriage(triage)

	if insights.TotalCount != 42 {
		t.Fatalf("TotalCount = %d, want 42", insights.TotalCount)
	}
	if insights.ReadyCount != 2 {
		t.Fatalf("ReadyCount = %d, want 2", insights.ReadyCount)
	}
	if len(insights.Bottlenecks) != 0 {
		t.Fatalf("Bottlenecks len = %d, want 0", len(insights.Bottlenecks))
	}
	if len(insights.Cycles) != 0 {
		t.Fatalf("Cycles len = %d, want 0", len(insights.Cycles))
	}
}
