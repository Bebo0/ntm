package robot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/config"
	"github.com/Dicklesworthstone/ntm/internal/pressure"
	"github.com/Dicklesworthstone/ntm/internal/recipe"
	"github.com/Dicklesworthstone/ntm/internal/resilience"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

func TestSpawnRecipeExpansionPreservesControlsAndRepeatedTypes(t *testing.T) {
	r := &recipe.Recipe{Name: "mixed", Agents: []recipe.AgentSpec{
		{Type: "claude-code", Count: 2, Model: "one"},
		{Type: "cod", Count: 1},
		{Type: "cc", Count: 3, Model: "two"},
		{Type: "oh-my-pi", Count: 1},
	}}
	opts := SpawnOptions{Session: "chosen--lane", Preset: "MIXED", DryRun: true, Safety: true,
		AssignWork: true, AssignStrategy: "top-n", RequireReservation: true,
		ReservationPaths: []string{"src/**"}, CCReasoningEffort: "high",
		LifecycleDeps: &SpawnLifecycleDependencies{},
	}
	got, err := expandSpawnRecipe(opts, r)
	if err != nil {
		t.Fatal(err)
	}
	want := opts
	want.Preset, want.CCCount, want.CodCount, want.OmpCount = "mixed", 5, 1, 1
	if !reflect.DeepEqual(got, want) || opts.CCCount != 0 || r.Agents[0].Type != "claude-code" {
		t.Fatalf("expansion changed controls/caller or lost repeated types: %+v", got)
	}
}

func TestSpawnRecipeRejectsUnsupportedIntentBeforeExpansion(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    *recipe.Recipe
	}{
		{"missing", nil},
		{"empty", &recipe.Recipe{Name: "empty"}},
		{"unknown type", &recipe.Recipe{Name: "bad", Agents: []recipe.AgentSpec{{Type: "typo", Count: 1}}}},
		{"unsupported robot type", &recipe.Recipe{Name: "bad", Agents: []recipe.AgentSpec{{Type: "cursor", Count: 1}}}},
		{"persona", &recipe.Recipe{Name: "bad", Agents: []recipe.AgentSpec{{Type: "cc", Count: 1, Persona: "reviewer"}}}},
		{"zero count", &recipe.Recipe{Name: "bad", Agents: []recipe.AgentSpec{{Type: "cc", Count: 0}}}},
		{"negative count", &recipe.Recipe{Name: "bad", Agents: []recipe.AgentSpec{{Type: "cc", Count: -1}}}},
		{"overlarge count", &recipe.Recipe{Name: "bad", Agents: []recipe.AgentSpec{{Type: "cc", Count: 21}}}},
		{"overlarge fleet", &recipe.Recipe{Name: "bad", Agents: []recipe.AgentSpec{{Type: "cc", Count: 20}, {Type: "cod", Count: 20}, {Type: "gmi", Count: 20}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := SpawnOptions{Session: "original", Preset: "bad", Safety: true}
			got, err := expandSpawnRecipe(opts, tc.r)
			if err == nil || !reflect.DeepEqual(got, opts) {
				t.Fatalf("invalid recipe accepted or partially changed request: %+v %v", got, err)
			}
		})
	}
}

