package status

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

func TestOMPStateRequiresFreshNativeEvidence(t *testing.T) {
	now := time.Now().UTC()
	pane := tmux.Pane{ID: "%42", Type: tmux.AgentOMP, Title: "OSC overwritten title"}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		err    error
		want   AgentState
		safe   bool
	}{
		{name: "idle", want: StateIdle, safe: true},
		{name: "streaming", change: func(s map[string]any) { s["idle"] = false }, want: StateWorking},
		{name: "queued", change: func(s map[string]any) { s["pending"] = true }, want: StateWorking},
		{name: "tool", change: func(s map[string]any) { s["tools"] = 1 }, want: StateWorking},
		{name: "async", change: func(s map[string]any) { s["async_busy"] = true }, want: StateWorking},
		{name: "human draft", change: func(s map[string]any) { s["draft"] = true }, want: StateIdle},
		{name: "stopped", err: errors.New("connection refused"), want: StateUnknown},
		{name: "stale", change: func(s map[string]any) { s["observed_at"] = now.Add(-11 * time.Second) }, want: StateUnknown},
		{name: "future", change: func(s map[string]any) { s["observed_at"] = now.Add(2 * time.Second) }, want: StateUnknown},
		{name: "incomplete", change: func(s map[string]any) { delete(s, "pending") }, want: StateUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{"ready": true, "observed_at": now, "last_activity": now, "idle": true, "pending": false, "draft": false, "tools": 0, "async_busy": false}
			if tc.change != nil {
				tc.change(payload)
			}
			data, _ := json.Marshal(payload)
			got := observeOMPData(pane, data, tc.err, now)
			if got.Status.State != tc.want {
				t.Fatalf("state=%s, want %s", got.Status.State, tc.want)
			}
			if (PaneObservation{Current: got}).SafeToDispatch() != tc.safe {
				t.Fatal("incorrect dispatch safety")
			}
			if tc.want == StateUnknown && (got.Freshness != FreshnessUnavailable || got.Error == "") {
				t.Fatalf("missing unavailable evidence: %+v", got)
			}
			if got.Evidence[0].Provenance != ProvenanceOMPNative {
				t.Fatal("incorrect provenance")
			}
		})
	}
}

func TestOMPObserverIgnoresTerminalAndRetainsUnavailable(t *testing.T) {
	now := time.Now().UTC()
	pane := tmux.PaneActivity{Pane: tmux.Pane{ID: "%42", Type: tmux.AgentOMP}, LastActivity: now}
	unavailable := false
	observer := NewSessionObserverWithDependencies(nil, SessionObserverConfig{}, SessionObserverDependencies{
		Now:       func() time.Time { return now },
		ListPanes: func(context.Context, string) ([]tmux.PaneActivity, error) { return []tmux.PaneActivity{pane}, nil },
		CapturePane: func(context.Context, string, int) (string, error) {
			t.Fatal("OMP status captured terminal")
			return "", nil
		},
		ObserveOMP: func(context.Context, tmux.Pane, time.Time) StateObservation {
			if unavailable {
				return observeOMPData(pane.Pane, nil, errors.New("adapter stopped"), now)
			}
			return StateObservation{Status: AgentStatus{PaneID: "%42", AgentType: "omp", State: StateIdle}, ObservedAt: now, Freshness: FreshnessFresh, Confidence: 1}
		},
	})
	first, err := observer.Observe(context.Background(), "worker")
	if err != nil || !first.Panes[0].SafeToDispatch() {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	unavailable = true
	next := observer.ObservePaneCapture("worker", pane, "Thinking… Rate limit exceeded ❯", nil)
	if next.Current.Status.State != StateUnknown || next.Current.Freshness != FreshnessUnavailable || next.SafeToDispatch() {
		t.Fatalf("fallback to terminal: %+v", next)
	}
	if next.LastKnown == nil || next.LastKnown.Freshness != FreshnessStale {
		t.Fatal("missing explicitly stale last-known state")
	}
	failed, _ := observer.Observe(context.Background(), "worker")
	if failed.Complete || len(failed.Failures) != 1 || failed.Failures[0].Stage != "omp_native" {
		t.Fatalf("missing native failure: %+v", failed)
	}
	for _, output := range []string{"", "Thinking…", "❯", "Rate limit exceeded"} {
		if got := NewDetector().AnalyzeAt("%42", "omp", "omp", output, now, now); got.State != StateUnknown {
			t.Fatalf("terminal classified OMP: %+v", got)
		}
	}
}
