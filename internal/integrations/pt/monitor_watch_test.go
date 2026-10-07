package pt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/config"
	"github.com/Dicklesworthstone/ntm/internal/integrations/rano"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
	"github.com/Dicklesworthstone/ntm/internal/tools"
)

type watchPaneMap struct {
	identities map[int]*rano.PaneIdentity
	err        error
}

func (p *watchPaneMap) RefreshContext(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return p.err
}
func (p *watchPaneMap) GetPIDLabels() map[int]string {
	labels := make(map[int]string, len(p.identities))
	for pid, id := range p.identities {
		labels[pid] = id.String()
	}
	return labels
}
func (p *watchPaneMap) GetPaneForPID(pid int) *rano.PaneIdentity { return p.identities[pid] }

type watchClassifier struct {
	classify func(context.Context, []int) ([]tools.PTProcessResult, error)
}

func (*watchClassifier) IsAvailable(context.Context) bool { return true }
func (*watchClassifier) InvalidateStatusCache()           {}
func (c *watchClassifier) ClassifyProcesses(ctx context.Context, pids []int) ([]tools.PTProcessResult, error) {
	return c.classify(ctx, pids)
}

type watchNetworkSource struct{ stats []tools.RanoProcessStats }

func (*watchNetworkSource) IsAvailable(context.Context) bool { return true }
func (n *watchNetworkSource) GetAllProcessStats(context.Context) ([]tools.RanoProcessStats, error) {
	return n.stats, nil
}

func newWatchTestMonitor() (*HealthMonitor, *watchPaneMap) {
	cfg := config.DefaultProcessTriageConfig()
	cfg.UseRanoData = false
	cfg.CheckInterval = 30
	cfg.StuckThreshold = 120
	m := NewHealthMonitor(&cfg)
	panes := &watchPaneMap{identities: map[int]*rano.PaneIdentity{
		42: {PaneID: "%1", Session: "first", PaneIndex: 0, WindowIndex: 1, PaneTitle: "same", AgentType: tmux.AgentClaude},
		43: {PaneID: "%1", Session: "first", PaneIndex: 0, WindowIndex: 1, PaneTitle: "same", AgentType: tmux.AgentClaude},
		50: {PaneID: "%2", Session: "second", PaneIndex: 0, WindowIndex: 2, PaneTitle: "same", AgentType: tmux.AgentCodex},
		51: {PaneID: "%3", Session: "second", PaneIndex: 1, AgentType: tmux.AgentUser},
	}}
	m.pidMap = panes
	return m, panes
}

func TestPassiveMonitorAggregatesChildrenOnceAndAttributesCallbacks(t *testing.T) {
	m, _ := newWatchTestMonitor()
	m.stuckThreshold = 0
	calls := 0
	m.ptAdapter = &watchClassifier{classify: func(_ context.Context, pids []int) ([]tools.PTProcessResult, error) {
		calls++
		if !reflect.DeepEqual(pids, []int{42, 43, 50}) {
			t.Errorf("wrong process set/order: %v", pids)
		}
		p := 0.98
		results := []tools.PTProcessResult{
			{PID: 42, Classification: tools.PTClassUnknown},
			{PID: 43, Classification: tools.PTClassAbandoned, Confidence: p, AbandonmentProbability: &p, Recommendation: "kill", Source: "pt_agent_watch"},
			{PID: 50, Classification: tools.PTClassUnknown},
		}
		if calls%2 == 0 {
			results[0], results[2] = results[2], results[0]
		}
		return results, nil
	}}
	var changes []ClassificationStateChange
	var alerts []Alert
	m.stateChangeCallbacks = []StateChangeCallback{func(c ClassificationStateChange) { changes = append(changes, c); _ = m.GetAllStates() }}
	m.alertCallbacks = []AlertCallback{func(a Alert) { alerts = append(alerts, a) }}
	m.checkAll()
	m.checkAll()
	states := m.GetAllStates()
	if len(states) != 2 || states["%1"].PID != 43 || states["%1"].Classification != ClassStuck ||
		states["%1"].ConsecutiveCount != 2 || len(states["%1"].History) != 2 || states["%2"].Classification != ClassUnknown {
		t.Fatalf("per-child overwrite or repeated observation: %+v %+v", states["%1"], states["%2"])
	}
	if len(changes) != 2 || len(alerts) != 2 || alerts[0].Session != "first" || alerts[0].Pane != "%1" || changes[0].Session != "first" {
		t.Fatalf("duplicate or unattributed callbacks: %+v %+v", changes, alerts)
	}
	if states["%1"].Session != "first" || states["%1"].WindowIndex != 1 || states["%1"].PaneIndex != 0 ||
		states["%1"].History[1].Recommendation != "kill" || states["%1"].History[1].AbandonmentProbability != 0.98 {
		t.Fatalf("lost source identity/evidence: %+v", states["%1"])
	}
	states["%1"].History[0].Reason = "caller mutation"
	if m.GetAllStates()["%1"].History[0].Reason == "caller mutation" {
		t.Fatal("history alias escaped snapshot")
	}
}

func TestPassiveMonitorFailedOrEmptySamplesClearStaleEvidence(t *testing.T) {
	for _, mode := range []string{"topology", "classifier", "empty", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			m, panes := newWatchTestMonitor()
			m.states["%1"] = &AgentState{Pane: "%1", PID: 43, Classification: ClassStuck, Since: time.Now().Add(-time.Hour)}
			m.ptAdapter = &watchClassifier{classify: func(context.Context, []int) ([]tools.PTProcessResult, error) {
				return nil, errors.New("sample unavailable")
			}}
			switch mode {
			case "topology":
				panes.err = errors.New("tmux unavailable")
			case "empty":
				panes.identities = nil
			case "cancelled":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				m.pollContext = ctx
			}
			m.checkAll()
			if len(m.GetAllStates()) != 0 {
				t.Fatal("stale stuck evidence survived an unavailable sample")
			}
		})
	}
}