func TestSpawnRecipeRenderPreservesPerInstanceModelsAndEffort(t *testing.T) {
	cfg := config.Default()
	cfg.Agents.Claude = "claude --model {{.Model}} --effort {{.ReasoningEffort}}"
	cfg.Agents.Codex = "codex --model {{.Model}} --effort {{.ReasoningEffort}}"
	r := &recipe.Recipe{Name: "mixed", Agents: []recipe.AgentSpec{
		{Type: "cc", Count: 2, Model: "first-model", ReasoningEffort: "low"},
		{Type: "cod", Count: 1, Model: "other-model", ReasoningEffort: "medium"},
		{Type: "claude", Count: 1, Model: "second-model", ReasoningEffort: "high"},
	}}
	opts, err := expandSpawnRecipe(SpawnOptions{Preset: "mixed"}, r)
	if err != nil {
		t.Fatal(err)
	}
	for _, override := range []bool{false, true} {
		t.Run(fmt.Sprintf("request_override=%t", override), func(t *testing.T) {
			request := opts
			if override {
				request.CCModel, request.CCReasoningEffort = "requested-model", "medium"
			}
			launches, err := renderSpawnRecipe(request, r, cfg)
			if err != nil || len(launches) != 4 {
				t.Fatalf("render: %v %+v", err, launches)
			}
			for n := 1; n <= 3; n++ {
				model, effort := "first-model", "low"
				if n == 3 {
					model, effort = "second-model", "high"
				}
				if override {
					model, effort = "requested-model", "medium"
				}
				got := launches[spawnRecipeKey{agentType: "claude", number: n}]
				if got.model.Model != model || got.model.ModelAlias != model || got.model.ReasoningEffort != effort ||
					!strings.Contains(got.command, model) || !strings.Contains(got.command, effort) {
					t.Fatalf("slot %d lost its launch specification: %+v", n, got)
				}
			}
			other := launches[spawnRecipeKey{agentType: "codex", number: 1}]
			if other.model.ModelAlias != "other-model" || other.model.ReasoningEffort != "medium" {
				t.Fatalf("override leaked across agent types: %+v", other)
			}
		})
	}
	if r.Agents[0].Model != "first-model" || opts.CCModel != "" {
		t.Fatal("rendering mutated the recipe or caller's model options")
	}
}

func TestSpawnRecipeRenderRejectsLaterInvalidSpecification(t *testing.T) {
	for _, bad := range []recipe.AgentSpec{
		{Type: "cod", Count: 1, Model: "cannot-render"},
		{Type: "gmi", Count: 1, ReasoningEffort: "high"},
		{Type: "agy", Count: 1, Model: "cannot-override-pinned-model"},
	} {
		cfg := config.Default()
		cfg.Agents.Claude = "claude --model {{.Model}}"
		cfg.Agents.Codex = "codex" // Explicit model cannot be honored.
		r := &recipe.Recipe{Name: "invalid-tail", Agents: []recipe.AgentSpec{{Type: "cc", Count: 1, Model: "valid-model"}, bad}}
		got, err := renderSpawnRecipe(SpawnOptions{}, r, cfg)
		if err == nil || got != nil || !strings.Contains(err.Error(), "agent[1]") {
			t.Fatalf("last entry was ignored or leaked a partial plan: %+v %v", got, err)
		}
	}
}

func TestSpawnRecipePreviewUsesPerTypeOrdinals(t *testing.T) {
	agents := []SpawnedAgent{{Type: "user", Variant: "user"}, {Type: "claude"}, {Type: "claude"}, {Type: "codex"}}
	plan := map[spawnRecipeKey]spawnRecipeLaunch{
		{agentType: "claude", number: 1}: {model: spawnLaunchModel{ModelAlias: "writer"}},
		{agentType: "claude", number: 2}: {model: spawnLaunchModel{ModelAlias: "reviewer"}},
		{agentType: "codex", number: 1}:  {model: spawnLaunchModel{ModelAlias: "coder"}},
	}
	applySpawnRecipePreview(agents, plan)
	if got := []string{agents[0].Variant, agents[1].Variant, agents[2].Variant, agents[3].Variant}; !reflect.DeepEqual(got, []string{"user", "writer", "reviewer", "coder"}) {
		t.Fatalf("preview lost per-instance models: %v", got)
	}
}

