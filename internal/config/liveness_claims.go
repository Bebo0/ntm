//go:build !liveness_audit

package config

import (
	"github.com/Dicklesworthstone/ntm/internal/models"
)

// WS0-G2 claims registered from inside internal/config
// (bd-g2-claims-backlog-o787y). Like every claim file, this one is dropped by
// -tags liveness_audit so the dead-code gate sees readers kept alive only by
// their claim (bd-ir0li).
func init() {
	// models.context_limits is consumed by internal/models.ApplyOverrides,
	// but that package cannot claim it without an import cycle
	// (internal/config imports internal/models), and the field's only
	// dereference is loadWithCWD handing it to ApplyOverrides.
	RegisterReader("models.context_limits", models.ApplyOverrides)

	// memory.enabled is additionally read directly by the send injection
	// path (internal/cli), but a key has exactly one claim.
	RegisterReader("memory.enabled", applyMemoryRecoveryLink)
}
