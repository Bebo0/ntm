package panels

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/status"
	"github.com/Dicklesworthstone/ntm/internal/tui/theme"
)

// =============================================================================
// Panel rendering integration tests
// =============================================================================

func TestRanoNetworkPanelViewDisabled(t *testing.T) {
	panel := NewRanoNetworkPanel()
	panel.SetSize(60, 12)
	panel.SetData(RanoNetworkPanelData{
		Loaded:  true,
		Enabled: false,
	})

	out := status.StripANSI(panel.View())
	if !strings.Contains(out, "rano disabled") {
		t.Fatalf("expected disabled state, got:\n%s", out)
	}
}

func TestRanoNetworkPanelViewWithRowsExpanded(t *testing.T) {
	panel := NewRanoNetworkPanel()
	panel.SetSize(80, 16)
	panel.SetData(RanoNetworkPanelData{
		Loaded:       true,
		Enabled:      true,
		Available:    true,
		Version:      "0.2.1",
		PollInterval: 1 * time.Second,
		Window:       5 * time.Minute,
		Rows: []RanoNetworkRow{
			{
				Label:          "proj__cc_1",
				AgentType:      "cc",
				Connections:    3,
				LastConnection: time.Now().Add(-100 * time.Millisecond),
				Providers:      map[string]int{"anthropic": 3},
			},
			{
				Label:          "proj__cod_1",
				AgentType:      "cod",
				Connections:    1,
				LastConnection: time.Now().Add(-10 * time.Second),
				Providers:      map[string]int{"openai": 1},
			},
		},
		TotalConnections: 4,
	})

	out := status.StripANSI(panel.View())
	for _, want := range []string{
		"Network Activity",
		"proj__cc_1",
		"proj__cod_1",
		"Conn",
		"Total: 4 conn in the last 5m",
		"By provider:",
		"anthropic: 3",
		"openai: 1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected output to contain %q, got:\n%s", want, out)
		}
	}
	for _, fabricated := range []string{"req", "KB", " out", " in\n"} {
		if strings.Contains(out, fabricated) {
			t.Fatalf("panel shows an unmeasured request/byte metric %q:\n%s", fabricated, out)
		}
	}
}

func TestRanoNetworkPanelViewNotAvailable(t *testing.T) {
	panel := NewRanoNetworkPanel()
	panel.SetSize(60, 12)
	panel.SetData(RanoNetworkPanelData{
		Loaded:    true,
		Enabled:   true,
		Available: false,
	})

	out := status.StripANSI(panel.View())
	if !strings.Contains(out, "rano not available") {
		t.Fatalf("expected not-available state, got:\n%s", out)
	}
}

func TestRanoNetworkPanelViewError(t *testing.T) {
	panel := NewRanoNetworkPanel()
	panel.SetSize(80, 14)
	panel.SetData(RanoNetworkPanelData{
		Loaded:  true,
		Enabled: true,
		Error:   errors.New("rano observer database not found at /srv/observer.sqlite"),
	})

	out := status.StripANSI(panel.View())
	if !strings.Contains(out, "Network stats unavailable") || !strings.Contains(out, "database not found") {
		t.Fatalf("expected unavailable reason, got:\n%s", out)
	}
	if strings.Contains(out, "No agent connections") {
		t.Fatalf("missing source rendered as zero traffic:\n%s", out)
	}
}

func TestRanoNetworkPanelViewNoConnections(t *testing.T) {
	panel := NewRanoNetworkPanel()
	panel.SetSize(60, 12)
	panel.SetData(RanoNetworkPanelData{
		Loaded:    true,
		Enabled:   true,
		Available: true,
		Window:    5 * time.Minute,
		Rows:      nil,
	})

	out := status.StripANSI(panel.View())
	if !strings.Contains(out, "No agent connections") || !strings.Contains(out, "last 5m") {
		t.Fatalf("expected no-connections state, got:\n%s", out)
	}
}

func TestRanoNetworkPanelViewCompact(t *testing.T) {
	panel := NewRanoNetworkPanel()
	panel.SetSize(80, 10) // h < 14, compact mode
	panel.SetData(RanoNetworkPanelData{
		Loaded:       true,
		Enabled:      true,
		Available:    true,
		PollInterval: 1 * time.Second,
		Rows: []RanoNetworkRow{
			{
				Label:          "proj__cc_1",
				AgentType:      "cc",
				Connections:    5,
				LastConnection: time.Now(),
				Providers:      map[string]int{"anthropic": 5},
			},
		},
		TotalConnections: 5,
	})

	out := status.StripANSI(panel.View())
	if !strings.Contains(out, "proj__cc_1") {
		t.Fatalf("expected agent row, got:\n%s", out)
	}
	// Compact mode should NOT show totals or provider breakdown
	if strings.Contains(out, "Total:") || strings.Contains(out, "By provider:") {
		t.Fatalf("compact mode should not show totals, got:\n%s", out)
	}
}

