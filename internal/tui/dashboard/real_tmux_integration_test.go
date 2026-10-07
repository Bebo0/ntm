//go:build integration

package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/events"
	"github.com/Dicklesworthstone/ntm/internal/status"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
	"github.com/Dicklesworthstone/ntm/internal/tui/layout"
	"github.com/Dicklesworthstone/ntm/tests/testutil"
)

func TestRealTmuxDashboardE2E(t *testing.T) {
	testutil.RequireTmuxThrottled(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", testutil.ShortTmuxTempDir(t))

	projectDir := t.TempDir()
	session := fmt.Sprintf("ntm_tui_e2e_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancelCleanup()
		if _, err := tmux.NewClient("").RunContext(cleanupCtx, "kill-server"); err != nil &&
			tmux.ClassifyCommandError(err).Kind != tmux.CommandErrorNoServer {
			t.Errorf("stop isolated tmux server: %v", err)
		}
	})
	createCtx, cancelCreate := context.WithTimeout(context.Background(), 8*time.Second)
	createErr := tmux.CreateSessionContext(createCtx, session, projectDir)
	cancelCreate()
	if createErr != nil {
		t.Fatalf("create isolated tmux session: %v", createErr)
	}

	initial := waitForDashboardPaneCount(t, session, 1)
	codexID := initial[0].ID
	titleCtx, cancelTitle := context.WithTimeout(t.Context(), 8*time.Second)
	err := tmux.SetPaneTitleContext(titleCtx, codexID, session+"__cod_1")
	cancelTitle()
	if err != nil {
		t.Fatalf("title codex pane: %v", err)
	}
	claudeID, err := tmux.DefaultClient.Run(
		"new-window", "-d", "-t", session, "-c", projectDir,
		"-P", "-F", "#{pane_id}", "/bin/sh -i",
	)
	if err != nil {
		t.Fatalf("create second tmux window: %v", err)
	}
	claudeID = strings.TrimSpace(claudeID)
	titleCtx, cancelTitle = context.WithTimeout(t.Context(), 8*time.Second)
	err = tmux.SetPaneTitleContext(titleCtx, claudeID, session+"__cc_1")
	cancelTitle()
	if err != nil {
		t.Fatalf("title claude pane: %v", err)
	}

	realPanes := waitForDashboardPaneCount(t, session, 2)
	assertDuplicateLocalPaneIndices(t, realPanes)
	if paneByID(t, realPanes, codexID).Type != tmux.AgentCodex {
		t.Fatalf("real pane %s was not detected as codex: %+v", codexID, paneByID(t, realPanes, codexID))
	}
	if paneByID(t, realPanes, claudeID).Type != tmux.AgentClaude {
		t.Fatalf("real pane %s was not detected as claude: %+v", claudeID, paneByID(t, realPanes, claudeID))
	}

	m := New(session, projectDir)
	m.width = 140
	m.tier = layout.TierForWidth(m.width)
	m.startupWarmupDone = true
	m.paneOutputCaptureBudget = 0
	m = fetchRealDashboardSession(t, m)
	if !m.initialPaneSnapshotDone || len(m.panes) != 2 {
		t.Fatalf("initial production fetch did not seed both panes: snapshot=%t panes=%+v", m.initialPaneSnapshotDone, m.panes)
	}
	assertDuplicateLocalPaneIndices(t, m.panes)

	m.initialFocusedPaneHydrationDone = true
	m.paneStatus[codexID] = PaneStatus{State: "working", ContextPercent: 71, MailUnread: 3, HealthStatus: "warning"}
	m.paneStatus[claudeID] = PaneStatus{State: "idle", ContextPercent: 19, MailUnread: 1, HealthStatus: "ok"}

	beforeCodex := paneByID(t, m.panes, codexID)
	beforeClaude := paneByID(t, m.panes, claudeID)
	if _, err := tmux.DefaultClient.Run(
		"swap-window",
		"-s", fmt.Sprintf("%s:%d", session, beforeCodex.WindowIndex),
		"-t", fmt.Sprintf("%s:%d", session, beforeClaude.WindowIndex),
	); err != nil {
		t.Fatalf("swap real tmux windows: %v", err)
	}

	m = fetchRealDashboardSession(t, m)
	afterCodex := paneByID(t, m.panes, codexID)
	afterClaude := paneByID(t, m.panes, claudeID)
	if afterCodex.WindowIndex != beforeClaude.WindowIndex || afterClaude.WindowIndex != beforeCodex.WindowIndex {
		t.Fatalf("production topology did not refresh after swap: before=%+v/%+v after=%+v/%+v", beforeCodex, beforeClaude, afterCodex, afterClaude)
	}
	assertDashboardPaneState(t, m, codexID, "working", 71, 3, "warning")
	assertDashboardPaneState(t, m, claudeID, "idle", 19, 1, "ok")
	assertRealDashboardRows(t, m, map[string]string{codexID: "working", claudeID: "idle"})

	newPaneID, err := tmux.DefaultClient.Run(
		"split-window", "-d", "-t", fmt.Sprintf("%s:%d", session, afterClaude.WindowIndex), "-c", projectDir,
		"-P", "-F", "#{pane_id}", "/bin/sh -i",
	)
	if err != nil {
		t.Fatalf("split real tmux pane: %v", err)
	}
	newPaneID = strings.TrimSpace(newPaneID)
	titleCtx, cancelTitle = context.WithTimeout(t.Context(), 8*time.Second)
	err = tmux.SetPaneTitleContext(titleCtx, newPaneID, session+"__agy_1")
	cancelTitle()
	if err != nil {
		t.Fatalf("title added pane: %v", err)
	}
	waitForDashboardPaneCount(t, session, 3)

	m = fetchRealDashboardSession(t, m)
	if _, inherited := m.paneStatus[newPaneID]; inherited {
		t.Fatalf("new physical pane %s inherited an existing pane's state: %+v", newPaneID, m.paneStatus[newPaneID])
	}
	assertDashboardPaneState(t, m, codexID, "working", 71, 3, "warning")
	assertDashboardPaneState(t, m, claudeID, "idle", 19, 1, "ok")
	m.paneStatus[newPaneID] = PaneStatus{State: "error", ContextPercent: 99}
	assertRealDashboardRows(t, m, map[string]string{codexID: "working", claudeID: "idle", newPaneID: "error"})

	killCtx, cancelKill := context.WithTimeout(t.Context(), 8*time.Second)
	err = tmux.KillPaneContext(killCtx, newPaneID)
	cancelKill()
	if err != nil {
		t.Fatalf("remove added real tmux pane: %v", err)
	}
	waitForDashboardPaneCount(t, session, 2)
	m = fetchRealDashboardSession(t, m)
	if _, retained := m.paneStatus[newPaneID]; retained {
		t.Fatalf("removed physical pane %s retained stale dashboard state: %+v", newPaneID, m.paneStatus[newPaneID])
	}
	assertDashboardPaneState(t, m, codexID, "working", 71, 3, "warning")
	assertDashboardPaneState(t, m, claudeID, "idle", 19, 1, "ok")
	assertRealDashboardRows(t, m, map[string]string{codexID: "working", claudeID: "idle"})

	grid := status.StripANSI(m.renderPaneGrid())
	if !strings.Contains(grid, "#0.0") || !strings.Contains(grid, "#1.0") || !strings.Contains(grid, "WORK") || !strings.Contains(grid, "IDLE") {
		t.Fatalf("real multiwindow dashboard grid lost addresses or independent states:\n%s", grid)
	}
}

