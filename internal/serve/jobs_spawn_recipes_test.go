package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/ntm/internal/config"
	"github.com/Dicklesworthstone/ntm/internal/resilience"
	"github.com/Dicklesworthstone/ntm/internal/robot"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

func TestSwarmJobRecipePreviewThroughSharedEngine(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".ntm"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".ntm", "recipes.toml"), []byte(`[[recipes]]
name = "minimal"
[[recipes.agents]]
type = "cc"
count = 3
model = "http-project-model"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := NewHermeticServer("recipe-test")
	defer srv.Stop()
	srv.projectDir = project
	cfg := config.Default()
	cfg.SpawnPacing.Enabled = false
	srv.spawnAgents = func(ctx context.Context, opts robot.SpawnOptions) (*robot.SpawnOutput, error) {
		// Keep the production recipe/preview/admission path. Only the tmux
		// installation and fleet observation ports are replaced in this test.
		opts.LifecycleDeps = &robot.SpawnLifecycleDependencies{
			IsTMUXInstalled: func() bool { return true },
			GetAllPanes: func(context.Context) (map[string][]tmux.Pane, error) {
				return map[string][]tmux.Pane{}, nil
			},
			CreateSession: func(context.Context, string, string, int) error {
				t.Error("recipe preview created a session")
				return nil
			},
		}
		return robot.GetSpawn(ctx, opts, cfg)
	}
	env := postJob(t, srv, `{"type":"swarm_spawn","params":{"session":"recipe-http","preset":"minimal","dry_run":true}}`)
	final := pollJobTerminal(t, srv, env.Job.ID)
	if final.Job.Status != string(JobStatusCompleted) || final.Job.Result["preset_used"] != "minimal" || final.Job.Result["working_dir"] != project {
		t.Fatalf("preset-only HTTP job failed or lost the admitted project: %+v", final.Job)
	}
	agents, ok := final.Job.Result["would_create"].([]interface{})
	if !ok || len(agents) != 4 {
		t.Fatalf("HTTP preview did not expand the project's recipe: %+v", final.Job.Result)
	}
	if agents[1].(map[string]interface{})["variant"] != "http-project-model" {
		t.Fatalf("HTTP preview lost the recipe model: %+v", agents)
	}
}

func TestConfiguredSwarmJobUsesSelectedLaunchCommand(t *testing.T) {
	t.Setenv("NTM_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	selected := filepath.Join(t.TempDir(), "operator.toml")
	if err := os.WriteFile(selected, []byte("[spawn_pacing]\nenabled = false\n[agents]\nclaude = 'operator-claude --model {{shellQuote .Model}}'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := NewHermeticServer("spawn-policy-command")
	defer srv.Stop()
	srv.projectDir = t.TempDir()
	if err := srv.ConfigureSpawnPolicy(selected, true); err != nil {
		t.Fatal(err)
	}
	// Changing environment selection must not redirect this server's policy.
	t.Setenv("NTM_CONFIG", filepath.Join(t.TempDir(), "not-selected.toml"))
	commands := make(chan string, 1)
	configured := srv.spawnAgents
	srv.spawnAgents = func(ctx context.Context, opts robot.SpawnOptions) (*robot.SpawnOutput, error) {
		opts.LifecycleDeps = &robot.SpawnLifecycleDependencies{
			IsTMUXInstalled: func() bool { return true },
			GetAllPanes:     func(context.Context) (map[string][]tmux.Pane, error) { return map[string][]tmux.Pane{}, nil },
			SessionExists:   func(context.Context, string) (bool, error) { return true, nil },
			GetPanes: func(context.Context, string) ([]tmux.Pane, error) {
				return []tmux.Pane{{ID: "%8", WindowIndex: 0, Index: 0}}, nil
			},
			ApplyTiledLayout: func(context.Context, string) error { return nil },
			LaunchAgent: func(_ context.Context, _ tmux.Pane, _ string, kind string, _ int, _ string, command string) (robot.SpawnedAgent, error) {
				commands <- command
				return robot.SpawnedAgent{Pane: "0.0", Type: kind}, nil
			},
			StartSessionMonitor: func(context.Context, resilience.SpawnMonitorRequest) (*resilience.SpawnMonitorResult, error) {
				return &resilience.SpawnMonitorResult{}, nil
			},
		}
		return configured(ctx, opts)
	}
	env := postJob(t, srv, `{"type":"swarm_spawn","params":{"session":"policy-command","cc_count":1,"cc_model":"operator/model","no_user_pane":true}}`)
	final := pollJobTerminal(t, srv, env.Job.ID)
	if final.Job.Status != string(JobStatusCompleted) {
		t.Fatalf("configured launch failed: %+v", final.Job)
	}
	select {
	case command := <-commands:
		if command != "operator-claude --model 'operator/model'" {
			t.Fatalf("selected launch command ignored: %q", command)
		}
	default:
		t.Fatal("no agent was launched")
	}
}

// These tests use real strict config loading and the real served surfaces.
// Only tmux ports are replaced; no caller injects the effective configuration.
func TestConfiguredSpawnPolicyFailureThroughBothHTTPPaths(t *testing.T) {
	t.Setenv("NTM_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, kind := range []string{"missing-global", "invalid-project"} {
		t.Run(kind, func(t *testing.T) {
			project := t.TempDir()
			selected := filepath.Join(t.TempDir(), "selected.toml")
			if kind == "invalid-project" {
				if err := os.WriteFile(selected, []byte("[spawn_pacing]\nenabled = false\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(project, ".ntm"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(project, ".ntm", "config.toml"), []byte("[invalid TOML"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			srv := NewHermeticServer("spawn-policy")
			defer srv.Stop()
			srv.projectDir = project
			if err := srv.ConfigureSpawnPolicy(selected, true); err != nil {
				t.Fatal(err)
			}
			configured := srv.spawnAgents
			srv.spawnAgents = func(ctx context.Context, opts robot.SpawnOptions) (*robot.SpawnOutput, error) {
				opts.LifecycleDeps = &robot.SpawnLifecycleDependencies{
					IsTMUXInstalled: func() bool { t.Error("invalid policy reached tmux discovery"); return false },
				}
				return configured(ctx, opts)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/policy-workers/agents/spawn", strings.NewReader(`{"cc_count":1}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, req)
			var output struct {
				Success bool   `json:"success"`
				Error   string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &output); err != nil || output.Success || !strings.Contains(output.Error, "load spawn policy") {
				t.Fatalf("synchronous policy refusal = %d %s (%v)", rec.Code, rec.Body.String(), err)
			}
			env := postJob(t, srv, `{"type":"swarm_spawn","params":{"session":"policy-workers","cc_count":1}}`)
			final := pollJobTerminal(t, srv, env.Job.ID)
			if final.Job.Status != string(JobStatusFailed) || !strings.Contains(final.Job.Error, "load spawn policy") || final.Job.Result["working_dir"] != project {
				t.Fatalf("queued policy refusal lost failure or project: %+v", final.Job)
			}
		})
	}
}