func TestRanoNetworkPanelViewZeroSize(t *testing.T) {
	panel := NewRanoNetworkPanel()
	panel.SetSize(0, 0)
	panel.SetData(RanoNetworkPanelData{Loaded: true})

	out := panel.View()
	if out != "" {
		t.Fatalf("expected empty string for zero size, got: %q", out)
	}
}

func TestRanoNetworkPanelHasData(t *testing.T) {
	panel := NewRanoNetworkPanel()

	if panel.HasData() {
		t.Fatal("HasData() should be false before SetData")
	}

	panel.SetData(RanoNetworkPanelData{Loaded: true})
	if !panel.HasData() {
		t.Fatal("HasData() should be true after SetData with Loaded=true")
	}

	panel2 := NewRanoNetworkPanel()
	panel2.SetData(RanoNetworkPanelData{Error: errors.New("test")})
	if !panel2.HasData() {
		t.Fatal("HasData() should be true when Error is set")
	}
}

func TestRanoNetworkPanelViewWithVersion(t *testing.T) {
	panel := NewRanoNetworkPanel()
	panel.SetSize(80, 16)
	panel.SetData(RanoNetworkPanelData{
		Loaded:    true,
		Enabled:   true,
		Available: true,
		Version:   "1.2.3",
		Rows: []RanoNetworkRow{
			{Label: "test_agent", AgentType: "cc", Connections: 1, LastConnection: time.Now()},
		},
		TotalConnections: 1,
	})

	out := status.StripANSI(panel.View())
	if !strings.Contains(out, "1.2.3") {
		t.Fatalf("expected version in output, got:\n%s", out)
	}
}

// =============================================================================
// Pure helper function tests
// =============================================================================

func TestRenderActivity(t *testing.T) {
	t.Parallel()
	poll := 1 * time.Second

	tests := []struct {
		name string
		last time.Time
		want string
	}{
		{"zero time", time.Time{}, "(idle)"},
		{"just now", time.Now(), "▲▲▲"},
		{"3 seconds ago", time.Now().Add(-3 * time.Second), "▲▲"},
		{"20 seconds ago", time.Now().Add(-20 * time.Second), "▲"},
		{"5 minutes ago", time.Now().Add(-5 * time.Minute), "(idle)"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := renderActivity(tc.last, poll)
			if got != tc.want {
				t.Errorf("renderActivity(%v, %v) = %q; want %q", tc.last, poll, got, tc.want)
			}
		})
	}
}

func TestRenderActivity_ZeroPollInterval(t *testing.T) {
	t.Parallel()
	// Zero poll interval should default to 1s
	got := renderActivity(time.Now(), 0)
	if got != "▲▲▲" {
		t.Errorf("renderActivity(now, 0) = %q; want %q", got, "▲▲▲")
	}
}