// The dashboard's uptime and restart badges read the pane shell's start time
// and the session monitor's restart records — the readers --robot-metrics and
// ntm health use — instead of an in-memory tracker nothing wrote to.
func TestRealTmuxDashboardHealthReadsUptimeAndRecordedRestarts(t *testing.T) {
	testutil.RequireTmuxThrottled(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", testutil.ShortTmuxTempDir(t))

	projectDir := t.TempDir()
	session := fmt.Sprintf("ntm_tui_health_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancelCleanup()
		if _, err := tmux.NewClient("").RunContext(cleanupCtx, "kill-server"); err != nil &&
			tmux.ClassifyCommandError(err).Kind != tmux.CommandErrorNoServer {
			t.Errorf("stop isolated tmux server: %v", err)
		}
	})
	if err := tmux.CreateSession(session, projectDir); err != nil {
		t.Fatalf("create isolated tmux session: %v", err)
	}
	paneID := waitForDashboardPaneCount(t, session, 1)[0].ID
	if err := tmux.SetPaneTitle(paneID, session+"__cc_1"); err != nil {
		t.Fatalf("title claude pane: %v", err)
	}

	time.Sleep(1200 * time.Millisecond) // a whole second of pane age
	now := time.Now()
	logPath := filepath.Join(home, ".config", "ntm", "analytics", "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	var lines strings.Builder
	for _, at := range []time.Time{now.Add(-300 * time.Millisecond), now.Add(-100 * time.Millisecond)} {
		line, err := json.Marshal(events.Event{
			Timestamp: at,
			Type:      events.EventAgentRestart,
			Session:   session,
			Data:      events.ToMap(events.AgentLifecycleData{PaneID: paneID}),
		})
		if err != nil {
			t.Fatal(err)
		}
		lines.Write(line)
		lines.WriteByte('\n')
	}
	if err := os.WriteFile(logPath, []byte(lines.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	msg, ok := New(session, projectDir).fetchHealthCmd()().(HealthUpdateMsg)
	if !ok || msg.Err != nil {
		t.Fatalf("fetchHealthCmd = %+v (ok %v)", msg, ok)
	}
	info, ok := msg.Health[paneID]
	if !ok {
		t.Fatalf("no health info for %s: %+v", paneID, msg.Health)
	}
	if info.RestartCount != 2 {
		t.Errorf("restart badge count = %d, want 2", info.RestartCount)
	}
	if info.Uptime < 1 || info.Uptime > 120 {
		t.Errorf("uptime = %ds, want the pane's age (1..120s)", info.Uptime)
	}
}

func fetchRealDashboardSession(t *testing.T, m Model) Model {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := m.fetchSessionDataWithOutputsCtx(ctx)
	if cmd == nil {
		t.Fatal("production dashboard session fetch returned no command")
	}
	msg, ok := cmd().(SessionDataWithOutputMsg)
	if !ok {
		t.Fatalf("production dashboard fetch returned unexpected message type")
	}
	if msg.Err != nil {
		t.Fatalf("production dashboard fetch failed: %v", msg.Err)
	}
	updated, _ := m.Update(msg)
	got, ok := updated.(Model)
	if !ok {
		t.Fatalf("production dashboard update returned %T, want Model", updated)
	}
	got.startupWarmupDone = true
	got.paneOutputCaptureBudget = 0
	got.initialFocusedPaneHydrationDone = true
	return got
}

func waitForDashboardPaneCount(t *testing.T, session string, want int) []tmux.Pane {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var last []tmux.Pane
	var lastErr error
	for ctx.Err() == nil {
		last, lastErr = tmux.GetPanesContext(ctx, session)
		if lastErr == nil && len(last) == want {
			return last
		}
		select {
		case <-ctx.Done():
		case <-time.After(25 * time.Millisecond):
		}
	}
	t.Fatalf("session %s pane count=%d, want %d (last error: %v; panes: %+v)", session, len(last), want, lastErr, last)
	return nil
}

func paneByID(t *testing.T, panes []tmux.Pane, paneID string) tmux.Pane {
	t.Helper()
	for _, pane := range panes {
		if pane.ID == paneID {
			return pane
		}
	}
	t.Fatalf("pane %s not found in %+v", paneID, panes)
	return tmux.Pane{}
}

func assertDuplicateLocalPaneIndices(t *testing.T, panes []tmux.Pane) {
	t.Helper()
	if len(panes) != 2 || panes[0].ID == panes[1].ID || panes[0].WindowIndex == panes[1].WindowIndex || panes[0].Index != panes[1].Index {
		t.Fatalf("expected two physical panes in separate windows sharing one local index, got %+v", panes)
	}
	for _, pane := range panes {
		if key := paneStatusKey(pane); key != pane.ID {
			t.Fatalf("pane %+v stable dashboard key=%q, want physical ID %q", pane, key, pane.ID)
		}
	}
}

func assertDashboardPaneState(t *testing.T, m Model, paneID, state string, contextPercent float64, unread int, health string) {
	t.Helper()
	got, ok := m.paneStatus[paneID]
	if !ok || got.State != state || got.ContextPercent != contextPercent || got.MailUnread != unread || got.HealthStatus != health {
		t.Fatalf("pane %s state=%+v, want state=%s context=%.0f unread=%d health=%s", paneID, got, state, contextPercent, unread, health)
	}
}

func assertRealDashboardRows(t *testing.T, m Model, wantStates map[string]string) {
	t.Helper()
	rows := BuildPaneTableRows(m.panes, m.agentStatuses, m.paneStatus, nil, nil, nil, 0, m.theme)
	if len(rows) != len(wantStates) {
		t.Fatalf("dashboard rows=%d, want %d: %+v", len(rows), len(wantStates), rows)
	}
	addresses := make(map[string]string, len(rows))
	for _, row := range rows {
		wantState, ok := wantStates[row.PaneID]
		if !ok {
			t.Fatalf("unexpected dashboard row: %+v", row)
		}
		if row.Status != wantState {
			t.Fatalf("pane %s row state=%q, want %q: %+v", row.PaneID, row.Status, wantState, row)
		}
		if !strings.Contains(row.Address, ".") {
			t.Fatalf("multiwindow pane %s rendered non-canonical address %q", row.PaneID, row.Address)
		}
		if prior, duplicate := addresses[row.Address]; duplicate {
			t.Fatalf("panes %s and %s rendered duplicate address %q", prior, row.PaneID, row.Address)
		}
		addresses[row.Address] = row.PaneID
	}
}
