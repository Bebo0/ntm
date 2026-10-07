package robot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/ntm/internal/integrations/rano"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
	"github.com/Dicklesworthstone/ntm/internal/tools"
)

func TestNormalizeRanoWindowDefault(t *testing.T) {
	got, err := normalizeRanoWindow("")
	if err != nil {
		t.Fatalf("normalizeRanoWindow returned error: %v", err)
	}
	if got != "5m" {
		t.Fatalf("expected default window 5m, got %s", got)
	}
}

func TestNormalizeRanoWindowInvalid(t *testing.T) {
	if _, err := normalizeRanoWindow("5x"); err == nil {
		t.Fatal("expected error for invalid duration, got nil")
	}
}

func TestAggregateRanoStats(t *testing.T) {
	stats := []tools.RanoProcessStats{
		{PID: 100, ConnectionCount: 2, LastConnection: "2026-01-01T00:00:01Z", Providers: map[string]int{"anthropic": 2}},
		{PID: 101, ConnectionCount: 3, LastConnection: "2026-01-01T00:00:02Z", Providers: map[string]int{"anthropic": 1, "unknown": 2}},
		{PID: 200, ConnectionCount: 1, LastConnection: "2026-01-01T00:00:03Z", Providers: map[string]int{"openai": 1}},
	}

	pidLookup := func(pid int) *rano.PaneIdentity {
		switch pid {
		case 100, 101:
			return &rano.PaneIdentity{
				Session:   "s1",
				PaneIndex: 1,
				PaneTitle: "s1__cc_1",
				NTMIndex:  1,
			}
		case 200:
			return &rano.PaneIdentity{
				Session:   "s1",
				PaneIndex: 2,
				PaneTitle: "s1__cc_2",
				NTMIndex:  2,
			}
		default:
			return nil
		}
	}

	allowPane := func(identity *rano.PaneIdentity) bool {
		return identity != nil && identity.PaneTitle == "s1__cc_1"
	}

	panes, total := AggregateRanoStats(stats, pidLookup, allowPane)

	pane, ok := panes["s1__cc_1"]
	if !ok || len(panes) != 1 {
		t.Fatalf("expected only pane s1__cc_1, got %+v", panes)
	}
	if pane.ConnectionCount != 5 {
		t.Fatalf("expected connection count 5, got %d", pane.ConnectionCount)
	}
	if pane.LastConnection != "2026-01-01T00:00:02Z" {
		t.Fatalf("expected last_connection 2026-01-01T00:00:02Z, got %s", pane.LastConnection)
	}
	if pane.Providers["anthropic"].Connections != 3 || pane.Providers["unknown"].Connections != 2 || len(pane.Providers) != 2 {
		t.Fatalf("provider tags not summed per pane: %+v", pane.Providers)
	}
	if !reflect.DeepEqual(pane.PIDs, []int{100, 101}) {
		t.Fatalf("expected pids [100 101], got %v", pane.PIDs)
	}
	if total.ConnectionCount != 5 {
		t.Fatalf("expected total connection count 5 (filtered pane excluded), got %d", total.ConnectionCount)
	}
}

// Exercise the same getter used by --robot-rano-stats with explicit external
// ports. The adapter's real subprocess/JSONL contract is covered in tools.
type ranoStatsFixture struct {
	availability                          *tools.RanoAvailability
	stats                                 []tools.RanoProcessStats
	pids                                  map[int]*rano.PaneIdentity
	availabilityErr, refreshErr, statsErr error
	calls                                 []string
}

func (f *ranoStatsFixture) GetAvailability(context.Context) (*tools.RanoAvailability, error) {
	f.calls = append(f.calls, "availability")
	return f.availability, f.availabilityErr
}
func (f *ranoStatsFixture) Database() (string, error) { return "/data/rano/observer.sqlite", nil }
func (f *ranoStatsFixture) GetAllProcessStatsWithWindow(ctx context.Context, w string) ([]tools.RanoProcessStats, error) {
	f.calls = append(f.calls, "export:"+w)
	return f.stats, f.statsErr
}
func (f *ranoStatsFixture) RefreshContext(context.Context) error {
	f.calls = append(f.calls, "mapping")
	return f.refreshErr
}
func (f *ranoStatsFixture) GetPaneForPID(pid int) *rano.PaneIdentity { return f.pids[pid] }
func readyRanoStatsFixture() *ranoStatsFixture {
	return &ranoStatsFixture{availability: &tools.RanoAvailability{Available: true, Compatible: true, Operational: true, CanReadProc: true}}
}

