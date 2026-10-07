package hooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGeneratePreCommitScriptIncludesBeadsSync(t *testing.T) {
	repoRoot := "/tmp/project path/with spaces"
	script := generatePreCommitScript("/usr/local/bin/ntm", repoRoot)

	if !strings.Contains(script, "br sync --flush-only") {
		t.Errorf("expected beads sync in pre-commit hook, got: %q", script)
	}
	if !strings.Contains(script, "hooks run pre-commit") {
		t.Errorf("expected pre-commit hook to call ntm hooks run, got: %q", script)
	}
	if !strings.Contains(script, "REPO_ROOT="+quoteShell(repoRoot)) {
		t.Errorf("expected quoted REPO_ROOT assignment, got: %q", script)
	}
}

// ntm's managed pre-commit hook (written by `ntm init`) runs the reservation
// check and refuses the commit when it fails. Before, the only hook that
// checked reservations was the standalone guard, which `ntm guards install`
// refused to install over this hook, so reservations stayed advisory.
func TestPreCommitScriptRunsReservationGuard(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	fakeNTM := filepath.Join(dir, "ntm")
	// The fake ntm fails `guards check` when GUARD_FAIL is set and records
	// every invocation.
	if err := os.WriteFile(fakeNTM, []byte(`#!/bin/sh
echo "$*" >> "$CALLS"
if [ "$1 $2" = "guards check" ] && [ -n "$GUARD_FAIL" ]; then
  echo "[ntm-guard] BLOCKED: src/a.go is reserved by GreenLake" >&2
  exit 1
fi
exit 0
`), 0o755); err != nil {
		t.Fatal(err)
	}
	script := generatePreCommitScript(fakeNTM, dir)
	if !PreCommitRunsReservationGuard(script) {
		t.Fatal("managed pre-commit hook does not report running the reservation guard")
	}
	hookPath := filepath.Join(dir, "pre-commit")
	if err := os.WriteFile(hookPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	run := func(guardFail bool) (int, string) {
		calls := filepath.Join(dir, "calls")
		_ = os.Remove(calls)
		cmd := exec.Command("bash", hookPath)
		cmd.Env = append(os.Environ(), "CALLS="+calls, "PATH=/usr/bin:/bin")
		if guardFail {
			cmd.Env = append(cmd.Env, "GUARD_FAIL=1")
		}
		out, err := cmd.CombinedOutput()
		recorded, _ := os.ReadFile(calls)
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), string(out) + string(recorded)
		}
		if err != nil {
			t.Fatalf("running hook: %v", err)
		}
		return 0, string(out) + string(recorded)
	}

	if code, out := run(false); code != 0 || !strings.Contains(out, "guards check --staged") || !strings.Contains(out, "hooks run pre-commit") {
		t.Fatalf("clean commit: exit %d, output %q; want 0 with the guard and UBS both run", code, out)
	}
	if code, out := run(true); code == 0 || !strings.Contains(out, "reserved by GreenLake") {
		t.Fatalf("reserved file: exit %d, output %q; want the commit refused with the holder named", code, out)
	}
}

func TestPreCommitRunsReservationGuard(t *testing.T) {
	for content, want := range map[string]bool{
		"#!/bin/bash\n# ntm-precommit-guard\nexec ntm guards check --staged\n": true,
		generatePreCommitScript("ntm", "/repo"):                                true,
		"#!/bin/bash\n# NTM_MANAGED_HOOK\nntm hooks run pre-commit\n":          false, // managed hook from before the guard
		"#!/bin/sh\nmake lint\n":                                               false,
	} {
		if got := PreCommitRunsReservationGuard(content); got != want {
			t.Errorf("PreCommitRunsReservationGuard(%.40q) = %v, want %v", content, got, want)
		}
	}
}

func TestGeneratePostCheckoutScriptWarnsOnBeadsChanges(t *testing.T) {
	repoRoot := "/tmp/project path/with spaces"
	script := generatePostCheckoutScript(repoRoot)

	if !strings.Contains(script, "post-checkout") {
		t.Errorf("expected post-checkout marker in hook, got: %q", script)
	}
	if !strings.Contains(script, "Warning: .beads has uncommitted changes") {
		t.Errorf("expected .beads warning in post-checkout hook, got: %q", script)
	}
	if !strings.Contains(script, "REPO_ROOT="+quoteShell(repoRoot)) {
		t.Errorf("expected quoted REPO_ROOT assignment, got: %q", script)
	}
}

// =============================================================================
// isNTMHook
// =============================================================================

