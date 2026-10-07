package spawnidentity

// Unit tests for the shared identity coordinator's internals. The lifecycle
// behavior is exercised through each surface: internal/cli (spawn/add/adopt/
// relaunch bindings), internal/robot (--robot-spawn) and internal/serve (REST
// spawn through the robot engine).

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/ntm/internal/agentmail"
	"github.com/Dicklesworthstone/ntm/internal/config"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

func isolateIdentityDirs(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".state"))
}

func readIdentity(t *testing.T, projectKey, paneID string) string {
	t.Helper()
	raw, err := os.ReadFile(agentmail.CanonicalIdentityPath(projectKey, paneID))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// TestConfigOptionsFailsClosed pins the registration gate every surface now
// shares (#243): no config, a disabled toggle, or auto_register off never
// authorize contacting Agent Mail, and badges ride on the same gate.
func TestConfigOptionsFailsClosed(t *testing.T) {
	if opts := ConfigOptions(nil); opts.Enabled || opts.PaneBadges || len(opts.ClientOptions) != 0 || opts.BadgeTemplate != agentmail.DefaultBadgeTemplate {
		t.Fatalf("nil config options = %+v, want disabled with the default template", opts)
	}

	on := true
	cfg := config.Default()
	cfg.AgentMail.PaneBadges = &on
	if opts := ConfigOptions(cfg); !opts.Enabled || !opts.PaneBadges || len(opts.ClientOptions) == 0 {
		t.Fatalf("default config options = %+v, want enabled with badges and client options", opts)
	}

	cfg.AgentMail.AutoRegister = false
	if opts := ConfigOptions(cfg); opts.Enabled || opts.PaneBadges {
		t.Fatalf("auto_register=false options = %+v, want registration and badges off", opts)
	}

	cfg.AgentMail.AutoRegister = true
	cfg.AgentMail.Enabled = false
	if opts := ConfigOptions(cfg); opts.Enabled || opts.PaneBadges {
		t.Fatalf("enabled=false options = %+v, want registration and badges off", opts)
	}

	cfg.AgentMail.Enabled = true
	cfg.AgentMail.PaneBadgeFormat = "<{name}>"
	if got := ConfigOptions(cfg).BadgeTemplate; got != "<{name}>" {
		t.Fatalf("badge template = %q, want the configured format", got)
	}
}

// TestDisabledCoordinatorIsInert: with registration off the coordinator never
// lists panes, never contacts Agent Mail, never writes identity files, and
// reports a nil status — the surface omits agent_mail entirely.
func TestDisabledCoordinatorIsInert(t *testing.T) {
	isolateIdentityDirs(t)
	projectKey := t.TempDir()
	listed := false
	c := New(projectKey, "inert", Options{
		PreLaunch: true,
		ListPanes: func(context.Context, string) ([]tmux.Pane, error) {
			listed = true
			return nil, nil
		},
	})
	c.PrepareAgent(context.Background(), Agent{PaneIndex: 1, PaneID: "%1", PaneTitle: "inert__cc_1", AgentType: "cc"})
	if c.Status() != nil {
		t.Fatalf("disabled status = %+v, want nil", c.Status())
	}
	if c.ReconcileBadges(context.Background()) != nil {
		t.Fatal("disabled coordinator reconciled badges")
	}
	if listed {
		t.Fatal("disabled coordinator listed tmux panes")
	}
	if got := readIdentity(t, projectKey, "%1"); got != "" {
		t.Fatalf("disabled coordinator wrote identity %q", got)
	}
}

// TestRegisterBatchWithoutContextCountsEveryAgentFailed keeps the historical
// batch contract: a missing context fails every agent without side effects.
func TestRegisterBatchWithoutContextCountsEveryAgentFailed(t *testing.T) {
	agents := []Agent{{PaneID: "%1"}, {PaneID: "%2"}}
	status := RegisterBatch(nil, t.TempDir(), "batch", agents, Options{Enabled: true})
	if status == nil || status.AgentsFailed != 2 || status.AgentsRegistered != 0 {
		t.Fatalf("status = %+v, want both agents failed", status)
	}
}

func TestLivenessFromPanes(t *testing.T) {
	isLive := LivenessFromPanes([]tmux.Pane{
		{ID: "%5", PID: 4242},
		{ID: "%6", PID: 0}, // pid unknown on the tmux side
		{ID: ""},           // malformed entry is ignored
	})

	cases := []struct {
		name        string
		paneID      string
		recordedPID int
		want        bool
	}{
		{"present, pid matches", "%5", 4242, true},
		{"present, no recorded pid", "%5", 0, true},
		{"present, recorded pid differs", "%5", 1, false},
		{"present, tmux pid unknown", "%6", 99, true},
		{"absent", "%9", 0, false},
		{"empty id", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isLive(tc.paneID, tc.recordedPID); got != tc.want {
				t.Fatalf("isLive(%q, %d) = %v, want %v", tc.paneID, tc.recordedPID, got, tc.want)
			}
		})
	}
}

