//go:build !liveness_audit

package context

import (
	"github.com/Dicklesworthstone/ntm/internal/config"
)

// WS0-G2 config-key liveness claims for keys consumed by internal/context
// (bd-g2-claims-backlog-o787y). The rotator receives cfg.ContextRotation via
// internal/coordinator/rotation.go → NewRotator. See
// internal/config/liveness.go.
func init() {
	// Compaction-before-rotation decision in the shared rotation path.
	config.RegisterReader("context_rotation.rotate_threshold", (*Rotator).rotateAgentContext)
	config.RegisterReader("context_rotation.try_compact_first", (*Rotator).rotateAgentContext)
	// Pending-rotation record written by EnqueuePendingRotation.
	config.RegisterReader("context_rotation.confirm_timeout_sec", (*Rotator).createPendingRotation)
	config.RegisterReader("context_rotation.default_confirm_action", (*Rotator).createPendingRotation)
	config.RegisterReader("context_rotation.summary_max_tokens", NewRotator)
}
