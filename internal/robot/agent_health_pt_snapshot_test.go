package robot

import (
	"testing"

	"github.com/Dicklesworthstone/ntm/internal/integrations/pt"
)

func TestPTHealthSnapshotRequiresObservedLiveAgent(t *testing.T) {
	live := PaneWorkStatus{AgentType: "cc", paneType: "cc", PanePID: 42,
		ObservationFreshness: "fresh", ObservationState: "idle"}
	for _, tc := range []struct {
		name   string
		change func(*PaneWorkStatus)
		want   bool
	}{
		{"live", func(*PaneWorkStatus) {}, true},
		{"no_root", func(p *PaneWorkStatus) { p.PanePID = 0 }, false},
		{"shell", func(p *PaneWorkStatus) { p.paneType = "user" }, false},
		{"unknown", func(p *PaneWorkStatus) { p.AgentType = "unknown" }, false},
		{"no_type", func(p *PaneWorkStatus) { p.AgentType = "" }, false},
		{"dead_cli", func(p *PaneWorkStatus) { p.AgentCLIDead = true }, false},
		{"stale", func(p *PaneWorkStatus) { p.ObservationFreshness = "stale" }, false},
		{"observation_error", func(p *PaneWorkStatus) { p.ObservationError = "unavailable" }, false},
		{"unknown_state", func(p *PaneWorkStatus) { p.ObservationState = "unknown" }, false},
		{"missing_state", func(p *PaneWorkStatus) { p.ObservationState = "" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pane := live
			tc.change(&pane)
			if got := hasPTHealthTargets(map[string]PaneWorkStatus{"1": pane}); got != tc.want {
				t.Fatalf("eligible=%v want=%v", got, tc.want)
			}
			if got := ptHealthMatchesObservation(&pt.AgentState{PanePID: 42, PID: 43}, pane); got != tc.want {
				t.Fatalf("matching child=%v want=%v", got, tc.want)
			}
		})
	}
	if hasPTHealthTargets(nil) {
		t.Fatal("empty selection starts PT")
	}
	if ptHealthMatchesObservation(nil, live) || ptHealthMatchesObservation(&pt.AgentState{PID: 42}, live) ||
		ptHealthMatchesObservation(&pt.AgentState{PanePID: 100, PID: 43}, live) {
		t.Fatal("missing or respawned process inherited a PT observation")
	}
}
