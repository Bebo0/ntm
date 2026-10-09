package robot

import (
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/state"
	"github.com/Dicklesworthstone/ntm/internal/status"
)

func TestOMPProjectionUsesNativeStateOnly(t *testing.T) {
	adapter := NewTmuxAdapter(DefaultTmuxAdapterConfig())
	for _, tc := range []struct {
		name   string
		native *status.StateObservation
		want   state.AgentState
	}{
		{name: "missing", want: state.AgentStateUnknown},
		{name: "unavailable", native: &status.StateObservation{Freshness: status.FreshnessUnavailable, Error: "stopped"}, want: state.AgentStateUnknown},
		{name: "idle", native: &status.StateObservation{Freshness: status.FreshnessFresh, Status: status.AgentStatus{State: status.StateIdle}}, want: state.AgentStateIdle},
		{name: "working", native: &status.StateObservation{Freshness: status.FreshnessFresh, Status: status.AgentStatus{State: status.StateWorking}}, want: state.AgentStateBusy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := Agent{Type: "omp", NativeObservation: tc.native, SecondsSinceOutput: 0, OutputLinesSinceLast: 100, ProcessState: "R", RateLimitDetected: true, LastOutputTS: time.Now()}
			if got := adapter.classifyAgentState(&agent); got != tc.want {
				t.Fatalf("state=%s want=%s", got, tc.want)
			}
			if got := determineState("Thinking… ❯ Rate limit exceeded", "omp"); got != "unknown" {
				t.Fatalf("terminal fallback: %s", got)
			}
		})
	}
}
