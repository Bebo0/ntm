package robot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Dicklesworthstone/ntm/internal/config"
	"github.com/Dicklesworthstone/ntm/internal/recipe"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

// A recipe supplies counts and per-instance launch specifications, not another
// spawn engine. Keys use the shared engine's agent type and per-type ordinal;
// grouping by type therefore preserves ordinary spawn's pane/name ordering.
type spawnRecipeKey struct {
	agentType string
	number    int
}

type spawnRecipeLaunch struct {
	command string
	model   spawnLaunchModel
}

func spawnWorkingDirectory(opts SpawnOptions, cfg *config.Config) (string, error) {
	if opts.WorkingDir != "" {
		return opts.WorkingDir, nil
	}
	if cfg != nil {
		if dir := cfg.GetProjectDir(opts.Session); dir != "" {
			return dir, nil
		}
	}
	return os.Getwd()
}

// loadSpawnRecipe freezes the selected recipe before any tmux mutation. The
// existing loader owns builtin < user < project precedence and strict decoding.
// In particular, project recipes come from the launch directory, not serve's CWD.
func loadSpawnRecipe(opts SpawnOptions, cfg *config.Config) (SpawnOptions, *recipe.Recipe, error) {
	if opts.Preset == "" {
		return opts, nil, nil
	}
	name := strings.TrimSpace(opts.Preset)
	if name == "" {
		return opts, nil, errors.New("spawn preset must name a recipe")
	}
	// Adding counts to a preset is ambiguous (replace, add, or multiply).
	// Refuse rather than silently ignoring either explicit request.
	for _, count := range []int{opts.CCCount, opts.CodCount, opts.GmiCount, opts.AgyCount, opts.GrokCount, opts.OmpCount, opts.OcCount} {
		if count != 0 {
			return opts, nil, errors.New("spawn preset cannot be combined with explicit agent counts")
		}
	}
	dir, err := spawnWorkingDirectory(opts, cfg)
	if err != nil {
		return opts, nil, fmt.Errorf("resolve preset project: %w", err)
	}
	loader := recipe.NewLoader()
	loader.ProjectDir = dir
	selected, err := loader.Get(name)
	if err != nil {
		return opts, nil, fmt.Errorf("load spawn preset %q: %w", name, err)
	}
	expanded, err := expandSpawnRecipe(opts, selected)
	if err != nil {
		return opts, nil, err
	}
	// Reuse exactly the same directory for loading, admission, and execution.
	expanded.WorkingDir = dir
	frozen := *selected
	frozen.Agents = append([]recipe.AgentSpec(nil), selected.Agents...)
	return expanded, &frozen, nil
}

func expandSpawnRecipe(opts SpawnOptions, selected *recipe.Recipe) (SpawnOptions, error) {
	if selected == nil {
		return opts, errors.New("spawn preset returned no recipe")
	}
	if err := selected.Validate(); err != nil {
		return opts, fmt.Errorf("invalid spawn preset: %w", err)
	}
	// Work on a value copy so failure never partially rewrites the request.
	expanded := opts
	expanded.CCCount, expanded.CodCount, expanded.GmiCount, expanded.AgyCount = 0, 0, 0, 0
	expanded.GrokCount, expanded.OmpCount, expanded.OcCount = 0, 0, 0
	total := 0
	for i, spec := range selected.Agents {
		if strings.TrimSpace(spec.Persona) != "" {
			return opts, fmt.Errorf("spawn preset %q agent[%d]: personas are not supported by robot spawning; use ntm spawn --recipe", selected.Name, i)
		}
		// Validate above bounds each count, but independently bound the sum
		// before addition rather than trusting Recipe.TotalAgents arithmetic.
		if spec.Count > 50-total {
			return opts, fmt.Errorf("spawn preset %q exceeds 50 agents", selected.Name)
		}
		total += spec.Count
		switch tmux.AgentType(spec.Type).Canonical() {
		case tmux.AgentClaude:
			expanded.CCCount += spec.Count
		case tmux.AgentCodex:
			expanded.CodCount += spec.Count
		case tmux.AgentGemini:
			expanded.GmiCount += spec.Count
		case tmux.AgentAntigravity:
			expanded.AgyCount += spec.Count
		case tmux.AgentGrok:
			expanded.GrokCount += spec.Count
		case tmux.AgentOmp:
			expanded.OmpCount += spec.Count
		case tmux.AgentOpencode:
			expanded.OcCount += spec.Count
		default:
			return opts, fmt.Errorf("spawn preset %q agent[%d]: robot spawning does not support agent type %q", selected.Name, i, spec.Type)
		}
	}
	expanded.Preset = selected.Name
	return expanded, nil
}