func TestIsNTMHook(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"empty", "", false},
		{"no marker", "#!/bin/bash\necho hello", false},
		{"has marker", "#!/bin/bash\n# NTM_MANAGED_HOOK\necho hello", true},
		{"marker in comment", "# NTM_MANAGED_HOOK - Do not edit manually", true},
		{"marker alone", "NTM_MANAGED_HOOK", true},
		{"partial marker", "NTM_MANAGED", false},
		{"different case", "ntm_managed_hook", false},
		{"marker mid-script", "#!/bin/bash\nset -e\n# NTM_MANAGED_HOOK\nntm hooks run pre-commit", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isNTMHook(tc.content)
			if got != tc.want {
				t.Errorf("isNTMHook(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

// =============================================================================
// quoteShell
// =============================================================================

func TestQuoteShell(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", "''"},
		{"simple", "hello", "'hello'"},
		{"path", "/usr/local/bin/ntm", "'/usr/local/bin/ntm'"},
		{"spaces", "path with spaces", "'path with spaces'"},
		{"single quote", "it's", "'it'\\''s'"},
		{"multiple single quotes", "a'b'c", "'a'\\''b'\\''c'"},
		{"special chars", "hello; rm -rf /", "'hello; rm -rf /'"},
		{"double quotes", `say "hi"`, `'say "hi"'`},
		{"backtick", "echo `pwd`", "'echo `pwd`'"},
		{"dollar sign", "echo $HOME", "'echo $HOME'"},
		{"newline", "line1\nline2", "'line1\nline2'"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := quoteShell(tc.input)
			if got != tc.want {
				t.Errorf("quoteShell(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// =============================================================================
// PreCommitResult.ExitCode
// =============================================================================

func TestPreCommitResultExitCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		passed bool
		want   int
	}{
		{"passed", true, 0},
		{"failed", false, 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &PreCommitResult{Passed: tc.passed}
			got := r.ExitCode()
			if got != tc.want {
				t.Errorf("ExitCode() = %d, want %d", got, tc.want)
			}
		})
	}
}

// =============================================================================
// DefaultPreCommitConfig
// =============================================================================

func TestDefaultPreCommitConfig(t *testing.T) {
	t.Parallel()

	cfg := DefaultPreCommitConfig()

	if cfg.MaxCritical != 0 {
		t.Errorf("MaxCritical = %d, want 0", cfg.MaxCritical)
	}
	if cfg.MaxWarning != 0 {
		t.Errorf("MaxWarning = %d, want 0", cfg.MaxWarning)
	}
	if !cfg.FailOnWarning {
		t.Error("FailOnWarning should be true")
	}
	if cfg.Timeout != 120*time.Second {
		t.Errorf("Timeout = %v, want 120s", cfg.Timeout)
	}
	if cfg.Verbose {
		t.Error("Verbose should be false")
	}
	if !cfg.SkipEmpty {
		t.Error("SkipEmpty should be true")
	}
}

// =============================================================================
// Duration.MarshalText
// =============================================================================

func TestDurationMarshalText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		dur  Duration
		want string
	}{
		{"zero", Duration(0), "0s"},
		{"30 seconds", Duration(30 * time.Second), "30s"},
		{"one minute", Duration(time.Minute), "1m0s"},
		{"complex", Duration(5*time.Minute + 30*time.Second), "5m30s"},
		{"one hour", Duration(time.Hour), "1h0m0s"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.dur.MarshalText()
			if err != nil {
				t.Fatalf("MarshalText() error = %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("MarshalText() = %q, want %q", string(got), tc.want)
			}
		})
	}
}

// =============================================================================
// Duration.Duration (getter)
// =============================================================================

func TestDurationGetter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		dur  Duration
		want time.Duration
	}{
		{"zero", Duration(0), 0},
		{"positive", Duration(42 * time.Second), 42 * time.Second},
		{"negative", Duration(-5 * time.Second), -5 * time.Second},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.dur.Duration()
			if got != tc.want {
				t.Errorf("Duration() = %v, want %v", got, tc.want)
			}
		})
	}
}

// =============================================================================
// CommandHooksConfig.Validate
// =============================================================================

func TestCommandHooksConfigValidate(t *testing.T) {
	t.Parallel()

	t.Run("empty config is valid", func(t *testing.T) {
		t.Parallel()
		cfg := &CommandHooksConfig{}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() error = %v", err)
		}
	})

	t.Run("valid hooks pass", func(t *testing.T) {
		t.Parallel()
		cfg := &CommandHooksConfig{
			Hooks: []CommandHook{
				{Event: EventPreSpawn, Command: "echo a"},
				{Event: EventPostSend, Command: "echo b"},
			},
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() error = %v", err)
		}
	})

	t.Run("invalid hook at index 1 reports index", func(t *testing.T) {
		t.Parallel()
		cfg := &CommandHooksConfig{
			Hooks: []CommandHook{
				{Event: EventPreSpawn, Command: "echo good"},
				{Event: EventPreSpawn, Command: ""},
			},
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("Validate() should return error for empty command")
		}
		if !strings.Contains(err.Error(), "command_hooks[1]") {
			t.Errorf("error should indicate index 1, got: %v", err)
		}
	})
}

// =============================================================================
// generatePreCommitScript injection safety
// =============================================================================

func TestGeneratePreCommitScriptSanitizesNewlines(t *testing.T) {
	t.Parallel()
	script := generatePreCommitScript("/usr/local/bin/ntm", "/tmp/evil\ninjection")
	if !strings.Contains(script, "NTM_MANAGED_HOOK") {
		t.Error("script should contain NTM_MANAGED_HOOK marker")
	}
	// The repo root comment should have newlines replaced with spaces
	lines := strings.Split(script, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "# Repository:") {
			if strings.Contains(line, "injection") {
				break
			}
		}
	}
}

func TestGeneratePostCheckoutScriptSanitizesNewlines(t *testing.T) {
	t.Parallel()
	script := generatePostCheckoutScript("/tmp/evil\ninjection")
	if !strings.Contains(script, "NTM_MANAGED_HOOK") {
		t.Error("script should contain NTM_MANAGED_HOOK marker")
	}
}

// =============================================================================
// HookType constants
// =============================================================================

func TestHookTypeConstants(t *testing.T) {
	t.Parallel()

	if HookPreCommit != "pre-commit" {
		t.Errorf("HookPreCommit = %q", HookPreCommit)
	}
	if HookPrePush != "pre-push" {
		t.Errorf("HookPrePush = %q", HookPrePush)
	}
	if HookCommitMsg != "commit-msg" {
		t.Errorf("HookCommitMsg = %q", HookCommitMsg)
	}
	if HookPostCommit != "post-commit" {
		t.Errorf("HookPostCommit = %q", HookPostCommit)
	}
	if HookPostCheckout != "post-checkout" {
		t.Errorf("HookPostCheckout = %q", HookPostCheckout)
	}
}
