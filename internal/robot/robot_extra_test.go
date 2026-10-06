package robot

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/ntm/internal/bv"
	"github.com/Dicklesworthstone/ntm/internal/config"
	"github.com/Dicklesworthstone/ntm/internal/pressure"
	"github.com/Dicklesworthstone/ntm/internal/robot/adapters"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

func TestPrintTerse(t *testing.T) {
	skipSlowRobotShortIntegrationTest(t, "PrintTerse walks live runtime collectors and is too expensive for go test -short")
	if !tmux.IsInstalled() {
		t.Skip("tmux not installed")
	}

	cfg := config.Default()
	output, err := captureStdout(t, func() error { return PrintTerse(cfg) })
	if err != nil {
		t.Fatalf("PrintTerse failed: %v", err)
	}

	// Output format: S:...|... (may be empty if no sessions exist and ListSessions returns empty)
	// When there are no sessions (but tmux is running), output may be just a newline
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		// No sessions - this is valid, skip further checks
		t.Log("No sessions found, output is empty (valid)")
		return
	}
	// Check for S: prefix (session) if there is output
	if !strings.HasPrefix(trimmed, "S:") {
		t.Errorf("Expected output to start with 'S:', got %q", trimmed)
	}
}

func TestPrintTerseNoTmux(t *testing.T) {
	skipSlowRobotShortIntegrationTest(t, "PrintTerseNoTmux still exercises live terse collection and is too expensive for go test -short")
	// Without mocking, we can only test the parsing logic helper if we extract it,
	// or rely on PrintTerse behavior in current env.

	cfg := config.Default()
	output, err := captureStdout(t, func() error { return PrintTerse(cfg) })
	if err != nil {
		t.Fatalf("PrintTerse failed: %v", err)
	}

	parts := parseTerseOutput(output)
	// If output is empty (e.g. no sessions and no alert config), parts might be nil or empty string
	if len(output) > 0 && len(parts) == 0 {
		// It might be just a newline?
		if strings.TrimSpace(output) != "" {
			t.Error("No terse parts found but output not empty")
		}
	}

}

func TestTerseKeyMapUnique(t *testing.T) {
	seen := make(map[string]string, len(TerseKeyMap))
	for longKey, shortKey := range TerseKeyMap {
		if shortKey == "" {
			t.Fatalf("short key is empty for %q", longKey)
		}
		if existing, ok := seen[shortKey]; ok {
			t.Fatalf("short key %q collision: %q and %q", shortKey, existing, longKey)
		}
		seen[shortKey] = longKey
	}
}

func TestGetACFSStatus_MissingBinary(t *testing.T) {
	t.Setenv("PATH", "")

	output, err := GetACFSStatus()
	if err != nil {
		t.Fatalf("GetACFSStatus error: %v", err)
	}
	if output.Success {
		t.Fatalf("expected failure when acfs missing")
	}
	if output.ErrorCode != ErrCodeDependencyMissing {
		t.Fatalf("error_code=%q, want %q", output.ErrorCode, ErrCodeDependencyMissing)
	}
	if output.Tools == nil {
		t.Fatalf("expected tools map to be present")
	}
}

// --robot-suggest decoded bv's SuggestionSet object as a list and failed on
// every call. Run it against the installed bv on two near-duplicate issues.
func TestGetSuggestReadsInstalledBV(t *testing.T) {
	if _, err := exec.LookPath("bv"); err != nil {
		t.Skip("bv not installed")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	issue := func(id, title string) string {
		return `{"id":"` + id + `","title":"` + title + `","description":"Users get logged out when the session token expires during a long form.","status":"open","priority":2,"issue_type":"bug","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}`
	}
	jsonl := issue("sg-a", "Fix login timeout when session expires") + "\n" + issue("sg-b", "Fix login timeout when the session expires") + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".beads", "issues.jsonl"), []byte(jsonl), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	out, err := GetSuggest()
	if err != nil || !out.Success || out.Suggestions == nil {
		t.Fatalf("GetSuggest = %+v, %v; want bv's suggestions", out, err)
	}
	found := false
	for _, s := range out.Suggestions.Suggestions.Suggestions {
		if s.Type == "potential_duplicate" && (s.TargetBead == "sg-a" || s.TargetBead == "sg-b") {
			found = true
		}
	}
	if !found {
		t.Fatalf("suggestions = %+v, want the sg-a/sg-b duplicate", out.Suggestions.Suggestions)
	}
}

