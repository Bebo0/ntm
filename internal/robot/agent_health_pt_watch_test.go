package robot

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/integrations/pt"
)

func TestFindPTStateUsesObservedSessionAndWindow(t *testing.T) {
	first := &pt.AgentState{Pane: "%10", Session: "first", WindowIndex: 1, PaneIndex: 0, Classification: pt.ClassStuck}
	second := &pt.AgentState{Pane: "%11", Session: "first", WindowIndex: 2, PaneIndex: 0, Classification: pt.ClassUnknown}
	other := &pt.AgentState{Pane: "%12", Session: "other", WindowIndex: 1, PaneIndex: 0, Classification: pt.ClassUseful}
	states := map[string]*pt.AgentState{"%10": first, "%11": second, "%12": other, "nil": nil}
	for _, tc := range []struct {
		name, session, selector string
		want                    *pt.AgentState
	}{
		{"physical", "first", "1.0", first},
		{"second-window", "first", "2.0", second},
		{"exact-id", "first", "%10", first},
		{"other-session", "other", "1.0", other},
		{"unique-bare", "other", "0", other},
		{"ambiguous-bare", "first", "0", nil},
		{"wrong-session-id", "other", "%10", nil},
		{"absent-session", "absent", "0", nil},
		{"absent-pane", "first", "8.0", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := findPTState(states, tc.session, tc.selector, "claude"); got != tc.want {
				t.Fatalf("lookup (%s,%s) = %+v, want %+v", tc.session, tc.selector, got, tc.want)
			}
		})
	}
	states["legacy__cc_9"] = &pt.AgentState{Classification: pt.ClassUseful}
	if got := findPTState(states, "first", "9", "claude"); got != nil {
		t.Fatalf("live topology fell through to an unrelated title: %+v", got)
	}
}

func TestConvertPTWatchStateRetainsAdviceNotInventedHealth(t *testing.T) {
	now := time.Now().UTC()
	state := &pt.AgentState{Pane: "%10", Session: "first", Classification: pt.ClassUnknown,
		Since: now, LastCheck: now, History: []pt.ClassificationEvent{{
			Source: "pt_agent_watch", Recommendation: "spare", AbandonmentProbability: 0.76,
			Reason: "PT advice, not proof of usefulness", Timestamp: now,
		}}}
	info := convertPTState(state, false)
	if info.Classification != "unknown" || info.Confidence != 0 || info.Source != "pt_agent_watch" ||
		info.Recommendation != "spare" || info.AbandonmentProbability == nil || *info.AbandonmentProbability != 0.76 ||
		info.ObservedAt != now.Format(time.RFC3339Nano) {
		t.Fatalf("passive advice was lost or promoted to health: %+v", info)
	}
	*info.AbandonmentProbability = 1
	if state.History[0].AbandonmentProbability != 0.76 {
		t.Fatal("export mutated monitor evidence")
	}
	state.History[0].Recommendation = ""
	state.History[0].AbandonmentProbability = 0
	data, err := json.Marshal(convertPTState(state, false))
	if err != nil || strings.Contains(string(data), `"abandonment_probability"`) {
		t.Fatalf("omitted candidate became zero-probability evidence: %s (%v)", data, err)
	}
}
