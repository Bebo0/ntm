package cli

import (
	"github.com/Dicklesworthstone/ntm/internal/agentmail"
	"github.com/Dicklesworthstone/ntm/internal/spawnidentity"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

// nextPaneIndices returns the highest NTM pane index currently in use per agent
// type, so `ntm add` can mint the next free slot. Two sources are folded in:
//
//  1. every live pane whose title still parses as session__<type>_<N>;
//  2. every registry title whose bound pane is still live, even though the
//     pane's current title no longer parses because the agent process or the
//     user overwrote it (tmux's allow-set-title guard is best-effort only).
//
// Without (2) a retitled live pane contributed nothing to the scan, its slot
// number was re-issued, the composed title collided with the registry entry,
// and the new pane inherited the running agent's Agent Mail identity (ntm#256).
func nextPaneIndices(panes []tmux.Pane, registry *agentmail.SessionAgentRegistry) map[string]int {
	maxIndices := make(map[string]int)
	fold := func(title string) {
		typeStr, num, ok := paneTitleTypeAndIndex(title)
		if ok && num > maxIndices[typeStr] {
			maxIndices[typeStr] = num
		}
	}
	for _, p := range panes {
		fold(p.Title)
	}
	if registry != nil {
		for _, title := range registry.OccupiedTitles(spawnidentity.LivenessFromPanes(panes)) {
			fold(title)
		}
	}
	return maxIndices
}
