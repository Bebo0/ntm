//go:build !liveness_audit

package coordinator

import (
	"github.com/Dicklesworthstone/ntm/internal/config"
)

// WS0-G2 config-key liveness claims for keys consumed by internal/coordinator
// (bd-g2-claims-backlog-o787y). See internal/config/liveness.go; the
// liveness_audit tag drops this file for the dead-code gate (bd-ir0li).
func init() {
	// CAAM auto-failover (caam_failover.go, coordinator.go).
	config.RegisterReader("integrations.caam.auto_failover", newFailoverChecker)
	config.RegisterReader("integrations.caam.failover_providers", newFailoverChecker)
	config.RegisterReader("integrations.caam.reset_horizon_minutes", newFailoverChecker)
	config.RegisterReader("integrations.caam.binary_path", newFailoverChecker)

	// [coordinator] mail-nudge knobs, read in newMailNudgeChecker via the
	// CoordinatorConfig the CLI bridges from TOML (mail_nudge.go).
	config.RegisterReader("coordinator.mail_nudge", newMailNudgeChecker)
	config.RegisterReader("coordinator.nudge_cooldown_seconds", newMailNudgeChecker)
	config.RegisterReader("coordinator.nudge_message", newMailNudgeChecker)

	// [rotation.thresholds] restart triggers (rotation.go, WS6-wire).
	config.RegisterReader("rotation.thresholds.restart_if_tokens_above", newRotationChecker)
	config.RegisterReader("rotation.thresholds.restart_if_session_hours", newRotationChecker)
}
