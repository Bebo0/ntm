package tools

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// CautAdapter provides integration with the caut (Cloud API Usage Tracker) tool.
// caut tracks API usage, quotas, and spending across cloud providers like Anthropic, OpenAI, etc.
type CautAdapter struct {
	*BaseAdapter
}

// NewCautAdapter creates a new caut adapter
func NewCautAdapter() *CautAdapter {
	return &CautAdapter{
		BaseAdapter: NewBaseAdapter(ToolCaut, "caut"),
	}
}

// Detect checks if caut is installed
func (a *CautAdapter) Detect() (string, bool) {
	path, err := exec.LookPath(a.BinaryName())
	if err != nil {
		return "", false
	}
	return path, true
}

// Version returns the installed caut version
func (a *CautAdapter) Version(ctx context.Context) (Version, error) {
	ctx, cancel := context.WithTimeout(ctx, a.Timeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, a.BinaryName(), "--version")
	cmd.WaitDelay = time.Second
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return Version{}, fmt.Errorf("failed to get caut version: %w", err)
	}

	return ParseStandardVersion(stdout.String())
}

// Capabilities returns the list of caut capabilities
func (a *CautAdapter) Capabilities(ctx context.Context) ([]Capability, error) {
	caps := []Capability{}

	path, installed := a.Detect()
	if !installed {
		return caps, nil
	}

	ctx, cancel := context.WithTimeout(ctx, a.Timeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, path, "help")
	cmd.WaitDelay = time.Second
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	_ = cmd.Run() // Ignore error, just check output

	output := stdout.String()

	// Check for known capabilities
	if strings.Contains(output, "--json") || strings.Contains(output, "status") {
		caps = append(caps, CapRobotMode)
	}

	return caps, nil
}

// Health checks if caut is functioning correctly
func (a *CautAdapter) Health(ctx context.Context) (*HealthStatus, error) {
	start := time.Now()

	path, installed := a.Detect()
	if !installed {
		return &HealthStatus{
			Healthy:     false,
			Message:     "caut not installed",
			LastChecked: time.Now(),
		}, nil
	}

	// Try to get version as a basic health check
	_, err := a.Version(ctx)
	latency := time.Since(start)

	if err != nil {
		return &HealthStatus{
			Healthy:     false,
			Message:     fmt.Sprintf("caut at %s not responding", path),
			Error:       err.Error(),
			LastChecked: time.Now(),
			Latency:     latency,
		}, nil
	}

	return &HealthStatus{
		Healthy:     true,
		Message:     "caut is healthy",
		LastChecked: time.Now(),
		Latency:     latency,
	}, nil
}

// HasCapability checks if caut has a specific capability
func (a *CautAdapter) HasCapability(ctx context.Context, cap Capability) bool {
	caps, err := a.Capabilities(ctx)
	if err != nil {
		return false
	}
	for _, c := range caps {
		if c == cap {
			return true
		}
	}
	return false
}

// Info returns complete caut tool information
func (a *CautAdapter) Info(ctx context.Context) (*ToolInfo, error) {
	return a.BaseAdapter.Info(ctx, a)
}

// caut-specific types and methods

// CautAvailability represents the availability and compatibility of caut on PATH.
type CautAvailability struct {
	Available   bool      `json:"available"`
	Compatible  bool      `json:"compatible"`
	Version     Version   `json:"version,omitempty"`
	Path        string    `json:"path,omitempty"`
	LastChecked time.Time `json:"last_checked"`
	Error       string    `json:"error,omitempty"`
}

// CautProvider is one provider's quota reading in the usage cache that the
// dashboard and robot quota status read (filled from caut's usage windows).
type CautProvider struct {
	Name      string  `json:"name"`
	Enabled   bool    `json:"enabled"`
	HasQuota  bool    `json:"has_quota"`
	QuotaUsed float64 `json:"quota_used,omitempty"` // 0-100 percentage
}

