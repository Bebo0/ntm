package robot

import (
	"testing"

	"github.com/Dicklesworthstone/ntm/internal/tools"
)

// --- proxy_status.go edge cases ---

func TestBuildProxyStatusInfo_NilBoth(t *testing.T) {
	info := buildProxyStatusInfo(nil, nil)
	if info.DaemonRunning {
		t.Error("DaemonRunning = true, want false when both nil")
	}
	if info.Version != "" {
		t.Errorf("Version = %q, want empty", info.Version)
	}
}

func TestBuildProxyStatusInfo_StatusOverridesAvailability(t *testing.T) {
	status := &tools.ProxyStatus{
		Running:       true,
		Version:       "2.0.0",
		ListenPort:    9090,
		UptimeSeconds: 7200,
	}
	avail := &tools.ProxyAvailability{
		Running: false,
		Version: tools.Version{Raw: "1.0.0"},
	}
	info := buildProxyStatusInfo(status, avail)
	if !info.DaemonRunning {
		t.Error("DaemonRunning should come from status, not availability")
	}
	if info.Version != "2.0.0" {
		t.Errorf("Version = %q, should be overridden by status", info.Version)
	}
	if info.ListenPort != 9090 {
		t.Errorf("ListenPort = %d, want 9090", info.ListenPort)
	}
}

func TestBuildProxyStatusInfo_EmptyStatusVersion(t *testing.T) {
	// When status.Version is empty, availability version should persist
	status := &tools.ProxyStatus{Running: true, Version: ""}
	avail := &tools.ProxyAvailability{Version: tools.Version{Raw: "0.5.0"}}
	info := buildProxyStatusInfo(status, avail)
	if info.Version != "0.5.0" {
		t.Errorf("Version = %q, want 0.5.0 from availability fallback", info.Version)
	}
}

func TestBuildProxyRouteInfos_NilStatus(t *testing.T) {
	routes := buildProxyRouteInfos(nil)
	if routes != nil {
		t.Errorf("expected nil routes for nil status, got %v", routes)
	}
}

func TestBuildProxyRouteInfos_EmptyRouteStats(t *testing.T) {
	status := &tools.ProxyStatus{
		RouteStats: []tools.ProxyRouteStatus{},
		Routes:     0,
	}
	routes := buildProxyRouteInfos(status)
	if routes != nil {
		t.Errorf("expected nil for zero Routes and empty RouteStats, got len=%d", len(routes))
	}
}

func TestBuildProxyRouteInfos_MultipleRoutes(t *testing.T) {
	status := &tools.ProxyStatus{
		RouteStats: []tools.ProxyRouteStatus{
			{Domain: "api.openai.com", Active: true, Requests: 100},
			{Domain: "api.anthropic.com", Active: true, Requests: 200},
			{Domain: "generativelanguage.googleapis.com", Active: false, Errors: 5},
		},
	}
	routes := buildProxyRouteInfos(status)
	if len(routes) != 3 {
		t.Fatalf("len(routes) = %d, want 3", len(routes))
	}
	if routes[0].Domain != "api.openai.com" || routes[0].Requests != 100 {
		t.Errorf("route[0] = %+v, unexpected", routes[0])
	}
	if routes[2].Active {
		t.Error("route[2] should not be active")
	}
	if routes[2].Errors != 5 {
		t.Errorf("route[2].Errors = %d, want 5", routes[2].Errors)
	}
}

func TestBuildProxyRouteInfos_RouteCountOnly(t *testing.T) {
	status := &tools.ProxyStatus{Routes: 5}
	routes := buildProxyRouteInfos(status)
	if len(routes) != 5 {
		t.Fatalf("len(routes) = %d, want 5 placeholder routes", len(routes))
	}
	for i, r := range routes {
		if r.Domain != "" || r.Active || r.Requests != 0 {
			t.Errorf("route[%d] should be zero-value, got %+v", i, r)
		}
	}
}

func TestBuildProxyFailoverInfos_Empty(t *testing.T) {
	result := buildProxyFailoverInfos(nil)
	if result != nil {
		t.Errorf("expected nil for nil events, got %v", result)
	}
	result = buildProxyFailoverInfos([]tools.ProxyFailoverEvent{})
	if result != nil {
		t.Errorf("expected nil for empty events, got %v", result)
	}
}

func TestBuildProxyFailoverInfos_Multiple(t *testing.T) {
	events := []tools.ProxyFailoverEvent{
		{Timestamp: "t1", Domain: "d1", From: "a", To: "b", Reason: "timeout"},
		{Timestamp: "t2", Domain: "d2", From: "c", To: "d", Reason: "503"},
	}
	result := buildProxyFailoverInfos(events)
	if len(result) != 2 {
		t.Fatalf("len = %d, want 2", len(result))
	}
	if result[0].Reason != "timeout" {
		t.Errorf("result[0].Reason = %q, want timeout", result[0].Reason)
	}
	if result[1].From != "c" || result[1].To != "d" {
		t.Errorf("result[1] = %+v, unexpected", result[1])
	}
}