func TestFormatConnectionAge(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		last time.Time
		want string
	}{
		{"never", time.Time{}, "-"},
		{"seconds", time.Now().Add(-12 * time.Second), "12s"},
		{"future clock skew", time.Now().Add(time.Minute), "0s"},
		{"minutes", time.Now().Add(-3*time.Minute - time.Second), "3m"},
		{"hours", time.Now().Add(-2*time.Hour - time.Minute), "2h"},
		{"days", time.Now().Add(-49 * time.Hour), "2d"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := formatConnectionAge(tc.last); got != tc.want {
				t.Errorf("formatConnectionAge(%s) = %q; want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestRanoWindowLabel(t *testing.T) {
	t.Parallel()
	for window, want := range map[time.Duration]string{
		0:                "recently",
		5 * time.Minute:  "in the last 5m",
		2 * time.Hour:    "in the last 2h",
		90 * time.Second: "in the last 1m30s",
	} {
		if got := ranoWindowLabel(window); got != want {
			t.Errorf("ranoWindowLabel(%v) = %q; want %q", window, got, want)
		}
	}
}

func TestRenderRanoTable_EmptyRows(t *testing.T) {
	t.Parallel()
	out := renderRanoTable(defaultTheme(), 60, nil, time.Second)
	// Should have header but no data rows
	if !strings.Contains(out, "Agent") || !strings.Contains(out, "Conn") {
		t.Fatalf("expected table header, got: %q", out)
	}
}

func TestRenderRanoTable_ZeroWidth(t *testing.T) {
	t.Parallel()
	out := renderRanoTable(defaultTheme(), 0, nil, time.Second)
	if out != "" {
		t.Fatalf("expected empty string for zero width, got: %q", out)
	}
}

func TestRenderRanoTable_WithRows(t *testing.T) {
	rows := []RanoNetworkRow{
		{Label: "test__cc_1", AgentType: "cc", Connections: 17, LastConnection: time.Now().Add(-42 * time.Second)},
	}
	out := status.StripANSI(renderRanoTable(defaultTheme(), 70, rows, time.Second))
	if !strings.Contains(out, "test__cc_1") {
		t.Fatalf("expected agent label, got:\n%s", out)
	}
	if !strings.Contains(out, "17") || !strings.Contains(out, "42s") {
		t.Fatalf("expected connection count and age, got:\n%s", out)
	}
}

func TestRenderRanoTable_UnknownLabel(t *testing.T) {
	rows := []RanoNetworkRow{{Label: "", AgentType: "cc", Connections: 1}}
	out := status.StripANSI(renderRanoTable(defaultTheme(), 70, rows, time.Second))
	if !strings.Contains(out, "(unknown)") {
		t.Fatalf("expected (unknown) for empty label, got:\n%s", out)
	}
}

// The breakdown uses rano's recorded provider tag per connection, not a guess
// from the pane's agent type: a Claude pane's openai connection counts as openai.
func TestRenderRanoProviderBreakdownUsesRecordedTags(t *testing.T) {
	rows := []RanoNetworkRow{
		{AgentType: "cc", Providers: map[string]int{"anthropic": 5, "openai": 1}},
		{AgentType: "cod", Providers: map[string]int{"openai": 3, "zeta": 1}},
		{AgentType: "gmi", Providers: map[string]int{"google": 2, "unknown": 4}},
	}
	out := status.StripANSI(renderRanoProviderBreakdown(defaultTheme(), 120, rows))
	want := "By provider: anthropic: 5  openai: 4  google: 2  unknown: 4  zeta: 1"
	if strings.TrimSpace(out) != want {
		t.Fatalf("breakdown = %q, want %q", strings.TrimSpace(out), want)
	}
}

func TestRenderRanoProviderBreakdown_EmptyRows(t *testing.T) {
	out := renderRanoProviderBreakdown(defaultTheme(), 80, nil)
	if out != "" {
		t.Fatalf("expected empty breakdown for no rows, got: %q", out)
	}
}

func TestRenderRanoProviderBreakdown_AllZero(t *testing.T) {
	rows := []RanoNetworkRow{{AgentType: "cc", Providers: map[string]int{"anthropic": 0}}}
	out := renderRanoProviderBreakdown(defaultTheme(), 80, rows)
	if out != "" {
		t.Fatalf("expected empty breakdown for zero-connection rows, got: %q", out)
	}
}

func TestTruncateWidth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		width int
		check func(string) bool
	}{
		{"empty", "", 10, func(s string) bool { return s == "" }},
		{"fits", "hello", 10, func(s string) bool { return s == "hello" }},
		{"zero width", "hello", 0, func(s string) bool { return s == "" }},
		{"narrow", "hello", 3, func(s string) bool { return len(s) <= 3 }},
		{"exact", "hello", 5, func(s string) bool { return s == "hello" }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := truncateWidth(tc.input, tc.width)
			if !tc.check(got) {
				t.Errorf("truncateWidth(%q, %d) = %q", tc.input, tc.width, got)
			}
		})
	}
}

// =============================================================================
// Data flow integration tests (simulating injected adapter data)
// =============================================================================

func TestRanoNetworkPanelDataFlow_MultiAgent(t *testing.T) {
	// Simulate what fetchRanoNetworkStats produces from a real export:
	// two Claude agents and one Codex agent, aggregated by pane.
	panel := NewRanoNetworkPanel()
	panel.SetSize(100, 20)

	panel.SetData(RanoNetworkPanelData{
		Loaded:       true,
		Enabled:      true,
		Available:    true,
		Version:      "0.2.1",
		PollInterval: 1 * time.Second,
		Window:       5 * time.Minute,
		Rows: []RanoNetworkRow{
			{Label: "swarm__cc_1", AgentType: "cc", Connections: 15, LastConnection: time.Now().Add(-200 * time.Millisecond), Providers: map[string]int{"anthropic": 15}},
			{Label: "swarm__cc_2", AgentType: "cc", Connections: 10, LastConnection: time.Now().Add(-2 * time.Second), Providers: map[string]int{"anthropic": 10}},
			{Label: "swarm__cod_1", AgentType: "cod", Connections: 7, LastConnection: time.Now().Add(-30 * time.Second), Providers: map[string]int{"openai": 7}},
		},
		TotalConnections: 32,
	})
	out := status.StripANSI(panel.View())

	for _, want := range []string{"swarm__cc_1", "swarm__cc_2", "swarm__cod_1", "anthropic: 25", "openai: 7", "Total: 32 conn"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output, got:\n%s", want, out)
		}
	}
	// Most recent connection is listed first.
	if strings.Index(out, "swarm__cc_1") > strings.Index(out, "swarm__cod_1") {
		t.Errorf("rows not ordered by recency:\n%s", out)
	}
}

// defaultTheme returns the current theme for test use.
func defaultTheme() theme.Theme {
	return theme.Current()
}