// CautStatus represents the current caut status
type CautStatus struct {
	Running       bool           `json:"running"`
	Tracking      bool           `json:"tracking"`
	ProviderCount int            `json:"provider_count"`
	Providers     []CautProvider `json:"providers,omitempty"`
	TotalSpend    float64        `json:"total_spend,omitempty"`   // Total spend in USD
	QuotaPercent  float64        `json:"quota_percent,omitempty"` // Overall quota usage 0-100
	LastUpdated   string         `json:"last_updated,omitempty"`  // ISO timestamp
	Error         string         `json:"error,omitempty"`
}

// CautUsage represents usage data for a specific time period
type CautUsage struct {
	Provider     string  `json:"provider"`
	RequestCount int     `json:"request_count"`
	TokensIn     int64   `json:"tokens_in"`
	TokensOut    int64   `json:"tokens_out"`
	Cost         float64 `json:"cost"`
	Period       string  `json:"period"` // "day", "week", "month"
	StartDate    string  `json:"start_date,omitempty"`
	EndDate      string  `json:"end_date,omitempty"`
}

var (
	cautAvailabilityCache  CautAvailability
	cautAvailabilityExpiry time.Time
	cautAvailabilityMutex  sync.RWMutex
	cautAvailabilityTTL    = 5 * time.Minute
	cautMinVersion         = Version{Major: 0, Minor: 1, Patch: 0}
)

func cautLogger() *slog.Logger {
	return slog.Default().With("component", "tools.caut")
}

// GetAvailability returns whether caut is available and compatible, with caching.
func (a *CautAdapter) GetAvailability(ctx context.Context) (*CautAvailability, error) {
	cautAvailabilityMutex.RLock()
	if time.Now().Before(cautAvailabilityExpiry) {
		availability := cautAvailabilityCache
		cautAvailabilityMutex.RUnlock()
		return &availability, nil
	}
	cautAvailabilityMutex.RUnlock()

	cautAvailabilityMutex.Lock()
	defer cautAvailabilityMutex.Unlock()

	// Double-check after acquiring write lock
	if time.Now().Before(cautAvailabilityExpiry) {
		availability := cautAvailabilityCache
		return &availability, nil
	}

	availability := a.fetchAvailability(ctx)

	cautAvailabilityCache = *availability
	cautAvailabilityExpiry = time.Now().Add(cautAvailabilityTTL)

	return availability, nil
}

// InvalidateAvailabilityCache forces the next GetAvailability call to re-check.
func (a *CautAdapter) InvalidateAvailabilityCache() {
	cautAvailabilityMutex.Lock()
	cautAvailabilityExpiry = time.Time{}
	cautAvailabilityMutex.Unlock()
}

// IsAvailable returns true if caut is installed and compatible.
func (a *CautAdapter) IsAvailable(ctx context.Context) bool {
	availability, err := a.GetAvailability(ctx)
	if err != nil || availability == nil {
		return false
	}
	return availability.Available && availability.Compatible
}

func (a *CautAdapter) fetchAvailability(ctx context.Context) *CautAvailability {
	availability := &CautAvailability{
		LastChecked: time.Now(),
	}

	path, err := exec.LookPath(a.BinaryName())
	if err != nil {
		availability.Error = err.Error()
		cautLogger().Debug("caut binary not found", "error", err)
		return availability
	}

	availability.Available = true
	availability.Path = path

	version, err := a.Version(ctx)
	if err != nil {
		availability.Error = err.Error()
		cautLogger().Warn("caut version check failed", "path", path, "error", err)
		return availability
	}

	availability.Version = version
	if !cautCompatible(version) {
		cautLogger().Warn("caut version incompatible", "path", path, "version", version.String(), "min_version", cautMinVersion.String())
		return availability
	}

	availability.Compatible = true
	return availability
}

func cautCompatible(version Version) bool {
	return version.AtLeast(cautMinVersion)
}
