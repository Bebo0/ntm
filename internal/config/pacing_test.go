package config

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// Spawn pacing narrowed to the admission-control surface in v1.28.0
// (bd-6otuk): only enabled, max_concurrent_spawns, and the per-agent
// *_max_concurrent caps remain — the rest of the historical pacing surface
// (rate limiter, backoff, headroom) never had a runtime consumer and now
// takes the deprecated-knob warn path (see removed_knobs_test.go).

func TestDefaultSpawnPacingConfig(t *testing.T) {
	cfg := DefaultSpawnPacingConfig()

	if !cfg.Enabled {
		t.Error("Enabled should default to true")
	}
	if cfg.MaxConcurrentSpawns != 4 {
		t.Errorf("MaxConcurrentSpawns = %d, want 4", cfg.MaxConcurrentSpawns)
	}
	if cfg.AgentCaps.ClaudeMaxConcurrent != 3 {
		t.Errorf("ClaudeMaxConcurrent = %d, want 3", cfg.AgentCaps.ClaudeMaxConcurrent)
	}
	if cfg.AgentCaps.CodexMaxConcurrent != 2 {
		t.Errorf("CodexMaxConcurrent = %d, want 2", cfg.AgentCaps.CodexMaxConcurrent)
	}
	if cfg.AgentCaps.GeminiMaxConcurrent != 2 {
		t.Errorf("GeminiMaxConcurrent = %d, want 2", cfg.AgentCaps.GeminiMaxConcurrent)
	}

	if err := ValidateSpawnPacingConfig(&cfg); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
}

func TestValidateSpawnPacingConfig(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*SpawnPacingConfig)
		wantErr string
	}{
		{"disabled skips validation", func(c *SpawnPacingConfig) { c.Enabled = false; c.MaxConcurrentSpawns = 0 }, ""},
		{"max_concurrent_spawns zero", func(c *SpawnPacingConfig) { c.MaxConcurrentSpawns = 0 }, "max_concurrent_spawns"},
		{"claude cap negative", func(c *SpawnPacingConfig) { c.AgentCaps.ClaudeMaxConcurrent = -1 }, "claude_max_concurrent"},
		{"codex cap negative", func(c *SpawnPacingConfig) { c.AgentCaps.CodexMaxConcurrent = -1 }, "codex_max_concurrent"},
		{"gemini cap negative", func(c *SpawnPacingConfig) { c.AgentCaps.GeminiMaxConcurrent = -1 }, "gemini_max_concurrent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultSpawnPacingConfig()
			tt.mutate(&cfg)
			err := ValidateSpawnPacingConfig(&cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want mention of %q", err, tt.wantErr)
			}
		})
	}
}