func TestRanoStatsSurfaceReportsConnectionEvidence(t *testing.T) {
	f := readyRanoStatsFixture()
	first := &rano.PaneIdentity{PaneID: "%7", Session: "workers", PaneIndex: 0, WindowIndex: 1, AgentType: tmux.AgentClaude}
	second := &rano.PaneIdentity{PaneID: "%8", Session: "workers", PaneIndex: 0, WindowIndex: 2, AgentType: tmux.AgentCodex}
	f.pids = map[int]*rano.PaneIdentity{42: first, 43: first, 44: second}
	f.stats = []tools.RanoProcessStats{
		{PID: 43, ConnectionCount: 2, LastConnection: "2026-01-17T10:02:00Z", Providers: map[string]int{"anthropic": 2}},
		{PID: 42, ConnectionCount: 1, LastConnection: "2026-01-17T10:01:00Z", Providers: map[string]int{"openai": 1}},
		{PID: 44, ConnectionCount: 1, Providers: map[string]int{"openai": 1}},
		{PID: 99, ConnectionCount: 20}, // Not part of this live process tree.
	}
	collect := func(ctx context.Context, filter []int) (map[string]tmux.Pane, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded collection")
		}
		if !reflect.DeepEqual(filter, []int{0}) {
			t.Fatalf("lost pane selector: %v", filter)
		}
		f.calls = append(f.calls, "targets")
		return map[string]tmux.Pane{"%7": {ID: "%7"}, "%8": {ID: "%8"}}, nil
	}
	out, err := getRanoStats(RanoStatsOptions{Panes: []int{0}}, f, f, collect)
	if err != nil || !out.Success || len(out.Panes) != 2 || out.Total.ConnectionCount != 4 {
		t.Fatalf("getter lost evidence: %+v %v", out, err)
	}
	pane := out.Panes["%7"]
	if pane.ConnectionCount != 3 || pane.LastConnection != "2026-01-17T10:02:00Z" || !reflect.DeepEqual(pane.PIDs, []int{42, 43}) || pane.Providers["anthropic"].Connections != 2 || pane.WindowIndex != 1 {
		t.Fatalf("untitled pane aggregation reset or lost attribution: %+v", pane)
	}
	if out.Measurement != "connection_events" || out.Attribution != "current_process_tree" || len(out.UnavailableMetrics) != 3 ||
		out.Database != "/data/rano/observer.sqlite" {
		t.Fatalf("missing measurement scope or source database: %+v", out)
	}
	encoded, _ := json.Marshal(out)
	for _, unmeasured := range []string{`"request_count":`, `"bytes_in":`, `"bytes_out":`, `"last_request":`, `"bytes_sent":`, `"bytes_received":`} {
		if strings.Contains(string(encoded), unmeasured) {
			t.Fatalf("fabricated metric in surface: %s", encoded)
		}
	}
	if !reflect.DeepEqual(f.calls, []string{"availability", "targets", "mapping", "export:5m"}) {
		t.Fatalf("not one fleet query: %v", f.calls)
	}
	// Equal titles in separate windows must not merge a different set of PIDs.
	first.PaneTitle = "same-title"
	second.PaneTitle = "same-title"
	panes, total := AggregateRanoStats(f.stats, f.GetPaneForPID, func(*rano.PaneIdentity) bool { return true })
	if len(panes) != 2 || total.ConnectionCount != 4 {
		t.Fatalf("duplicate titles merged identities: %+v", panes)
	}
}