// --- watch_bead.go edge cases ---

func TestCompileBeadMentionPattern_SpecialChars(t *testing.T) {
	re, err := compileBeadMentionPattern("bd-123.456")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if re.MatchString("bd-123X456") {
		t.Error("dot should be literal, not regex wildcard")
	}
	if !re.MatchString("found bd-123.456 here") {
		t.Error("should match literal dot")
	}
}

func TestCompileBeadMentionPattern_Whitespace(t *testing.T) {
	_, err := compileBeadMentionPattern("   ")
	if err == nil {
		t.Error("expected error for whitespace-only bead ID")
	}
}

func TestCompileBeadMentionPattern_WithPrefix(t *testing.T) {
	re, err := compileBeadMentionPattern("ntm-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !re.MatchString("Working on NTM-ABC") {
		t.Error("should match case-insensitively")
	}
	if re.MatchString("ntm-abcdef") {
		t.Error("should not match longer IDs (word boundary)")
	}
}

func TestFindBeadMentionMatches_AllEmpty(t *testing.T) {
	re, _ := compileBeadMentionPattern("bd-1")
	matches := findBeadMentionMatches([]string{}, re)
	if len(matches) != 0 {
		t.Errorf("expected 0 matches for empty lines, got %d", len(matches))
	}
}

func TestFindBeadMentionMatches_OnlyBlankLines(t *testing.T) {
	re, _ := compileBeadMentionPattern("bd-1")
	matches := findBeadMentionMatches([]string{"", "  ", "\t"}, re)
	if len(matches) != 0 {
		t.Errorf("expected 0 matches for blank lines, got %d", len(matches))
	}
}

func TestFindBeadMentionMatches_MultipleMatchesSameLine(t *testing.T) {
	re, _ := compileBeadMentionPattern("bd-5")
	matches := findBeadMentionMatches([]string{"bd-5 and bd-5 again"}, re)
	if len(matches) != 1 {
		t.Errorf("expected 1 match per line, got %d", len(matches))
	}
}

func TestFindBeadMentionMatches_LineNumbers(t *testing.T) {
	re, _ := compileBeadMentionPattern("bd-42")
	lines := []string{
		"line one",
		"bd-42 here",
		"",
		"nothing",
		"also BD-42 y",
	}
	matches := findBeadMentionMatches(lines, re)
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matches))
	}
	if matches[0].LineNum != 2 {
		t.Errorf("first match line = %d, want 2", matches[0].LineNum)
	}
	if matches[1].LineNum != 5 {
		t.Errorf("second match line = %d, want 5", matches[1].LineNum)
	}
}

// --- robot.go: parseSwarmSessionName ---

func TestParseSwarmSessionName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantType string
		wantOK   bool
	}{
		{"cc agents", "cc_agents_proj1", "cc", true},
		{"cod agents", "cod_agents_proj2", "cod", true},
		{"gmi agents", "gmi_agents_proj3", "gmi", true},
		{"unknown prefix", "aider_agents_proj", "", false},
		{"empty", "", "", false},
		{"partial cc", "cc_", "", false},
		{"just cc_agents_", "cc_agents_", "cc", true},
		{"no underscore suffix", "cc_agents", "", false},
		{"user session", "my_project", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotType, gotOK := parseSwarmSessionName(tt.input)
			if gotType != tt.wantType || gotOK != tt.wantOK {
				t.Errorf("parseSwarmSessionName(%q) = (%q, %v), want (%q, %v)",
					tt.input, gotType, gotOK, tt.wantType, tt.wantOK)
			}
		})
	}
}

// --- rano_stats.go: normalizeRanoWindow edge cases ---

func TestNormalizeRanoWindow_ValidDurations(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"1m", "1m"},
		{"30s", "30s"},
		{"2h", "2h"},
		{"10m", "10m"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := normalizeRanoWindow(tt.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("normalizeRanoWindow(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- robot.go: formatRedactionCategoryCounts ---

func TestFormatRedactionCategoryCounts_Empty(t *testing.T) {
	got := formatRedactionCategoryCounts(nil)
	if got != "" {
		t.Errorf("expected empty for nil, got %q", got)
	}
	got = formatRedactionCategoryCounts(map[string]int{})
	if got != "" {
		t.Errorf("expected empty for empty map, got %q", got)
	}
}

func TestFormatRedactionCategoryCounts_Single(t *testing.T) {
	got := formatRedactionCategoryCounts(map[string]int{"api_key": 3})
	if got != "api_key=3" {
		t.Errorf("got %q, want api_key=3", got)
	}
}

func TestFormatRedactionCategoryCounts_SortedOutput(t *testing.T) {
	got := formatRedactionCategoryCounts(map[string]int{
		"ssh_key":  1,
		"api_key":  5,
		"password": 2,
	})
	want := "api_key=5, password=2, ssh_key=1"
	if got != want {
		t.Errorf("got %q, want %q (sorted)", got, want)
	}
}