func TestSpawnPacingFromTOML(t *testing.T) {
	path := createTempConfig(t, `
[spawn_pacing]
enabled = true
max_concurrent_spawns = 8

[spawn_pacing.agent_caps]
claude_max_concurrent = 5
codex_max_concurrent = 4
gemini_max_concurrent = 3
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SpawnPacing.MaxConcurrentSpawns != 8 {
		t.Errorf("MaxConcurrentSpawns = %d, want 8", cfg.SpawnPacing.MaxConcurrentSpawns)
	}
	if cfg.SpawnPacing.AgentCaps.ClaudeMaxConcurrent != 5 {
		t.Errorf("ClaudeMaxConcurrent = %d, want 5", cfg.SpawnPacing.AgentCaps.ClaudeMaxConcurrent)
	}
	if cfg.SpawnPacing.AgentCaps.CodexMaxConcurrent != 4 {
		t.Errorf("CodexMaxConcurrent = %d, want 4", cfg.SpawnPacing.AgentCaps.CodexMaxConcurrent)
	}
	if cfg.SpawnPacing.AgentCaps.GeminiMaxConcurrent != 3 {
		t.Errorf("GeminiMaxConcurrent = %d, want 3", cfg.SpawnPacing.AgentCaps.GeminiMaxConcurrent)
	}
}

// TestSpawnConfigDefaultsAndTOML pins the [spawn] table: unpaced by default
// with a 30s fixed-mode interval, and duration strings decode from TOML.
func TestSpawnConfigDefaultsAndTOML(t *testing.T) {
	defaults := Default().Spawn
	if defaults.StaggerMode != SpawnStaggerNone || defaults.StaggerDelay != 30*time.Second {
		t.Fatalf("default [spawn] = %+v, want none/30s", defaults)
	}
	if err := ValidateSpawnConfig(&defaults); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}

	cfg, err := Load(createTempConfig(t, "[spawn]\nstagger_mode = \"smart\"\nstagger_delay = \"45s\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Spawn.StaggerMode != SpawnStaggerSmart || cfg.Spawn.StaggerDelay != 45*time.Second {
		t.Fatalf("loaded [spawn] = %+v, want smart/45s", cfg.Spawn)
	}
	if got, err := GetValue(cfg, "spawn.stagger_delay"); err != nil || got != 45*time.Second {
		t.Fatalf("GetValue(spawn.stagger_delay) = %v, %v", got, err)
	}
	if !hasConfigDiffPath(Diff(cfg), "spawn.stagger_mode") || !hasConfigDiffPath(Diff(cfg), "spawn.stagger_delay") {
		t.Fatalf("Diff() omits changed [spawn] keys: %v", Diff(cfg))
	}

	var buf bytes.Buffer
	if err := Print(cfg, &buf); err != nil {
		t.Fatalf("Print: %v", err)
	}
	for _, want := range []string{"[spawn]", `stagger_mode = "smart"`, `stagger_delay = "45s"`} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("Print output missing %q", want)
		}
	}
	// The printed table must load back to the same values.
	reloaded, err := Load(createTempConfig(t, "[spawn]\nstagger_mode = \"smart\"\nstagger_delay = \""+cfg.Spawn.StaggerDelay.String()+"\"\n"))
	if err != nil || reloaded.Spawn != cfg.Spawn {
		t.Fatalf("round trip = %+v, %v; want %+v", reloaded.Spawn, err, cfg.Spawn)
	}
}

func TestValidateSpawnConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     SpawnConfig
		wantErr string
	}{
		{name: "empty mode means none", cfg: SpawnConfig{StaggerDelay: time.Second}},
		{name: "fixed at maximum", cfg: SpawnConfig{StaggerMode: SpawnStaggerFixed, StaggerDelay: MaxSpawnStaggerDelay}},
		{name: "zero delay", cfg: SpawnConfig{StaggerMode: SpawnStaggerFixed}},
		{name: "unsupported mode", cfg: SpawnConfig{StaggerMode: "adaptive"}, wantErr: `stagger_mode must be one of none, fixed, or smart; got "adaptive"`},
		{name: "case sensitive mode", cfg: SpawnConfig{StaggerMode: "Fixed"}, wantErr: "stagger_mode must be one of none, fixed, or smart"},
		{name: "negative delay", cfg: SpawnConfig{StaggerDelay: -time.Second}, wantErr: "stagger_delay must be between 0 and 5m0s"},
		{name: "delay above maximum", cfg: SpawnConfig{StaggerDelay: MaxSpawnStaggerDelay + time.Nanosecond}, wantErr: "stagger_delay must be between 0 and 5m0s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSpawnConfig(&tc.cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.HasPrefix(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}

	cfg := Default()
	cfg.Spawn.StaggerMode = "bogus"
	found := false
	for _, err := range Validate(cfg) {
		if strings.HasPrefix(err.Error(), "spawn: stagger_mode must be one of") {
			found = true
		}
	}
	if !found {
		t.Fatalf("Validate() does not report the invalid [spawn] table: %v", Validate(cfg))
	}
}
