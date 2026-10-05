//go:build !liveness_audit

package quota

import (
	"github.com/Dicklesworthstone/ntm/internal/config"
)

// WS0-G2 config-key liveness claims for keys consumed by internal/quota. See
// internal/config/liveness.go; the liveness_audit tag drops this file for the
// dead-code gate (bd-ir0li).
func init() {
	// rotation.thresholds.{warning,critical}_percent, applied at startup
	// (rotation_thresholds.go).
	config.RegisterReader("rotation.thresholds.warning_percent", ApplyRotationThresholds)
	config.RegisterReader("rotation.thresholds.critical_percent", ApplyRotationThresholds)
}