func TestSpawnLaunchModelBindingPreservesContextAndIsolation(t *testing.T) {
	type reporterKey struct{}
	parent, cancel := context.WithTimeout(context.WithValue(context.Background(), reporterKey{}, "reporter"), time.Minute)
	defer cancel()
	first := spawnLaunchModel{Model: "first", ModelAlias: "alias-one", ReasoningEffort: "high"}
	second := spawnLaunchModel{Model: "second", ModelAlias: "alias-two", ReasoningEffort: "low"}
	a := context.WithValue(parent, spawnLaunchModelContextKey{}, first)
	b := context.WithValue(parent, spawnLaunchModelContextKey{}, second)
	if spawnLaunchModelFromContext(a) != first || spawnLaunchModelFromContext(b) != second || spawnLaunchModelFromContext(parent) != (spawnLaunchModel{}) {
		t.Fatal("launch models crossed request boundaries")
	}
	if deadline, ok := a.Deadline(); !ok || deadline.IsZero() || a.Value(reporterKey{}) != "reporter" {
		t.Fatal("binding detached deadline or journal reporter")
	}
	cancel()
	if !errors.Is(a.Err(), context.Canceled) || !errors.Is(b.Err(), context.Canceled) {
		t.Fatal("binding detached cancellation")
	}
}

