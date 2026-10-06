package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// XFAdapter provides integration with the XF (X Find) tool.
// XF is a CLI for indexing and searching X/Twitter data archives,
// supporting full-text search with BM25 ranking via Tantivy.
type XFAdapter struct {
	*BaseAdapter
}

// NewXFAdapter creates a new XF adapter
func NewXFAdapter() *XFAdapter {
	return &XFAdapter{
		BaseAdapter: NewBaseAdapter(ToolXF, "xf"),
	}
}

// Detect checks if xf is installed
func (a *XFAdapter) Detect() (string, bool) {
	path, err := exec.LookPath(a.BinaryName())
	if err != nil {
		return "", false
	}
	return path, true
}

// Version returns the installed xf version
func (a *XFAdapter) Version(ctx context.Context) (Version, error) {
	ctx, cancel := context.WithTimeout(ctx, a.Timeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, a.BinaryName(), "--version")
	cmd.WaitDelay = time.Second
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return Version{}, fmt.Errorf("failed to get xf version: %w", err)
	}

	return ParseStandardVersion(stdout.String())
}

// Capabilities returns the list of xf capabilities
func (a *XFAdapter) Capabilities(ctx context.Context) ([]Capability, error) {
	caps := []Capability{}

	// Check if xf has specific capabilities by examining help output
	path, installed := a.Detect()
	if !installed {
		return caps, nil
	}

	ctx, cancel := context.WithTimeout(ctx, a.Timeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, path, "--help")
	cmd.WaitDelay = time.Second
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	_ = cmd.Run() // Ignore error, just check output

	output := stdout.String()

	// Check for known capabilities
	if strings.Contains(output, "search") {
		caps = append(caps, CapSearch)
	}
	// XF supports JSON output via --output json
	if strings.Contains(output, "output") || strings.Contains(output, "json") {
		caps = append(caps, CapRobotMode)
	}

	return caps, nil
}

// Health checks if xf is functioning correctly
func (a *XFAdapter) Health(ctx context.Context) (*HealthStatus, error) {
	start := time.Now()

	path, installed := a.Detect()
	if !installed {
		return &HealthStatus{
			Healthy:     false,
			Message:     "xf not installed",
			LastChecked: time.Now(),
		}, nil
	}

	// Try to get version as a basic health check
	ver, err := a.Version(ctx)
	latency := time.Since(start)

	if err != nil {
		return &HealthStatus{
			Healthy:     false,
			Message:     fmt.Sprintf("xf at %s not responding", path),
			Error:       err.Error(),
			LastChecked: time.Now(),
			Latency:     latency,
		}, nil
	}

	// Tool health = xf is installed and responds with a parseable version.
	// Whether an archive is *indexed* is operational readiness, NOT tool health:
	// a freshly-installed xf is perfectly healthy with nothing indexed yet.
	// Previously `ntm doctor` HARD-FAILED on a missing default archive, so a
	// healthy xf permanently reported "health check failed" (#202). Index state
	// is advisory context only, and comes from xf itself so XF_DB/XF_INDEX and
	// xf's config are honored instead of guessed.
	versionOK := VersionRegex.MatchString(ver.Raw)
	stats, statsErr := a.archiveStats(ctx)
	msg := xfHealthMessage(ver, versionOK, stats, statsErr)

	return &HealthStatus{
		Healthy:     versionOK,
		Message:     msg,
		LastChecked: time.Now(),
		Latency:     latency,
	}, nil
}

