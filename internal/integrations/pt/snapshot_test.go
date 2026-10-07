package pt

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/tools"
)

func TestSampleSessionRejectsUnscopedOrCancelledReads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		session string
		want    error
	}{
		{"nil_context", nil, "session", nil},
		{"all_sessions_refused", context.Background(), "", nil},
		{"cancelled", ctx, "session", context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			states, err := SampleSession(tc.ctx, tc.session)
			if err == nil || states != nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("SampleSession = %v, %v; want no observations and error %v", states, err, tc.want)
			}
		})
	}
}

// Both readers must use the exact same aggregation and evidence semantics;
// one-shot observation cannot become a second classifier or call Start/Stop.
func TestSynchronousHealthSampleMatchesResidentPoll(t *testing.T) {
	snapshot, snapshotPanes := newWatchTestMonitor()
	resident, residentPanes := newWatchTestMonitor()
	for _, panes := range []*watchPaneMap{snapshotPanes, residentPanes} {
		for pid, identity := range panes.identities {
			identity.PanePID = pid
			if pid == 43 {
				identity.PanePID = 42
			}
		}
	}
	classify := func(context.Context, []int) ([]tools.PTProcessResult, error) {
		prob := 0.98
		return []tools.PTProcessResult{{PID: 43, Classification: tools.PTClassAbandoned,
			Confidence: prob, AbandonmentProbability: &prob, Recommendation: "kill", Source: "pt_agent_watch"}}, nil
	}
	snapshot.ptAdapter = &watchClassifier{classify: classify}
	resident.ptAdapter = &watchClassifier{classify: classify}
	if err := snapshot.sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	resident.checkAll()
	snapshots, polled := snapshot.GetAllStates(), resident.GetAllStates()
	if len(snapshots) != 2 || len(polled) != 2 {
		t.Fatalf("lost pane samples: %v %v", snapshots, polled)
	}
	for pane, got := range snapshots {
		want := polled[pane]
		if got.ConsecutiveCount != 1 || len(got.History) != 1 || got.LastCheck.IsZero() || got.Since != got.LastCheck {
			t.Fatalf("snapshot invented history: %+v", got)
		}
		got.Since, want.Since = time.Time{}, time.Time{}
		got.LastCheck, want.LastCheck = time.Time{}, time.Time{}
		got.History[0].Timestamp, want.History[0].Timestamp = time.Time{}, time.Time{}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("one-shot and resident disagree: %+v %+v", got, want)
		}
	}
	if snapshot.Running() || snapshot.pollContext != nil {
		t.Fatal("synchronous read started a background worker")
	}
	state := snapshots["%1"]
	if state.PID != 43 || state.PanePID != 42 || state.Classification != ClassStuck || state.Session != "first" {
		t.Fatalf("child classification lost its root identity: %+v", state)
	}
}

func TestSynchronousHealthSamplePreservesFailuresAndClearsState(t *testing.T) {
	for _, stage := range []string{"topology", "classifier", "cancelled_after_classification", "empty"} {
		t.Run(stage, func(t *testing.T) {
			m, panes := newWatchTestMonitor()
			m.states["%1"] = &AgentState{Classification: ClassStuck}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sentinel := errors.New("sample failure")
			m.ptAdapter = &watchClassifier{classify: func(context.Context, []int) ([]tools.PTProcessResult, error) {
				if stage == "cancelled_after_classification" {
					cancel()
					return nil, nil
				}
				return nil, sentinel
			}}
			want := sentinel
			switch stage {
			case "topology":
				panes.err = sentinel
			case "empty":
				panes.identities = nil
				want = nil
			case "cancelled_after_classification":
				want = context.Canceled
			}
			err := m.sample(ctx)
			if !errors.Is(err, want) {
				t.Fatalf("failure identity lost: got %v want %v", err, want)
			}
			if len(m.GetAllStates()) != 0 {
				t.Fatal("failed/empty sample retained a verdict")
			}
		})
	}
}

func TestSynchronousHealthSampleRootChangeResetsHistory(t *testing.T) {
	m, panes := newWatchTestMonitor()
	for _, identity := range panes.identities {
		identity.PanePID = 42
	}
	m.ptAdapter = &watchClassifier{classify: func(context.Context, []int) ([]tools.PTProcessResult, error) {
		return []tools.PTProcessResult{{PID: 43, Classification: tools.PTClassAbandoned, Confidence: 0.99}}, nil
	}}
	if err := m.sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.states["%1"].Since = time.Now().Add(-time.Hour)
	panes.identities[43].PanePID = 142
	if err := m.sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := m.GetAllStates()["%1"]
	if s.PanePID != 142 || s.PID != 43 || s.ConsecutiveCount != 1 || len(s.History) != 1 || time.Since(s.Since) > time.Second {
		t.Fatalf("new root inherited prior duration: %+v", s)
	}
}
