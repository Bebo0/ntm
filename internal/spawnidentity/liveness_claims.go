//go:build !liveness_audit

package spawnidentity

import (
	"github.com/Dicklesworthstone/ntm/internal/config"
)

// WS0-G2 config-key liveness claims for the Agent Mail identity keys this
// package consumes on behalf of every launch surface (cli spawn/add/adopt/
// relaunch, robot spawn, REST spawn). Each claim references the real function
// on the read path; see internal/config/liveness.go for the contract. The
// liveness_audit tag drops this file for the dead-code gate (bd-ir0li).
func init() {
	config.RegisterReader("agent_mail.auto_register", RegistrationEnabled)
	config.RegisterReader("agent_mail.pane_badges", BadgesEnabled)
	config.RegisterReader("agent_mail.pane_badge_format", BadgeTemplate)
}