// HasCapability checks if xf has a specific capability
func (a *XFAdapter) HasCapability(ctx context.Context, cap Capability) bool {
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

// Info returns complete xf tool information
func (a *XFAdapter) Info(ctx context.Context) (*ToolInfo, error) {
	return a.BaseAdapter.Info(ctx, a)
}

// XF-specific methods

// XFSearchResult is one record of `xf search --format json`, which prints a
// bare array of these.
type XFSearchResult struct {
	ID         string  `json:"id"`
	Text       string  `json:"text"`
	CreatedAt  string  `json:"created_at,omitempty"`
	ResultType string  `json:"result_type,omitempty"` // tweet, like, dm, grok, bookmark
	Score      float64 `json:"score,omitempty"`
}

// Search performs a full-text search on the indexed archive
func (a *XFAdapter) Search(ctx context.Context, query string, limit int) ([]XFSearchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, a.Timeout())
	defer cancel()

	// --format is xf's global output selector; "--" keeps a query that starts
	// with "-" from being parsed as a flag.
	args := []string{"search", "--format", "json"}
	if limit > 0 {
		args = append(args, "--limit", fmt.Sprintf("%d", limit))
	}
	args = append(args, "--", query)

	cmd := exec.CommandContext(ctx, a.BinaryName(), args...)
	cmd.WaitDelay = time.Second
	stdout := NewLimitedBuffer(10 * 1024 * 1024)
	var stderr bytes.Buffer
	cmd.Stdout = stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, ErrTimeout
		}
		return nil, fmt.Errorf("xf search failed: %w: %s", err, stderr.String())
	}

	// xf prints "[]" for no matches, so output that is not a JSON array is a
	// contract break, not an empty result.
	var results []XFSearchResult
	if err := json.Unmarshal(stdout.Bytes(), &results); err != nil {
		return nil, fmt.Errorf("failed to parse xf search results: %w", err)
	}
	if results == nil {
		results = []XFSearchResult{}
	}

	return results, nil
}

// Doctor runs xf doctor diagnostics
func (a *XFAdapter) Doctor(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, a.Timeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, a.BinaryName(), "doctor")
	cmd.WaitDelay = time.Second
	stdout := NewLimitedBuffer(10 * 1024 * 1024)
	var stderr bytes.Buffer
	cmd.Stdout = stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", ErrTimeout
		}
		return "", fmt.Errorf("xf doctor failed: %w: %s", err, stderr.String())
	}

	return stdout.String(), nil
}

// xfArchiveStats is the subset of `xf stats --format json` that health reports.
type xfArchiveStats struct {
	TweetsCount  int    `json:"tweets_count"`
	IndexBuiltAt string `json:"index_built_at"`
}

// archiveStats asks xf whether an archive is indexed. xf exits non-zero with
// "No archive indexed yet" when there is nothing to search.
func (a *XFAdapter) archiveStats(ctx context.Context) (*xfArchiveStats, error) {
	ctx, cancel := context.WithTimeout(ctx, a.Timeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, a.BinaryName(), "stats", "--format", "json")
	cmd.WaitDelay = time.Second
	stdout := NewLimitedBuffer(1024 * 1024)
	var stderr bytes.Buffer
	cmd.Stdout = stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, ErrTimeout
		}
		reason, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(reason))
	}

	var stats xfArchiveStats
	if err := json.Unmarshal(stdout.Bytes(), &stats); err != nil {
		return nil, fmt.Errorf("failed to parse xf stats: %w", err)
	}
	return &stats, nil
}

func xfHealthMessage(ver Version, versionOK bool, stats *xfArchiveStats, statsErr error) string {
	// `xf --version` prints "xf X.Y.Z" followed by build lines.
	name, _, _ := strings.Cut(strings.TrimSpace(ver.Raw), "\n")
	if name = strings.TrimSpace(name); name == "" {
		name = ver.String()
	}
	if !strings.HasPrefix(name, "xf") {
		name = "xf " + name
	}
	parts := []string{name, fmt.Sprintf("version_ok=%t", versionOK)}

	if statsErr != nil || stats == nil {
		parts = append(parts, "index_valid=false")
		if statsErr != nil {
			parts = append(parts, fmt.Sprintf("stats_err=%q", statsErr.Error()))
		}
	} else {
		parts = append(parts, "index_valid=true", fmt.Sprintf("tweet_count=%d", stats.TweetsCount))
		if stats.IndexBuiltAt != "" {
			parts = append(parts, fmt.Sprintf("index_built_at=%s", stats.IndexBuiltAt))
		}
	}

	return strings.Join(parts, " ")
}
