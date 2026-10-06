package robot

import (
	"encoding/json"
	"testing"
)

// ---------------------------------------------------------------------------
// firstNonEmpty — 75% → 100%
// ---------------------------------------------------------------------------

func TestFirstNonEmpty(t *testing.T) {

	tests := []struct {
		name   string
		values []string
		want   string
	}{
		{"all empty", []string{}, ""},
		{"first non-empty", []string{"", "hello", "world"}, "hello"},
		{"first is non-empty", []string{"hello", "world"}, "hello"},
		{"all whitespace", []string{"", "  ", "\t"}, ""},
		{"whitespace then value", []string{"  ", "hello"}, "hello"},
		{"single value", []string{"hello"}, "hello"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := firstNonEmpty(tt.values...)
			if got != tt.want {
				t.Errorf("firstNonEmpty(%v) = %q, want %q", tt.values, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// countJSONArray — 66.7% → 100%
// ---------------------------------------------------------------------------

func TestCountJSONArray(t *testing.T) {

	tests := []struct {
		name string
		raw  json.RawMessage
		want int
	}{
		{"empty", json.RawMessage{}, 0},
		{"null", json.RawMessage("null"), 0},
		{"empty array", json.RawMessage("[]"), 0},
		{"one item", json.RawMessage(`["a"]`), 1},
		{"three items", json.RawMessage(`["a","b","c"]`), 3},
		{"invalid json", json.RawMessage("not-json"), 0},
		{"object not array", json.RawMessage(`{"a":1}`), 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := countJSONArray(tt.raw)
			if got != tt.want {
				t.Errorf("countJSONArray(%s) = %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// agentTypeFromProgram — 44.4% → 100%
// ---------------------------------------------------------------------------

func TestAgentTypeFromProgram(t *testing.T) {

	tests := []struct {
		name    string
		program string
		want    string
	}{
		{"claude", "claude-code", "cc"},
		{"claude uppercase", "Claude Code", "cc"},
		{"codex", "codex-cli", "cod"},
		{"gemini", "gemini-pro", "gmi"},
		{"cursor", "cursor-ai", "cursor"},
		{"windsurf", "Windsurf IDE", "windsurf"},
		{"aider", "aider-chat", "aider"},
		{"unknown", "unknown-agent", "unknown-agent"},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := agentTypeFromProgram(tt.program)
			if got != tt.want {
				t.Errorf("agentTypeFromProgram(%q) = %q, want %q", tt.program, got, tt.want)
			}
		})
	}
}
