package robot

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// --robot-metrics reported token_usage and every numeric in agent_stats as 0 on
// a session with twelve agents that had been working for hours, because nothing
// populated them. A machine-readable contract that says "0 prompts, 0 tokens,
// $0" is read by other agents as an idle session, so the payload names every
// field no source measured for the call. Most fields now have real sources; a
// field leaves the list only while its source is live.

var fullMetricsCoverage = metricsCoverage{
	promptHistory:  true,
	lifecycle:      true,
	transcripts:    true,
	sessionCreated: true,
}

func containsField(fields []string, want string) bool {
	for _, f := range fields {
		if f == want {
			return true
		}
	}
	return false
}

// With no source live, every source-dependent field is declared.
func TestUnmeasuredFieldsAreDeclared(t *testing.T) {
	fields := unmeasuredMetricsFields(metricsCoverage{})
	for _, want := range []string{
		"token_usage.total_tokens",
		"token_usage.total_cost_usd",
		"token_usage.by_agent",
		"token_usage.by_model",
		"token_usage.context_current_percent",
		"agent_stats[].prompts_received",
		"agent_stats[].avg_response_time_sec",
		"agent_stats[].error_count",
		"agent_stats[].restart_count",
		"session_stats.total_prompts",
		"session_stats.session_duration",
	} {
		if !containsField(fields, want) {
			t.Errorf("%s has no live source but is not declared unmeasured", want)
		}
	}
}

// Cost and response time have no source at all: they stay declared even when
// every other source is live.
func TestSourcelessFieldsStayDeclaredUnderFullCoverage(t *testing.T) {
	got := unmeasuredMetricsFields(fullMetricsCoverage)
	sort.Strings(got)
	want := []string{"agent_stats[].avg_response_time_sec", "token_usage.total_cost_usd"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unmeasured under full coverage = %v, want exactly %v", got, want)
	}
}

// Each source covers exactly its own fields: losing one source declares those
// fields and nothing else.
func TestEachSourceDeclaresOnlyItsFields(t *testing.T) {
	cases := []struct {
		name   string
		drop   func(*metricsCoverage)
		fields []string
	}{
		{"prompt history", func(c *metricsCoverage) { c.promptHistory = false },
			[]string{"agent_stats[].prompts_received", "session_stats.total_prompts"}},
		{"session monitor", func(c *metricsCoverage) { c.lifecycle = false },
			[]string{"agent_stats[].error_count", "agent_stats[].restart_count"}},
		{"transcripts", func(c *metricsCoverage) { c.transcripts = false },
			[]string{"token_usage.by_agent", "token_usage.by_model", "token_usage.context_current_percent", "token_usage.total_tokens"}},
		{"session creation", func(c *metricsCoverage) { c.sessionCreated = false },
			[]string{"session_stats.session_duration"}},
	}
	base := unmeasuredMetricsFields(fullMetricsCoverage)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cov := fullMetricsCoverage
			tc.drop(&cov)
			var extra []string
			for _, f := range unmeasuredMetricsFields(cov) {
				if !containsField(base, f) {
					extra = append(extra, f)
				}
			}
			sort.Strings(extra)
			if !reflect.DeepEqual(extra, tc.fields) {
				t.Fatalf("dropping %s declared %v, want %v", tc.name, extra, tc.fields)
			}
		})
	}
}

// The declaration has to reach the wire, or it protects nobody.
func TestMetricsOutputSerializesUnmeasured(t *testing.T) {
	output := &MetricsOutput{
		RobotResponse: NewRobotResponse(true),
		Period:        "24h",
		Unmeasured:    unmeasuredMetricsFields(metricsCoverage{}),
		AgentStats: map[string]AgentMetrics{
			"proj__gmi_1": {Type: "gemini", Unmeasured: []string{"tokens_used"}},
		},
	}

	raw, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"unmeasured"`) {
		t.Error("unmeasured list is not serialized")
	}
	if !strings.Contains(string(raw), "token_usage.total_cost_usd") {
		t.Error("the cost field is not declared unmeasured on the wire")
	}
	if !strings.Contains(string(raw), `"unmeasured":["tokens_used"]`) {
		t.Errorf("a per-agent gap is not declared on the wire: %s", raw)
	}
}

// A field with a writer must never be listed, so this cannot rot into the
// opposite lie: claiming something is unmeasured when it is measured.
func TestMeasuredFieldsAreNotDeclaredUnmeasured(t *testing.T) {
	alwaysPopulated := []string{
		"agent_stats[].type",
		"agent_stats[].pane",
		"session_stats.total_agents",
		"session_stats.active_agents",
		"session_stats.files_changed",
	}
	for _, cov := range []metricsCoverage{{}, fullMetricsCoverage} {
		for _, f := range unmeasuredMetricsFields(cov) {
			if containsField(alwaysPopulated, f) {
				t.Errorf("%s is always populated but declared unmeasured", f)
			}
		}
	}
	for _, f := range []string{
		"agent_stats[].prompts_received",
		"agent_stats[].tokens_used",
		"agent_stats[].uptime",
		"agent_stats[].error_count",
		"agent_stats[].restart_count",
		"token_usage.total_tokens",
		"token_usage.by_agent",
		"token_usage.by_model",
		"token_usage.context_current_percent",
		"session_stats.total_prompts",
		"session_stats.session_duration",
	} {
		if containsField(unmeasuredMetricsFields(fullMetricsCoverage), f) {
			t.Errorf("%s has a live source under full coverage but is declared unmeasured", f)
		}
	}
}
