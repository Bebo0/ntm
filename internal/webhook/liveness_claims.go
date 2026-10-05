//go:build !liveness_audit

package webhook

import (
	"github.com/Dicklesworthstone/ntm/internal/config"
)

// WS0-G2 config-key liveness claims for keys consumed by internal/webhook. See
// internal/config/liveness.go; the liveness_audit tag drops this file for the
// dead-code gate (bd-ir0li).
func init() {
	// [retry] globals and the [retry.webhook] override, applied at startup
	// (retry_policy.go).
	config.RegisterReader("retry.max_attempts", ApplyRetryPolicy)
	config.RegisterReader("retry.initial_delay_ms", ApplyRetryPolicy)
	config.RegisterReader("retry.max_delay_ms", ApplyRetryPolicy)
	config.RegisterReader("retry.backoff_factor", ApplyRetryPolicy)
	config.RegisterReader("retry.jitter", ApplyRetryPolicy)
	config.RegisterReader("retry.webhook.max_attempts", ApplyRetryPolicy)
	config.RegisterReader("retry.webhook.initial_delay_ms", ApplyRetryPolicy)
}
