package serve

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Dicklesworthstone/ntm/internal/config"
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