func TestConfiguredSwarmJobEnforcesFleetLimitWithoutAssignment(t *testing.T) {
	t.Setenv("NTM_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	project := t.TempDir()
	selected := filepath.Join(t.TempDir(), "selected.toml")
	if err := os.WriteFile(selected, []byte("[spawn_pacing]\nenabled = true\n[spawn_pacing.agent_type_limits]\nclaude = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := NewHermeticServer("spawn-policy-budget")
	defer srv.Stop()
	srv.projectDir = project
	if err := srv.ConfigureSpawnPolicy(selected, true); err != nil {
		t.Fatal(err)
	}
	configured := srv.spawnAgents
	srv.spawnAgents = func(ctx context.Context, opts robot.SpawnOptions) (*robot.SpawnOutput, error) {
		opts.LifecycleDeps = &robot.SpawnLifecycleDependencies{
			IsTMUXInstalled:  func() bool { return true },
			AcquireAdmission: func(context.Context) (func(), error) { return func() {}, nil },
			GetAllPanes: func(context.Context) (map[string][]tmux.Pane, error) {
				return map[string][]tmux.Pane{"existing": {{ID: "%9", Type: tmux.AgentClaude}}}, nil
			},
			SessionExists: func(context.Context, string) (bool, error) {
				t.Error("over-cap API spawn reached session creation preflight")
				return false, errors.New("unexpected lifecycle access")
			},
		}
		return configured(ctx, opts)
	}
	env := postJob(t, srv, `{"type":"swarm_spawn","params":{"session":"policy-budget","cc_count":1}}`)
	final := pollJobTerminal(t, srv, env.Job.ID)
	admission, ok := final.Job.Result["admission"].(map[string]interface{})
	if final.Job.Status != string(JobStatusFailed) || !ok || admission["reason"] != "agent_type_limit_exceeded" || admission["serialized"] != true {
		t.Fatalf("configured fleet cap was bypassed by ordinary HTTP spawn: %+v", final.Job)
	}
}