func writeRobotSpawnRecipe(t *testing.T, project, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(project, ".ntm"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".ntm", "recipes.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// These tests exercise GetSpawn, the shared robot/REST engine, with the real
// recipe loader, command renderer, admission check, and lifecycle port contract.
func TestGetSpawnRecipeBuiltinPreviews(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()
	for _, name := range recipe.BuiltinNames() {
		t.Run(name, func(t *testing.T) {
			loader := recipe.NewLoader()
			loader.ProjectDir = project
			selected, err := loader.Get(name)
			if err != nil {
				t.Fatal(err)
			}
			out, err := GetSpawn(context.Background(), SpawnOptions{Session: "preset-preview", Preset: name,
				WorkingDir: project, DryRun: true, LifecycleDeps: testSpawnLifecycleDependencies(nil)}, testSpawnConfig())
			if err != nil || out == nil || !out.Success || !out.DryRun || out.PresetUsed != name ||
				len(out.WouldCreate) != selected.TotalAgents()+1 || len(out.Agents) != 0 {
				t.Fatalf("preset-only request did not produce its actual fleet: %+v %v", out, err)
			}
		})
	}
}

func TestGetSpawnRecipeUsesLaunchProjectAndCannotBypassCaps(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()
	writeRobotSpawnRecipe(t, project, `[[recipes]]
name = "minimal"
[[recipes.agents]]
type = "cc"
count = 3
model = "project-model"
`)
	cfg := config.Default()
	cfg.SpawnPacing.AgentTypeLimits = map[string]int{"claude": 2}
	cfg.Agents.Claude = "claude --model {{.Model}}"
	deps := testSpawnLifecycleDependencies(nil)
	mutations := 0
	deps.CreateSession = func(context.Context, string, string, int) error { mutations++; return nil }
	deps.GetPanes = func(context.Context, string) ([]tmux.Pane, error) { mutations++; return nil, nil }
	for _, preview := range []bool{true, false} {
		out, err := GetSpawn(context.Background(), SpawnOptions{Session: "preset-cap", Preset: "minimal",
			WorkingDir: project, DryRun: preview, LifecycleDeps: deps}, cfg)
		if err != nil || out == nil || out.Admission == nil || out.Admission.Reason != "agent_type_limit_exceeded" ||
			out.Admission.RequestedAgents != 3 || out.Admission.Decision != pressure.SpawnAdmissionRefuse || mutations != 0 {
			t.Fatalf("preset counts bypassed admission or selected CWD recipe: %+v %v mutations=%d", out, err, mutations)
		}
		if preview {
			if !out.Success || len(out.WouldCreate) != 4 || out.WouldCreate[1].Variant != "project-model" {
				t.Fatalf("refused preview lost proposed recipe fleet: %+v", out)
			}
		} else if out.Success || out.ErrorCode != ErrCodeResourceBusy {
			t.Fatalf("real over-cap preset spawn succeeded: %+v", out)
		}
	}
}

func TestGetSpawnRecipeRejectsAmbiguousAndMalformedRequests(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()
	for _, opts := range []SpawnOptions{
		{Preset: "does-not-exist"}, {Preset: "minimal", CCCount: 1}, {Preset: " "},
	} {
		opts.Session, opts.WorkingDir = "invalid-preset", project
		calls := 0
		opts.LifecycleDeps = &SpawnLifecycleDependencies{IsTMUXInstalled: func() bool { calls++; return true }}
		out, err := GetSpawn(context.Background(), opts, testSpawnConfig())
		if err != nil || out == nil || out.Success || out.ErrorCode != ErrCodeInvalidFlag || calls != 0 {
			t.Fatalf("invalid preset crossed the preflight boundary: %+v %v calls=%d", out, err, calls)
		}
	}
	// A malformed recipe file cannot be silently replaced by a builtin.
	writeRobotSpawnRecipe(t, project, `[[recipes]]
name = "minimal"
unknown_control = true
`)
	out, err := GetSpawn(context.Background(), SpawnOptions{Session: "invalid-preset", Preset: "minimal", WorkingDir: project}, testSpawnConfig())
	if err != nil || out == nil || out.Success || !strings.Contains(out.Error, "unknown") {
		t.Fatalf("malformed project recipe was ignored: %+v %v", out, err)
	}
	// Non-recipe requests must not start reading recipe files.
	out, err = GetSpawn(context.Background(), SpawnOptions{Session: "ordinary", CCCount: 1, WorkingDir: project,
		DryRun: true, LifecycleDeps: testSpawnLifecycleDependencies(nil)}, testSpawnConfig())
	if err != nil || out == nil || !out.Success {
		t.Fatalf("recipe error leaked into ordinary count spawning: %+v %v", out, err)
	}
}

func TestGetSpawnRecipeLaunchesAndRecoversExactInstanceCommands(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()
	writeRobotSpawnRecipe(t, project, `[[recipes]]
name = "two-models"
[[recipes.agents]]
type = "cc"
count = 1
model = "first-model"
reasoning_effort = "low"
[[recipes.agents]]
type = "claude-code"
count = 1
model = "second-model"
reasoning_effort = "high"
`)
	cfg := testSpawnConfig()
	cfg.Agents.Claude = "claude --model {{.Model}} --effort {{.ReasoningEffort}}"
	deps := testSpawnLifecycleDependencies(spawnMonitorTestPanes())
	var commands []string
	var models []spawnLaunchModel
	var manifest resilience.SpawnMonitorRequest
	deps.LaunchAgent = func(ctx context.Context, pane tmux.Pane, session, kind string, n int, dir, command string) (SpawnedAgent, error) {
		if dir != project || kind != "claude" || n != len(commands)+1 {
			t.Fatalf("wrong recipe slot: dir=%q kind=%q ordinal=%d", dir, kind, n)
		}
		commands = append(commands, command)
		models = append(models, spawnLaunchModelFromContext(ctx))
		return SpawnedAgent{Pane: pane.Ref().Physical(), Type: kind}, nil
	}
	deps.WaitForReady = func(_ context.Context, out *SpawnOutput, _ time.Duration) error {
		for i := range out.Agents {
			out.Agents[i].Ready = true
		}
		return nil
	}
	deps.StartSessionMonitor = func(_ context.Context, req resilience.SpawnMonitorRequest) (*resilience.SpawnMonitorResult, error) {
		manifest = req
		return &resilience.SpawnMonitorResult{MonitorStarted: true}, nil
	}
	opts := WithSpawnProgress(SpawnOptions{Session: "recipe-launch", Preset: "two-models", WorkingDir: project, LifecycleDeps: deps}, func(SpawnProgress) error { return nil })
	var err error
	opts, err = WithSpawnLaunchInterval(opts, time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	opts, err = WithSpawnLaunchReadiness(opts, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	out, err := GetSpawn(context.Background(), opts, cfg)
	if err != nil || out == nil || !out.Success || len(commands) != 2 || len(manifest.Agents) != 2 {
		t.Fatalf("decorated recipe spawn failed: %+v %v commands=%v manifest=%+v", out, err, commands, manifest)
	}
	for i, want := range []spawnLaunchModel{{Model: "first-model", ModelAlias: "first-model", ReasoningEffort: "low"}, {Model: "second-model", ModelAlias: "second-model", ReasoningEffort: "high"}} {
		if models[i] != want || !strings.Contains(commands[i], want.Model) || !strings.Contains(commands[i], want.ReasoningEffort) ||
			manifest.Agents[i].Command != commands[i] || manifest.Agents[i].PaneID != fmt.Sprintf("%%%d", i+1) ||
			out.Agents[i+1].Variant != want.ModelAlias || !out.Agents[i+1].Ready {
			t.Fatalf("slot %d lost per-instance launch/recovery data: model=%+v command=%s manifest=%+v out=%+v", i, models[i], commands[i], manifest.Agents[i], out.Agents[i+1])
		}
	}
}

func TestSpawnRecipeRequestOverridesAreIndependent(t *testing.T) {
	cfg := config.Default()
	cfg.Agents.Claude = "claude --model {{.Model}} --effort {{.ReasoningEffort}}"
	r := &recipe.Recipe{Name: "overrides", Agents: []recipe.AgentSpec{{Type: "cc", Count: 1, Model: "recipe-model", ReasoningEffort: "low"}}}
	for _, tc := range []struct {
		name, model, effort, wantModel, wantEffort string
	}{
		{"model only", "requested-model", "", "requested-model", "low"},
		{"effort only", "", "high", "recipe-model", "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := SpawnOptions{CCModel: tc.model, CCReasoningEffort: tc.effort}
			plan, err := renderSpawnRecipe(opts, r, cfg)
			if err != nil {
				t.Fatal(err)
			}
			got := plan[spawnRecipeKey{agentType: "claude", number: 1}]
			if got.model.Model != tc.wantModel || got.model.ReasoningEffort != tc.wantEffort ||
				!strings.Contains(got.command, tc.wantModel) || !strings.Contains(got.command, tc.wantEffort) {
				t.Fatalf("one override erased the other recipe field: %+v", got)
			}
		})
	}
}

func TestGetSpawnRecipeOpencodeAliasesPreview(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()
	writeRobotSpawnRecipe(t, project, `[[recipes]]
name = "opencode-team"
[[recipes.agents]]
type = "opencode"
count = 1
model = "provider/first"
[[recipes.agents]]
type = "oc"
count = 1
model = "provider/second"
`)
	out, err := GetSpawn(context.Background(), SpawnOptions{
		Session: "oc-recipe", Preset: "opencode-team", WorkingDir: project,
		NoUserPane: true, DryRun: true, LifecycleDeps: testSpawnLifecycleDependencies(nil),
	}, testSpawnConfig())
	if err != nil || out == nil || !out.Success || len(out.WouldCreate) != 2 {
		t.Fatalf("OpenCode recipe was rejected or truncated: %+v %v", out, err)
	}
	for i, want := range []string{"provider/first", "provider/second"} {
		if got := out.WouldCreate[i]; got.Type != string(tmux.AgentOpencode) || got.Variant != want {
			t.Fatalf("OpenCode slot %d: %+v, want %s", i, got, want)
		}
	}
}

func TestSpawnRecipeLaunchReceiptKeepsModelOnCancellation(t *testing.T) {
	model := spawnLaunchModel{Model: "resolved-model", ModelAlias: "requested-alias", ReasoningEffort: "high"}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), spawnLaunchModelContextKey{}, model))
	cancel()
	agent, err := launchAgent(ctx, tmux.Pane{ID: "%77", WindowIndex: 2, Index: 3}, "model-receipt", "claude", 1, "/project", "unused")
	if !errors.Is(err, context.Canceled) || agent.Ready || agent.Variant != model.ModelAlias || agent.Pane != "2.3" {
		t.Fatalf("launch outcome lost model evidence before progress publication: %+v %v", agent, err)
	}
}
