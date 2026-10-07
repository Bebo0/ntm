package robot

import "github.com/Dicklesworthstone/ntm/internal/integrations/pt"

// Only observed live agent panes justify invoking a whole-host PT scan. This
// also keeps disabled, shell-only and unavailable-observation requests cheap.
func hasPTHealthTargets(panes map[string]PaneWorkStatus) bool {
	for _, pane := range panes {
		if ptHealthTarget(pane) {
			return true
		}
	}
	return false
}

func ptHealthTarget(pane PaneWorkStatus) bool {
	if _, nonAgent := nonAgentPaneFor(pane); nonAgent {
		return false
	}
	return pane.PanePID > 0 && !pane.AgentCLIDead && pane.AgentType != "" &&
		pane.ObservationState != "" &&
		!isNonAgentPaneType(pane.AgentType) && paneObservationUsableForHealth(pane)
}

// Join on the observed root process as well as the pane topology. A pane may
// have been respawned between the terminal observation and the PT sample. The
// representative PT PID can be a child, so comparing it to PanePID is wrong.
// This is current-tree attribution, not proof against historical PID reuse.
func ptHealthMatchesObservation(state *pt.AgentState, pane PaneWorkStatus) bool {
	return state != nil && ptHealthTarget(pane) &&
		state.PanePID > 0 && state.PanePID == pane.PanePID
}
