package cli

// Agent Mail pane identity badges (ntm#312). The reconciliation engine lives
// in internal/spawnidentity, shared with robot and REST spawn; this file only
// binds it to the cli configuration for `ntm mapping` and the cli lifecycle
// commands.

import (
	"context"

	"github.com/Dicklesworthstone/ntm/internal/spawnidentity"
)

// paneBadgeTmux is the tmux surface the cli hands to badge reconciliation;
// nil selects the default tmux client. Tests replace it.
var paneBadgeTmux spawnidentity.BadgeTmux

// reconcileSessionBadges runs one full badge reconciliation pass over a
// session with the cli's configured toggle and template (the explicit
// refresh behind `ntm mapping`).
func reconcileSessionBadges(ctx context.Context, session string) (*spawnidentity.BadgeReport, error) {
	return spawnidentity.ReconcileSessionBadges(ctx, spawnidentity.BadgeReconcileOptions{
		Session:  session,
		Enabled:  spawnidentity.BadgesEnabled(cfg),
		Template: spawnidentity.BadgeTemplate(cfg),
		Tmux:     paneBadgeTmux,
	})
}