func TestRanoStatsSurfaceFailsWithoutCompleteSources(t *testing.T) {
	for _, stage := range []string{"availability", "targets", "mapping", "export"} {
		t.Run(stage, func(t *testing.T) {
			f := readyRanoStatsFixture()
			failure := errors.New("source failed")
			switch stage {
			case "availability":
				f.availabilityErr = failure
			case "mapping":
				f.refreshErr = failure
			case "export":
				f.statsErr = failure
			}
			f.stats = []tools.RanoProcessStats{{PID: 42, ConnectionCount: 9}}
			collect := func(context.Context, []int) (map[string]tmux.Pane, error) {
				if stage == "targets" {
					return nil, failure
				}
				return map[string]tmux.Pane{}, nil
			}
			out, err := getRanoStats(RanoStatsOptions{}, f, f, collect)
			if err != nil || out.Success || len(out.Panes) != 0 || out.Panes == nil || out.Measurement != "" || out.Total.ConnectionCount != 0 {
				t.Fatalf("failed source advertised measurements: %+v %v", out, err)
			}
			if !strings.Contains(out.Error, "source failed") {
				t.Fatalf("lost source error: %+v", out)
			}
		})
	}
}

// No observer database means rano recorded nothing ntm can read: a missing
// dependency (exit 1, DEPENDENCY_MISSING) naming the database, not a
// successful zero-connection report and not an internal error.
func TestRanoStatsMissingDatabaseIsDependencyMissing(t *testing.T) {
	f := readyRanoStatsFixture()
	f.statsErr = fmt.Errorf("%w at /data/rano/observer.sqlite: start rano's monitor", tools.ErrRanoNoDatabase)
	f.stats = []tools.RanoProcessStats{{PID: 42, ConnectionCount: 9}}
	out, err := getRanoStats(RanoStatsOptions{}, f, f, func(context.Context, []int) (map[string]tmux.Pane, error) {
		return map[string]tmux.Pane{}, nil
	})
	if err != nil || out.Success || out.ErrorCode != ErrCodeDependencyMissing || ExitCodeForResponse(out.RobotResponse) != 1 {
		t.Fatalf("missing database = %+v %v; want DEPENDENCY_MISSING", out, err)
	}
	if out.Database != "/data/rano/observer.sqlite" || !strings.Contains(out.Hint, "sqlite_path") ||
		out.Measurement != "" || out.Total.ConnectionCount != 0 || len(out.Panes) != 0 {
		t.Fatalf("missing database reported as measurement or lost its source: %+v", out)
	}
}

func TestRanoStatsRejectsInvalidWindowBeforeProbing(t *testing.T) {
	for _, window := range []string{"-1m", "0s", "18446744073709551615w"} {
		f := readyRanoStatsFixture()
		out, err := getRanoStats(RanoStatsOptions{Window: window}, f, f, func(context.Context, []int) (map[string]tmux.Pane, error) {
			t.Fatal("invalid window read topology")
			return nil, nil
		})
		if err != nil || out.Success || out.ErrorCode != ErrCodeInvalidFlag || len(f.calls) != 0 {
			t.Fatalf("window %s = %+v %v", window, out, err)
		}
	}
}

func TestRanoPaneSelectionUsesRoleNotIndex(t *testing.T) {
	cases := []struct {
		name string
		pane tmux.Pane
		want bool
	}{
		{"agent-zero", tmux.Pane{Index: 0, Type: tmux.AgentClaude}, true},
		{"user-nonzero", tmux.Pane{Index: 2, Type: tmux.AgentUser}, false},
		{"service", tmux.Pane{Type: tmux.AgentClaude, Service: "cass"}, false},
		{"dead", tmux.Pane{Type: tmux.AgentClaude, Dead: true}, false},
		{"unknown", tmux.Pane{Type: tmux.AgentUnknown}, false},
		{"empty", tmux.Pane{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ranoAgentPane(tc.pane); got != tc.want {
				t.Fatalf("selection = %v, want %v", got, tc.want)
			}
		})
	}
	pane := tmux.Pane{ID: "%7", Title: "changing title", Index: 0, WindowIndex: 2}
	if got := paneIdentityKey(pane, "workers"); got != "%7" {
		t.Fatalf("used mutable identity %q", got)
	}
}

func TestRanoStatsSuccessEmptyIsNotUnavailable(t *testing.T) {
	f := readyRanoStatsFixture()
	out, err := getRanoStats(RanoStatsOptions{Window: "1d"}, f, f, func(context.Context, []int) (map[string]tmux.Pane, error) { return map[string]tmux.Pane{}, nil })
	if err != nil || !out.Success || out.Panes == nil || len(out.Panes) != 0 || out.Measurement != "connection_events" {
		t.Fatalf("checked-empty surface = %+v %v", out, err)
	}
}
