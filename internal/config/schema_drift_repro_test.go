package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRateLimitAutoRotateAlias — repro from ntm#113 Gap 3. The TOML key
// `[resilience.rate_limit] auto_rotate = true` must (a) parse cleanly through
// the strict loader instead of being rejected as `unknown field`, and (b)
// flip BOTH `Rotation.Enabled` and `Rotation.AutoTrigger` so the runtime
// monitor in internal/resilience/monitor.go (which gates on
// `rotateConfig.Enabled && rotateConfig.AutoTrigger`) actually acts on it.
// Flipping only AutoTrigger would silently no-op because Rotation.Enabled
// defaults to false.
func TestRateLimitAutoRotateAlias(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(`projects_base = "/tmp"

[resilience.rate_limit]
detect = true
notify = true
auto_rotate = true
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() rejected resilience.rate_limit.auto_rotate: %v", err)
	}

	if !cfg.Resilience.RateLimit.AutoRotate {
		t.Errorf("RateLimit.AutoRotate = false, want true (TOML set it true)")
	}
	if !cfg.Rotation.AutoTrigger {
		t.Errorf("Rotation.AutoTrigger = false, want true (alias should fold into canonical knob)")
	}
	if !cfg.Rotation.Enabled {
		t.Errorf("Rotation.Enabled = false, want true (alias must enable rotation or AutoTrigger silently no-ops)")
	}
}

// TestRateLimitAutoRotateAliasIsAdditive — the alias must NOT clobber an
// explicit `[rotation] auto_trigger = true` to false when the alias is unset.
// If the user has only `[rotation]` configured, the loader must leave it
// alone (and not flip Rotation.Enabled either, since the alias is the
// trigger for the dual-flip).
func TestRateLimitAutoRotateAliasIsAdditive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(`projects_base = "/tmp"

[rotation]
auto_trigger = true
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Resilience.RateLimit.AutoRotate {
		t.Errorf("RateLimit.AutoRotate = true unexpectedly; alias should default false")
	}
	if !cfg.Rotation.AutoTrigger {
		t.Errorf("Rotation.AutoTrigger = false, want true (TOML set it directly)")
	}
	// Rotation.Enabled was not set in TOML and the alias is unset, so it
	// must keep its default (false). The dual-flip path must only fire on
	// the alias.
	if cfg.Rotation.Enabled {
		t.Errorf("Rotation.Enabled = true unexpectedly; default is false and the alias is unset, so dual-flip must not fire")
	}
}

// TestContextRotationRecoveryNestedKey — repro from ntm#113 Gap 1. The TOML
// keys under `[context_rotation.recovery]` were rejected by the strict loader
// (`unknown field(s): context_rotation.recovery, ...`) even though the
// runtime status.RecoveryConfig has a 1:1 mapping for every value. After
// adding the `Recovery CompactionRecoveryConfig` field with TOML tag
// `recovery`, all five keys must round-trip without error.
func TestContextRotationRecoveryNestedKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(`projects_base = "/tmp"

[context_rotation]
rotate_threshold = 0.95

[context_rotation.recovery]
enabled = true
cooldown_seconds = 30
include_bead_context = true
max_recoveries_per_pane = 5
prompt = "Reread AGENTS.md so it's still fresh in your mind. Use ultrathink."
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() rejected [context_rotation.recovery]: %v", err)
	}

	rec := cfg.ContextRotation.Recovery
	if !rec.Enabled {
		t.Errorf("Recovery.Enabled = false, want true (TOML set it true)")
	}
	if rec.CooldownSeconds != 30 {
		t.Errorf("Recovery.CooldownSeconds = %d, want 30", rec.CooldownSeconds)
	}
	if !rec.IncludeBeadContext {
		t.Errorf("Recovery.IncludeBeadContext = false, want true")
	}
	if rec.MaxRecoveriesPerPane != 5 {
		t.Errorf("Recovery.MaxRecoveriesPerPane = %d, want 5", rec.MaxRecoveriesPerPane)
	}
	if rec.Prompt == "" {
		t.Errorf("Recovery.Prompt is empty; expected the override prompt to round-trip")
	}
}

// TestContextRotationRecoveryDefaultsAreUseful — when only `[context_rotation]`
// is configured (no `[context_rotation.recovery]` block), the nested defaults
// must still be sensible: enabled, IncludeBeadContext on, numeric "use the
// engine's default" sentinels (zero) for cooldown / max recoveries / prompt.
func TestContextRotationRecoveryDefaultsAreUseful(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(`projects_base = "/tmp"

[context_rotation]
rotate_threshold = 0.95
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	rec := cfg.ContextRotation.Recovery
	if !rec.Enabled {
		t.Errorf("Recovery.Enabled = false, want true by default")
	}
	if !rec.IncludeBeadContext {
		t.Errorf("Recovery.IncludeBeadContext = false, want true by default")
	}
	if rec.CooldownSeconds != 0 || rec.MaxRecoveriesPerPane != 0 || rec.Prompt != "" {
		t.Errorf("Default Recovery has unexpected non-zero overrides: %+v", rec)
	}
}
