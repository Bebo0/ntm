package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestSpawnPacingAgentTypeLimitsValidate(t *testing.T) {
	for _, kind := range []string{"claude", "codex", "gemini", "antigravity", "grok", "omp", "opencode"} {
		for _, limit := range []int{0, 1, int(^uint(0) >> 1)} {
			cfg := DefaultSpawnPacingConfig()
			cfg.AgentTypeLimits = map[string]int{kind: limit}
			if err := ValidateSpawnPacingConfig(&cfg); err != nil {
				t.Fatalf("%s=%d: %v", kind, limit, err)
			}
		}
	}
	for _, kind := range []string{"", "cc", "Claude", "cod", "opencode ", "typo", "claude\n"} {
		cfg := DefaultSpawnPacingConfig()
		cfg.AgentTypeLimits = map[string]int{kind: 2}
		if err := ValidateSpawnPacingConfig(&cfg); err == nil || !strings.Contains(err.Error(), "agent_type_limits") {
			t.Fatalf("unknown type %q accepted: %v", kind, err)
		}
	}
	cfg := DefaultSpawnPacingConfig()
	cfg.AgentTypeLimits = map[string]int{"claude": -1}
	if err := ValidateSpawnPacingConfig(&cfg); err == nil {
		t.Fatal("negative limit accepted")
	}
	cfg.Enabled = false
	if err := ValidateSpawnPacingConfig(&cfg); err != nil {
		t.Fatal("disabled validation behavior changed")
	}
}

func TestSpawnPacingAgentTypeLimitsAreOptIn(t *testing.T) {
	cfg := DefaultSpawnPacingConfig()
	if len(cfg.AgentTypeLimits) != 0 || cfg.AgentCaps != (AgentPacingConfig{ClaudeMaxConcurrent: 3, CodexMaxConcurrent: 2, GeminiMaxConcurrent: 2, OmpMaxConcurrent: 8}) {
		t.Fatal("new limits changed the existing default fleet budget")
	}
	cfg.AgentTypeLimits = map[string]int{"claude": 1, "codex": 2}
	original := map[string]int{"claude": 1, "codex": 2}
	if err := ValidateSpawnPacingConfig(&cfg); err != nil || !reflect.DeepEqual(cfg.AgentTypeLimits, original) {
		t.Fatal("validation changed limits")
	}
	cfg.AgentTypeLimits = map[string]int{"zzz": 1, "aaa": 1}
	for i := 0; i < 100; i++ {
		if err := ValidateSpawnPacingConfig(&cfg); err == nil || !strings.Contains(err.Error(), `"aaa"`) {
			t.Fatal("non-deterministic invalid-key diagnostic")
		}
	}
}