// --robot-file-beads decoded an invented files[] shape and returned success
// with nothing in it; bv answers {file_path,total_beads,open_beads,closed_beads}.
func TestGetFileBeadsReadsInstalledBV(t *testing.T) {
	if _, err := exec.LookPath("bv"); err != nil {
		t.Skip("bv not installed")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", "issues.jsonl"), []byte(`{"id":"fb-a","title":"A","status":"open","priority":2,"issue_type":"task","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// bv's file correlation reads git history, so it needs a commit.
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "fb-a: add app"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	t.Chdir(dir)

	out, err := GetFileBeads(FileBeadsOptions{FilePath: "app.go"})
	if err != nil || !out.Success || out.Beads == nil {
		t.Fatalf("GetFileBeads = %+v, %v", out, err)
	}
	if out.Beads.FilePath != "app.go" || out.Beads.OpenBeads == nil {
		t.Fatalf("file beads = %+v, want bv's file_path and bead lists", out.Beads)
	}
}

// br dep tree lists the queried issue itself at depth 0, so --robot-graph's
// correlation reported every bead as its own blocker and dependent.
func TestGetBeadNeighborsExcludesTheQueriedIssue(t *testing.T) {
	if _, err := exec.LookPath("br"); err != nil {
		t.Skip("br not installed")
	}
	dir := t.TempDir()
	brRun := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("br", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("br %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	brRun("init", "--prefix", "zz")
	newID := func(title string) string {
		var created struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(brRun("create", "--title", title, "--type", "task", "--json")), &created); err != nil || created.ID == "" {
			t.Fatalf("br create %q: %v", title, err)
		}
		return created.ID
	}
	blocked, blocker := newID("Blocked work"), newID("Blocker")
	brRun("dep", "add", blocked, blocker)

	if ids, _, err := getBeadNeighbors(dir, blocked, "down"); err != nil || len(ids) != 1 || ids[0] != blocker {
		t.Fatalf("down neighbors of %s = %v, %v; want [%s]", blocked, ids, err, blocker)
	}
	if ids, _, err := getBeadNeighbors(dir, blocker, "up"); err != nil || len(ids) != 1 || ids[0] != blocked {
		t.Fatalf("up neighbors of %s = %v, %v; want [%s]", blocker, ids, err, blocked)
	}
}

func TestAdditionalBVSurfacesReportTypedMissingDependency(t *testing.T) {
	t.Setenv("PATH", "")

	tests := []struct {
		name string
		get  func() (RobotResponse, error)
	}{
		{"forecast", func() (RobotResponse, error) { out, err := GetForecast("all"); return out.RobotResponse, err }},
		{"suggest", func() (RobotResponse, error) { out, err := GetSuggest(); return out.RobotResponse, err }},
		{"impact", func() (RobotResponse, error) {
			out, err := GetImpact("internal/robot/robot.go")
			return out.RobotResponse, err
		}},
		{"search", func() (RobotResponse, error) { out, err := GetSearch("assignment"); return out.RobotResponse, err }},
		{"label-attention", func() (RobotResponse, error) {
			out, err := GetLabelAttention(LabelAttentionOptions{})
			return out.RobotResponse, err
		}},
		{"label-flow", func() (RobotResponse, error) { out, err := GetLabelFlow(); return out.RobotResponse, err }},
		{"label-health", func() (RobotResponse, error) { out, err := GetLabelHealth(); return out.RobotResponse, err }},
		{"file-beads", func() (RobotResponse, error) {
			out, err := GetFileBeads(FileBeadsOptions{})
			return out.RobotResponse, err
		}},
		{"file-hotspots", func() (RobotResponse, error) {
			out, err := GetFileHotspots(FileHotspotsOptions{})
			return out.RobotResponse, err
		}},
		{"file-relations", func() (RobotResponse, error) {
			out, err := GetFileRelations(FileRelationsOptions{})
			return out.RobotResponse, err
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, err := test.get()
			if err != nil {
				t.Fatalf("getter error: %v", err)
			}
			if response.Success || response.ErrorCode != ErrCodeDependencyMissing || strings.TrimSpace(response.Hint) == "" {
				t.Fatalf("response=%+v, want typed dependency failure with remediation hint", response)
			}
		})
	}
}

func TestGetACFSStatus_WithFakeTools(t *testing.T) {
	cleanup := withFakeTools(t)
	defer cleanup()

	output, err := GetACFSStatus()
	if err != nil {
		t.Fatalf("GetACFSStatus error: %v", err)
	}
	if !output.Success {
		t.Fatalf("expected success, got error: %s", output.Error)
	}
	if !output.ACFSAvailable {
		t.Fatalf("expected acfs_available true")
	}
	if output.ACFSVersion == "" {
		t.Fatalf("expected acfs_version to be set")
	}
	if output.Tools == nil {
		t.Fatalf("expected tools map to be present")
	}
	// Ensure core keys exist (installed may vary by environment).
	for _, key := range []string{"tmux", "br", "bv", "cc", "cod", "gmi", "git"} {
		if _, ok := output.Tools[key]; !ok {
			t.Fatalf("missing tool entry for %q", key)
		}
	}
}

func parseTerseOutput(output string) []string {
	// Strip newline
	output = stripNewline(output)
	if output == "" {
		return nil
	}
	return strings.Split(output, ";")
}

func stripNewline(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\n' {
		return s[:len(s)-1]
	}
	return s
}

func TestAttachSnapshotResourcePressureEmitsRobotPressure(t *testing.T) {
	oldCollect := collectSnapshotResourcePressure
	t.Cleanup(func() {
		collectSnapshotResourcePressure = oldCollect
	})
	collectSnapshotResourcePressure = func() *pressure.RobotPressure {
		return &pressure.RobotPressure{
			Success:           true,
			Timestamp:         "2026-05-09T18:30:00Z",
			Mode:              "observe",
			Overall:           "high",
			Limiting:          []string{"proc_count"},
			RecommendedAction: "defer_non_urgent_work",
			Sources: []pressure.RobotSource{{
				Source: "proc_count",
				Value:  0.86,
				Unit:   "ratio",
				Level:  "high",
			}},
		}
	}

	snapshot := newSnapshotOutput(config.Default())
	attachSnapshotResourcePressure(snapshot)

	if snapshot.ResourcePressure == nil {
		t.Fatal("ResourcePressure is nil")
	}
	if snapshot.ResourcePressure.Overall != "high" {
		t.Fatalf("Overall=%q, want high", snapshot.ResourcePressure.Overall)
	}
	if len(snapshot.ResourcePressure.Sources) != 1 || snapshot.ResourcePressure.Sources[0].Source != "proc_count" {
		t.Fatalf("sources=%+v, want proc_count source", snapshot.ResourcePressure.Sources)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("Marshal snapshot: %v", err)
	}
	if !strings.Contains(string(raw), `"resource_pressure"`) || !strings.Contains(string(raw), `"proc_count"`) {
		t.Fatalf("snapshot JSON missing resource pressure projection: %s", raw)
	}
}

func TestBuildTerseOutputFromSnapshotUsesSharedProjection(t *testing.T) {
	snapshot := &SnapshotOutput{
		RobotResponse: NewRobotResponse(true),
		Summary: StatusSummary{
			MailUnread: 2,
			ReadyWork:  3,
			InProgress: 1,
		},
		Sessions: []SnapshotSession{
			{
				Name: "proj",
				Agents: []SnapshotAgent{
					{Type: "claude", State: "active", ContextPercent: 60},
					{Type: "codex", State: "idle", ContextPercent: 80},
					{Type: "gemini", State: "error", ContextPercent: 70},
					{Type: "user", State: "active", ContextPercent: 100},
				},
			},
		},
		BeadsSummary: &bv.BeadsSummary{
			Ready:      3,
			InProgress: 1,
			Blocked:    2,
		},
		AlertSummary: &AlertSummaryInfo{
			BySeverity: map[string]int{
				"critical": 1,
				"warning":  2,
			},
		},
		AttentionSummary: &SnapshotAttentionSummary{
			TotalEvents:         4,
			ActionRequiredCount: 2,
			InterestingCount:    1,
		},
		MailUnread: 2,
	}

	output := buildTerseOutputFromSnapshot(snapshot)
	if output.AttentionHint != "2!action 1?interest" {
		t.Fatalf("attention hint = %q, want compact summary", output.AttentionHint)
	}
	if len(output.States) != 1 {
		t.Fatalf("states len = %d, want 1", len(output.States))
	}

	state := output.States[0]
	if state.Session != "proj" {
		t.Fatalf("session = %q, want proj", state.Session)
	}
	if state.TotalAgents != 4 || state.ActiveAgents != 3 {
		t.Fatalf("agent counts = %+v, want total=4 active=3", state)
	}
	if state.WorkingAgents != 1 || state.IdleAgents != 1 || state.ErrorAgents != 1 {
		t.Fatalf("state counts = %+v, want 1 working/idle/error", state)
	}
	if state.ContextPct != 70 {
		t.Fatalf("context percentage = %d, want average of non-user agents 70", state.ContextPct)
	}
	if state.ReadyBeads != 3 || state.InProgressBead != 1 || state.BlockedBeads != 2 {
		t.Fatalf("work counts = %+v, want ready=3 in_progress=1 blocked=2", state)
	}
	if state.UnreadMail != 2 || state.CriticalAlerts != 1 || state.WarningAlerts != 2 {
		t.Fatalf("coordination counts = %+v", state)
	}
	if got := output.TerseLines[0]; !strings.Contains(got, "S:proj|A:3/4|W:1|I:1|E:1|C:70%") {
		t.Fatalf("terse line = %q, want shared session counts", got)
	}
}

func TestBuildTerseOutputFromSnapshotLeavesContextAtZeroWithoutAgents(t *testing.T) {
	snapshot := &SnapshotOutput{
		RobotResponse: NewRobotResponse(true),
		Sessions: []SnapshotSession{
			{
				Name: "operator-only",
				Agents: []SnapshotAgent{
					{Type: "user", State: "active", ContextPercent: 100},
				},
			},
		},
	}

	output := buildTerseOutputFromSnapshot(snapshot)
	if len(output.States) != 1 {
		t.Fatalf("states len = %d, want 1", len(output.States))
	}
	if state := output.States[0]; state.ActiveAgents != 0 || state.ContextPct != 0 {
		t.Fatalf("operator-only state = %+v, want zero active agents and context", state)
	}
}

func TestBuildTerseOutputFromSnapshotPropagatesFailure(t *testing.T) {
	snapshot := &SnapshotOutput{
		RobotResponse: NewErrorResponse(errors.New("snapshot unavailable"), ErrCodeInternalError, "retry snapshot"),
		Sessions:      []SnapshotSession{},
	}

	output := buildTerseOutputFromSnapshot(snapshot)
	if output.Success || output.ErrorCode != ErrCodeInternalError || output.Error != "snapshot unavailable" {
		t.Fatalf("terse response = %+v, want propagated snapshot failure", output.RobotResponse)
	}
	if output.States == nil || output.TerseLines == nil || len(output.States) != 0 || len(output.TerseLines) != 0 {
		t.Fatalf("terse failure collections = states:%v lines:%v, want non-nil empty slices", output.States, output.TerseLines)
	}
}

func TestRenderMarkdownFromSnapshotUsesRegistrySections(t *testing.T) {
	snapshot := &SnapshotOutput{
		Timestamp: "2026-03-25T03:00:00Z",
		Summary: StatusSummary{
			TotalSessions: 1,
			TotalAgents:   2,
			ReadyWork:     2,
			InProgress:    1,
			AlertsActive:  1,
			MailUnread:    4,
			HealthStatus:  "healthy",
		},
		Sessions: []SnapshotSession{
			{
				Name:     "proj",
				Attached: true,
				Agents: []SnapshotAgent{
					{Type: "claude", State: "active"},
					{Type: "codex", State: "idle"},
				},
			},
		},
		Work: &adapters.WorkSection{
			Available: true,
			Summary: &adapters.WorkSummary{
				Total:      486, // Includes closed work; markdown heading must not use it.
				Ready:      2,
				InProgress: 1,
				Blocked:    1,
			},
			Ready: []adapters.WorkItem{
				{ID: "bd-1", Title: "Ready work"},
			},
			InProgress: []adapters.WorkItem{
				{ID: "bd-2", Title: "Active work", Assignee: "codex"},
			},
		},
		AlertsDetailed: []AlertInfo{
			{Type: "quota", Severity: "critical", Message: "quota exhausted"},
		},
		AlertSummary: &AlertSummaryInfo{
			TotalActive: 1,
			BySeverity: map[string]int{
				"critical": 1,
			},
		},
		AttentionSummary: &SnapshotAttentionSummary{
			TotalEvents:         1,
			ActionRequiredCount: 1,
			TopItems: []SnapshotAttentionItem{
				{Cursor: 9, Category: "quota", Severity: "critical", Summary: "Investigate quota"},
			},
		},
	}

	rendered, err := renderMarkdownFromSnapshot(snapshot, MarkdownOptions{
		IncludeSections: []string{"summary", "sessions", "work", "alerts", "attention"},
	})
	if err != nil {
		t.Fatalf("renderMarkdownFromSnapshot error: %v", err)
	}

	for _, want := range []string{
		"### Summary",
		"### Sessions (1)",
		"### Work (R:2 I:1 B:1 = 4)",
		"### Alerts (1, 1 critical)",
		"### Attention",
		"`bd-1`",
		"Investigate quota",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("markdown missing %q:\n%s", want, rendered)
		}
	}
}

func TestRenderMarkdownFromSnapshotRejectsUnknownSections(t *testing.T) {
	_, err := renderMarkdownFromSnapshot(&SnapshotOutput{}, MarkdownOptions{
		IncludeSections: []string{"mail"},
	})
	if err == nil {
		t.Fatal("expected invalid section error")
	}
	if !strings.Contains(err.Error(), "supported: summary, sessions, work, alerts, attention") {
		t.Fatalf("unexpected error: %v", err)
	}
}