// renderSpawnRecipe runs after the authoritative assignment configuration (when
// requested) is loaded, and before admission/lifecycle mutation. Every slot is
// rendered now: a bad model/effort in the last entry cannot leave half a fleet.
// Explicit request model/effort overrides win over the recipe, independently.
func renderSpawnRecipe(opts SpawnOptions, selected *recipe.Recipe, cfg *config.Config) (map[spawnRecipeKey]spawnRecipeLaunch, error) {
	if selected == nil {
		return nil, nil
	}
	if cfg == nil {
		// Unconfigured embedding callers still need real default templates to
		// honor recipe models; naked "claude"/"codex" cannot render overrides.
		// This does not implicitly read or replace their admission configuration.
		cfg = config.Default()
	}
	launches := make(map[spawnRecipeKey]spawnRecipeLaunch)
	ordinals := make(map[string]int)
	for i, spec := range selected.Agents {
		perType := opts
		model, effort := strings.TrimSpace(spec.Model), strings.TrimSpace(spec.ReasoningEffort)
		var modelOverride, effortOverride *string
		kind := ""
		switch tmux.AgentType(spec.Type).Canonical() {
		case tmux.AgentClaude:
			kind, modelOverride, effortOverride = "claude", &perType.CCModel, &perType.CCReasoningEffort
		case tmux.AgentCodex:
			kind, modelOverride, effortOverride = "codex", &perType.CodModel, &perType.CodReasoningEffort
		case tmux.AgentGemini:
			kind, modelOverride = "gemini", &perType.GmiModel
		case tmux.AgentAntigravity:
			kind = "antigravity"
		case tmux.AgentGrok:
			kind, modelOverride, effortOverride = "grok", &perType.GrokModel, &perType.GrokReasoningEffort
		case tmux.AgentOmp:
			kind, modelOverride, effortOverride = "omp", &perType.OmpModel, &perType.OmpReasoningEffort
		case tmux.AgentOpencode:
			kind, modelOverride = string(tmux.AgentOpencode), &perType.OcModel
		default:
			return nil, fmt.Errorf("spawn preset %q agent[%d]: unsupported robot agent type %q", selected.Name, i, spec.Type)
		}
		if (model != "" && modelOverride == nil) || (effort != "" && effortOverride == nil) {
			return nil, fmt.Errorf("spawn preset %q agent[%d]: %s does not support the supplied model/effort override", selected.Name, i, kind)
		}
		if modelOverride != nil && *modelOverride == "" {
			*modelOverride = model
		}
		if effortOverride != nil && *effortOverride == "" {
			*effortOverride = effort
		}
		commands, err := getAgentCommandsWithOverrides(cfg, perType)
		if err != nil {
			return nil, fmt.Errorf("spawn preset %q agent[%d]: %w", selected.Name, i, err)
		}
		launch := spawnRecipeLaunch{command: commands[kind], model: spawnLaunchModels(cfg, perType)[kind]}
		for n := 0; n < spec.Count; n++ {
			ordinals[kind]++
			launches[spawnRecipeKey{agentType: kind, number: ordinals[kind]}] = launch
		}
	}
	return launches, nil
}

func applySpawnRecipePreview(agents []SpawnedAgent, launches map[spawnRecipeKey]spawnRecipeLaunch) {
	ordinals := make(map[string]int)
	for i := range agents {
		kind := agents[i].Type
		if kind == "user" {
			continue
		}
		ordinals[kind]++
		if launch, ok := launches[spawnRecipeKey{agentType: kind, number: ordinals[kind]}]; ok {
			agents[i].Variant = launch.model.ModelAlias
		}
	}
}

// The final resolved model is immutable per launch. Carry it with the existing
// lifecycle context so progress/pacing/readiness decorators cannot accidentally
// replace a model-aware launcher with an unconfigured launcher. Cancellation,
// deadlines and durable job reporters are inherited from the same parent.
type spawnLaunchModelContextKey struct{}

func spawnLaunchModelFromContext(ctx context.Context) spawnLaunchModel {
	if ctx == nil {
		return spawnLaunchModel{}
	}
	model, _ := ctx.Value(spawnLaunchModelContextKey{}).(spawnLaunchModel)
	return model
}
