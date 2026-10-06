package caut

import (
	"strings"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/agent"
)

// Response is caut's robot-output envelope. The field names follow caut's
// published contract (schemas/caut-v1.schema.json in coding_agent_usage_tracker):
// camelCase keys, and `data` is the array of provider payloads itself.
type Response struct {
	SchemaVersion string            `json:"schemaVersion"` // "caut.v1"
	GeneratedAt   string            `json:"generatedAt"`
	Command       string            `json:"command"` // "usage"
	Data          []ProviderPayload `json:"data"`
	Errors        []string          `json:"errors"`
}

// ProviderPayload contains usage data for one provider
type ProviderPayload struct {
	Provider string        `json:"provider"`
	Account  *string       `json:"account,omitempty"`
	Source   string        `json:"source"` // "web", "cli", "oauth", ...
	Status   *StatusInfo   `json:"status,omitempty"`
	Usage    UsageSnapshot `json:"usage"`
}

// StatusInfo is the provider's status-page reading. Indicator is the
// statuspage.io grade: none, minor, major, critical, maintenance or unknown.
type StatusInfo struct {
	Indicator   string  `json:"indicator"`
	Description *string `json:"description,omitempty"`
	URL         string  `json:"url,omitempty"`
}

// Operational reports whether the status page shows the provider working:
// no incident, or only a minor one.
func (s *StatusInfo) Operational() bool {
	switch strings.ToLower(strings.TrimSpace(s.Indicator)) {
	case "major", "critical":
		return false
	default:
		return true
	}
}

// UsageSnapshot contains rate window information
type UsageSnapshot struct {
	PrimaryRateWindow   *RateWindow `json:"primary,omitempty"`
	SecondaryRateWindow *RateWindow `json:"secondary,omitempty"`
	TertiaryRateWindow  *RateWindow `json:"tertiary,omitempty"`
	Identity            *Identity   `json:"identity,omitempty"`
}

// RateWindow describes a usage rate limiting window
type RateWindow struct {
	UsedPercent      *float64   `json:"usedPercent,omitempty"`
	WindowMinutes    *int       `json:"windowMinutes,omitempty"`
	ResetsAt         *time.Time `json:"resetsAt,omitempty"`
	ResetDescription *string    `json:"resetDescription,omitempty"`
}

// Identity contains account information
type Identity struct {
	AccountEmail *string `json:"accountEmail,omitempty"`
}

// UsageResult is the processed result for NTM consumption
type UsageResult struct {
	SchemaVersion string
	Payloads      []ProviderPayload
	Errors        []string
	FetchedAt     time.Time
}

// IsRateLimited returns true if primary window usage > threshold
func (p *ProviderPayload) IsRateLimited(threshold float64) bool {
	if p.Usage.PrimaryRateWindow == nil || p.Usage.PrimaryRateWindow.UsedPercent == nil {
		return false
	}
	return *p.Usage.PrimaryRateWindow.UsedPercent >= threshold
}

// GetResetTime returns when the primary window resets
func (p *ProviderPayload) GetResetTime() *time.Time {
	if p.Usage.PrimaryRateWindow == nil {
		return nil
	}
	return p.Usage.PrimaryRateWindow.ResetsAt
}

// UsedPercent returns primary window usage percentage
func (p *ProviderPayload) UsedPercent() *float64 {
	if p.Usage.PrimaryRateWindow == nil {
		return nil
	}
	return p.Usage.PrimaryRateWindow.UsedPercent
}

// GetWindowMinutes returns the primary rate window duration in minutes
func (p *ProviderPayload) GetWindowMinutes() *int {
	if p.Usage.PrimaryRateWindow == nil {
		return nil
	}
	return p.Usage.PrimaryRateWindow.WindowMinutes
}

// GetResetDescription returns a human-readable description of when the window resets
func (p *ProviderPayload) GetResetDescription() string {
	if p.Usage.PrimaryRateWindow == nil || p.Usage.PrimaryRateWindow.ResetDescription == nil {
		return ""
	}
	return *p.Usage.PrimaryRateWindow.ResetDescription
}

// GetAccountEmail returns the account email if available
func (p *ProviderPayload) GetAccountEmail() string {
	if p.Usage.Identity == nil || p.Usage.Identity.AccountEmail == nil {
		return ""
	}
	return *p.Usage.Identity.AccountEmail
}

// HasUsageData returns true if the payload contains any usage data
func (p *ProviderPayload) HasUsageData() bool {
	return p.Usage.PrimaryRateWindow != nil ||
		p.Usage.SecondaryRateWindow != nil ||
		p.Usage.TertiaryRateWindow != nil
}

// IsOperational returns true if the provider status indicates it's operational
func (p *ProviderPayload) IsOperational() bool {
	if p.Status == nil {
		return true // Assume operational if no status
	}
	return p.Status.Operational()
}

// AgentTypeToProvider maps NTM agent type to caut provider
func AgentTypeToProvider(agentType string) string {
	switch agent.AgentType(agentType).Canonical() {
	case agent.AgentTypeClaudeCode:
		return "claude"
	case agent.AgentTypeCodex:
		return "codex"
	case agent.AgentTypeGemini, agent.AgentTypeAntigravity:
		// agy (Antigravity) shares Google's auth/quota with Gemini, so for
		// provider/auth identity it reuses the gemini bucket.
		return "gemini"
	case agent.AgentTypeCursor:
		return "cursor"
	default:
		// caut has no windsurf or aider provider; asking for one is an
		// invalid-provider error, not usage data.
		return ""
	}
}

// SupportedProviders returns the caut providers NTM agents map to.
func SupportedProviders() []string {
	return []string{"claude", "codex", "gemini", "cursor"}
}