func TestPassiveMonitorUsesConnectionEvidenceAndResetsProcessDuration(t *testing.T) {
	m, panes := newWatchTestMonitor()
	m.useRano = true
	m.ranoAdapter = &watchNetworkSource{stats: []tools.RanoProcessStats{{PID: 43, ConnectionCount: 1, LastConnection: time.Now().UTC().Format(time.RFC3339Nano)}}}
	pid := 43
	m.ptAdapter = &watchClassifier{classify: func(context.Context, []int) ([]tools.PTProcessResult, error) {
		return []tools.PTProcessResult{{PID: pid, Classification: tools.PTClassAbandoned, Confidence: 0.99}}, nil
	}}
	m.checkAll()
	if s := m.GetAllStates()["%1"]; s.Classification != ClassWaiting || !s.History[0].NetworkActive {
		t.Fatalf("real connection ignored: %+v", s)
	}
	m.useRano = false
	m.checkAll()
	m.states["%1"].Since = time.Now().Add(-time.Hour)
	panes.identities[44] = panes.identities[43]
	delete(panes.identities, 43)
	pid = 44
	m.checkAll()
	s := m.GetAllStates()["%1"]
	if s.PID != 44 || s.ConsecutiveCount != 1 || len(s.History) != 1 || time.Since(s.Since) > time.Second {
		t.Fatalf("new process inherited old stuck duration: %+v", s)
	}
}

func TestPassiveMonitorStopCancelsAndJoinsActiveSample(t *testing.T) {
	m, _ := newWatchTestMonitor()
	entered := make(chan struct{})
	returned := make(chan struct{})
	m.ptAdapter = &watchClassifier{classify: func(ctx context.Context, _ []int) ([]tools.PTProcessResult, error) {
		close(entered)
		<-ctx.Done()
		close(returned)
		return nil, ctx.Err()
	}}
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("sample did not start")
	}
	stopped := make(chan struct{})
	go func() { m.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel its sample")
	}
	select {
	case <-returned:
	default:
		t.Fatal("Stop returned with sample still running")
	}
	if m.Running() || len(m.GetAllStates()) != 0 {
		t.Fatal("stopped monitor advertises observations")
	}
}

// Native integration over the public monitor lifecycle, the real PT adapter,
// and a CLI-contract subprocess. Only tmux/process attribution is a fixture.
func init() {
	if os.Getenv("NTM_TEST_PT_MONITOR_PROCESS") != "1" || len(os.Args) < 2 || (os.Args[1] != "agent" && os.Args[1] != "--version" && os.Args[1] != "classify") {
		return
	}
	if os.Args[1] == "--version" {
		fmt.Println("pt-core 2.2.1")
		os.Exit(0)
	}
	if reflect.DeepEqual(os.Args[1:], []string{"agent", "watch", "--help"}) {
		fmt.Println("--once --threshold --format")
		os.Exit(0)
	}
	if !reflect.DeepEqual(os.Args[1:], []string{"agent", "watch", "--once", "--threshold", "low", "--format", "jsonl"}) {
		os.Exit(2)
	}
	fmt.Printf("{\"event\":\"candidate_detected\",\"timestamp\":%q,\"pid\":43,\"classification\":\"kill\",\"confidence\":0.99}\n", time.Now().UTC().Format(time.RFC3339Nano))
	os.Exit(0)
}

func TestPassiveHealthMonitorPublicLifecycleUsesRealAdapter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable symlink fixture")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(binary, filepath.Join(bin, "pt")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("NTM_TEST_PT_MONITOR_PROCESS", "1")
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	m, _ := newWatchTestMonitor()
	m.ptAdapter.InvalidateStatusCache()
	defer m.ptAdapter.InvalidateStatusCache()
	changes := make(chan ClassificationStateChange, 4)
	m.stateChangeCallbacks = []StateChangeCallback{func(c ClassificationStateChange) { changes <- c }}
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case c := <-changes:
			if c.Pane != "%1" {
				continue
			}
			if c.Current != ClassStuck || c.Event.Source != "pt_agent_watch" || c.Session != "first" {
				t.Fatalf("passive source not delivered to consumers: %+v", c)
			}
			if m.GetAllStates()["%1"].Confidence != 0.99 {
				t.Fatal("getter lost live sample")
			}
			return
		case <-deadline:
			t.Fatal("passive watch never reached health readers")
		}
	}
}

func TestPassiveMonitorRetainsReportedReviewOverUnreportedSibling(t *testing.T) {
	m, _ := newWatchTestMonitor()
	m.ptAdapter = &watchClassifier{classify: func(context.Context, []int) ([]tools.PTProcessResult, error) {
		probability := 0.85
		return []tools.PTProcessResult{
			{PID: 42, Classification: tools.PTClassUnknown},
			{PID: 43, Classification: tools.PTClassUnknown, Source: "pt_agent_watch", Recommendation: "review", AbandonmentProbability: &probability},
		}, nil
	}}
	m.checkAll()
	state := m.GetAllStates()["%1"]
	if state.PID != 43 || state.Classification != ClassUnknown || state.Confidence != 0 || state.History[0].Recommendation != "review" {
		t.Fatalf("quiet sibling hid actual advisory evidence: %+v", state)
	}
}
