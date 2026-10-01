package robot

// Regression tests for GitHub issue #335: a plain user shell restored next to
// agent panes was run through the agent stall heuristic. Its prompt is
// whatever the user configured, so "no output for 30s and no recognised
// agent prompt" made every idle shell an error, a shell-only session
// critical, and --robot-agent-health graded each shell F.

import (
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/state"
	statuspkg "github.com/Dicklesworthstone/ntm/internal/status"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

// quietAgent is a pane last seen producing output ten minutes ago, past the
// adapter's stall threshold.
func quietAgent(pane, agentType string) Agent {
	return Agent{
		Type:               agentType,
		Pane:               pane,
		PID:                10,
		LastOutputTS:       time.Now().Add(-10 * time.Minute),
		SecondsSinceOutput: 600,
	}
}

// A themed zsh prompt that no agent idle detector recognises.
const themedZshTail = "~/projects/app on main\n❯ "

func TestClassifyAgentState_NonAgentPaneIsNeverAnError(t *testing.T) {
	a := NewTmuxAdapter(DefaultTmuxAdapterConfig())
	for _, typ := range []string{"user", "unknown"} {
		t.Run("type="+typ, func(t *testing.T) {
			agent := quietAgent("%1", typ)
			if got := a.classifyAgentState(&agent, themedZshTail); got != state.AgentStateIdle {
				t.Errorf("quiet shell = %q, want idle", got)
			}

			// Text the user happened to print is not an agent being throttled.
			limited := quietAgent("%2", typ)
			limited.RateLimitDetected = true
			limited.RateLimitMatch = "rate limit exceeded"
			if got := a.classifyAgentState(&limited, "grep 'rate limit exceeded' app.log\n❯ "); got == state.AgentStateError {
				t.Errorf("shell scrollback with a rate-limit phrase = error")
			}

			// Fresh output still reads as activity.
			busy := Agent{Type: typ, Pane: "%3", PID: 10, LastOutputTS: time.Now(), SecondsSinceOutput: 1, OutputLinesSinceLast: 4}
			if got := a.classifyAgentState(&busy, "building...\n"); got != state.AgentStateBusy {
				t.Errorf("shell printing output = %q, want busy", got)
			}

			if status, _ := a.computeAgentHealth(&agent, a.classifyAgentState(&agent, themedZshTail)); status == state.HealthStatusCritical {
				t.Errorf("quiet shell health = critical")
			}
		})
	}

	// The stall heuristic still applies to agents.
	stalled := quietAgent("%4", "claude")
	if got := a.classifyAgentState(&stalled, "partial output with no prompt"); got != state.AgentStateError {
		t.Errorf("stalled claude pane = %q, want error", got)
	}
}

func TestNormalizeSnapshot_UserShellsDoNotMakeSessionsCritical(t *testing.T) {
	a := NewTmuxAdapter(DefaultTmuxAdapterConfig())

	shellOnly := tmux.Session{Name: "restored-shell"}
	mixed := tmux.Session{Name: "mixed"}
	failed := tmux.Session{Name: "failed-agent"}

	agents := map[string][]Agent{
		shellOnly.Name: {quietAgent("%1", "user")},
		mixed.Name:     {quietAgent("%2", "user"), quietAgent("%3", "unknown"), {Type: "codex", Pane: "%4", PID: 10}},
		failed.Name:    {quietAgent("%5", "claude"), quietAgent("%6", "user")},
	}
	tails := map[string]map[string]string{
		shellOnly.Name: {"%1": themedZshTail},
		mixed.Name:     {"%2": themedZshTail, "%3": themedZshTail, "%4": "some earlier output\n› "},
		failed.Name:    {"%5": "partial output with no prompt", "%6": themedZshTail},
	}

	snapshot := a.NormalizeSnapshot([]tmux.Session{shellOnly, mixed, failed}, agents, tails)
	bySession := map[string]state.RuntimeSession{}
	for _, s := range snapshot.Sessions {
		bySession[s.Name] = s
	}

	for _, name := range []string{shellOnly.Name, mixed.Name} {
		s := bySession[name]
		if s.ErrorAgents != 0 || s.HealthStatus == state.HealthStatusCritical || s.HealthStatus == state.HealthStatusWarning {
			t.Errorf("%s: error_agents=%d health=%s (%s), want no errors", name, s.ErrorAgents, s.HealthStatus, s.HealthReason)
		}
	}
	for _, ra := range snapshot.Agents {
		if (ra.AgentType == "user" || ra.AgentType == "unknown") && ra.State == state.AgentStateError {
			t.Errorf("non-agent pane %s projected as error", ra.Pane)
		}
	}

	// A real failure is still critical, and the user shell beside it does
	// not dilute "all agents failed" into a warning.
	s := bySession[failed.Name]
	if s.HealthStatus != state.HealthStatusCritical || !strings.Contains(s.HealthReason, "all 1 agents") {
		t.Errorf("failed-agent session = %s (%s), want critical: all 1 agents in error state", s.HealthStatus, s.HealthReason)
	}
}

func TestAgentHealth_NonAgentPanesAreNotGraded(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   PaneWorkStatus
		want     bool
		wantType string
	}{
		{name: "user shell", status: PaneWorkStatus{AgentType: "unknown", paneType: "user"}, want: true, wantType: "user"},
		{name: "unattributed pane", status: PaneWorkStatus{AgentType: "unknown", paneType: "unknown"}, want: true, wantType: "unknown"},
		{name: "agent", status: PaneWorkStatus{AgentType: "claude", paneType: "claude"}, want: false},
		// Not observed at all: keep the "observation unavailable" grade.
		{name: "missing pane", status: PaneWorkStatus{AgentType: "unknown"}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := nonAgentPaneFor(tc.status)
			if ok != tc.want {
				t.Fatalf("nonAgentPaneFor ok = %v, want %v", ok, tc.want)
			}
			if ok && (got.AgentType != tc.wantType || got.Reason == "") {
				t.Errorf("nonAgentPaneFor = %+v, want type %q with a reason", got, tc.wantType)
			}
		})
	}
}

func TestPaneWorkStatusRecordsTmuxPaneType(t *testing.T) {
	obs := statuspkg.PaneObservation{Metadata: tmux.Pane{ID: "%7", Type: tmux.AgentUser, Title: "myproject__user"}}
	if got := paneWorkStatusFromObservation(obs).paneType; got != "user" {
		t.Errorf("paneType = %q, want user", got)
	}
	if got := paneWorkStatusFromObservation(statuspkg.PaneObservation{}).paneType; got != "" {
		t.Errorf("unobserved pane paneType = %q, want empty", got)
	}
}