func TestPublishKeys(t *testing.T) {
	if got := publishKeys("", ""); len(got) != 0 {
		t.Fatalf("empty inputs produced keys: %v", got)
	}

	// Non-existent paths cannot be symlink-resolved and must not duplicate.
	session := filepath.Join(string(os.PathSeparator), "nonexistent-ntm-257", "proj")
	if got := publishKeys(session, ""); !reflect.DeepEqual(got, []string{session}) {
		t.Fatalf("plain spawn keys = %v, want [%s]", got, session)
	}
	if got := publishKeys(session, session); !reflect.DeepEqual(got, []string{session}) {
		t.Fatalf("pane dir equal to session key must dedupe, got %v", got)
	}

	worktree := filepath.Join(session, ".ntm", "worktrees", "sess", "cc-1")
	got := publishKeys(session, worktree)
	want := []string{session, worktree}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("worktree keys = %v, want %v", got, want)
	}
	if got[0] != session {
		t.Fatalf("session key must come first, got %v", got)
	}
}

func TestPublishKeys_ResolvesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on windows")
	}
	root := t.TempDir()
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	realProj := filepath.Join(real, "proj")
	if err := os.MkdirAll(realProj, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(real, "alias")
	if err := os.Symlink(realProj, link); err != nil {
		t.Fatal(err)
	}

	got := publishKeys(link, "")
	want := []string{link, realProj}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("symlinked session keys = %v, want %v", got, want)
	}

	// Canonical input yields a single key.
	if got := publishKeys(realProj, ""); !reflect.DeepEqual(got, []string{realProj}) {
		t.Fatalf("canonical session keys = %v, want [%s]", got, realProj)
	}
}

// TestPublishIdentityPreservesServerReceipt: when registration carried a pane
// binding, the Agent Mail server has already written a structured generation
// receipt at the canonical session-key path. publishIdentity must keep it
// (not clobber it with a plain name) and mirror its exact bytes into the
// pane's worktree namespace.
func TestPublishIdentityPreservesServerReceipt(t *testing.T) {
	isolateIdentityDirs(t)

	projectKey := t.TempDir()
	paneDir := t.TempDir()
	receipt := `{"name":"BlueLake","session_name":"s","pane_id":"%7","pane_pid":4242,` +
		`"socket_path":"/tmp/tmux.sock","written_at":"2026-08-31T00:00:00Z"}`
	canonical := agentmail.CanonicalIdentityPath(projectKey, "%7")
	if err := os.MkdirAll(filepath.Dir(canonical), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canonical, []byte(receipt), 0o600); err != nil {
		t.Fatal(err)
	}

	c := New(projectKey, "receipt_session", Options{})
	c.publishIdentity(Agent{PaneIndex: 1, PaneID: "%7", PaneDir: paneDir}, "BlueLake")

	after, err := os.ReadFile(canonical)
	if err != nil || string(after) != receipt {
		t.Fatalf("canonical receipt after publish = %q err=%v, want untouched", after, err)
	}
	mirrored, err := os.ReadFile(agentmail.CanonicalIdentityPath(paneDir, "%7"))
	if err != nil || string(mirrored) != receipt {
		t.Fatalf("worktree mirror = %q err=%v, want byte-identical receipt", mirrored, err)
	}
}

// TestPublishIdentityOverwritesMismatchedReceipt: a receipt bound to a
// DIFFERENT identity is stale evidence for this pane and is replaced with the
// registered name.
func TestPublishIdentityOverwritesMismatchedReceipt(t *testing.T) {
	isolateIdentityDirs(t)

	projectKey := t.TempDir()
	canonical := agentmail.CanonicalIdentityPath(projectKey, "%7")
	if err := os.MkdirAll(filepath.Dir(canonical), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := `{"name":"OldTenant","session_name":"s","pane_id":"%7","pane_pid":1,` +
		`"socket_path":"/tmp/tmux.sock","written_at":"2026-08-01T00:00:00Z"}`
	if err := os.WriteFile(canonical, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}

	c := New(projectKey, "receipt_session", Options{})
	c.publishIdentity(Agent{PaneIndex: 1, PaneID: "%7"}, "BlueLake")

	if got := readIdentity(t, projectKey, "%7"); got != "BlueLake" {
		t.Fatalf("identity after publish = %q, want BlueLake replacing the stale receipt", got)
	}
}

func TestModelDelegationIsBuiltInDefault(t *testing.T) {
	for agentType, want := range map[string]bool{
		// Empty compiled-in default: delegation is the intended config.
		"cc":       true, // ntm#334
		"claude":   true,
		"grok":     true,
		"oc":       true,
		"opencode": true,
		"omp":      true,
		"cursor":   true, // no [models] key exists to set
		// Non-empty compiled-in default: an empty model means the user
		// blanked it, so the notice still explains the placeholder.
		"cod":    false,
		"gmi":    false,
		"ollama": false,
		// Plugin agent types keep the notice.
		"hermes": false,
		"":       false,
	} {
		if got := modelDelegationIsBuiltInDefault(agentType); got != want {
			t.Errorf("modelDelegationIsBuiltInDefault(%q) = %v, want %v", agentType, got, want)
		}
	}
}

func TestDelegatedModelPlaceholder(t *testing.T) {
	if got := delegatedModelPlaceholder("opencode"); got != "opencode/cli-default" {
		t.Fatalf("delegated model = %q", got)
	}
	if got := delegatedModelPlaceholder(""); got != "agent/cli-default" {
		t.Fatalf("empty program delegated model = %q", got)
	}
	if got := modelDefaultKeyForType("oc"); got != "opencode" {
		t.Fatalf("default key for oc = %q", got)
	}
	if got := modelDefaultKeyForType("omp"); got != "omp" {
		t.Fatalf("default key for plugin = %q", got)
	}
}
