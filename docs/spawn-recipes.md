# Recipe-backed robot and job spawning

Robot spawning now resolves `--spawn-preset` through the existing recipe loader
instead of merely recording its name. A preset supplies agent counts; do not
combine it with explicit count flags.

```sh
ntm --robot-spawn=myproject --spawn-preset=full-stack --spawn-dry-run
```

The same shared engine serves asynchronous jobs:

```json
{
  "type": "swarm_spawn",
  "params": {
    "session": "myproject",
    "preset": "full-stack",
    "dry_run": true,
    "launch_ready_timeout": "45s"
  }
}
```

Submit the JSON to `POST /api/v1/jobs`. Change `dry_run` to `false` for execution.
All existing work-source, reservation, fleet admission, readiness, cancellation,
and recovery checks remain in force. Expanded counts, not an empty request,
reach admission. A refused dry run is still a preview and carries its refusal
in `admission`; it does not create panes or launch agents.

Recipes use the existing builtin < user < project precedence. Project recipes
are read from the **launch directory's** `.ntm/recipes.toml`, not an unrelated
server working directory. Queued swarm jobs already bind that directory at
admission. The recipe is read once when the job executes; this freezes its
resolved entries for that execution, not its contents at HTTP acceptance.
Unknown presets, malformed recipe files, and ambiguous preset-plus-count
requests fail before launching. `preset_used` reports the selected recipe name.

Multiple entries of one agent type may specify different models and reasoning
efforts. The engine retains its normal type grouping; entries within each type
retain recipe order and receive consecutive per-type ordinals. Explicit
per-type request model/effort overrides take precedence over the corresponding
recipe field. Model aliases still resolve through the selected configuration.
Every instance command is rendered before lifecycle mutation, so an invalid
last entry cannot leave an earlier subset running.

Previews and launch results expose requested model aliases as `variant`. Each
actual launch carries its resolved model, alias and effort through the existing
progress/pacing/readiness decorators into the durable pane launch specification.
The resilience monitor receives the exact command for each pane, rather than
one last-writer-wins command per agent type. The admission and lifecycle engines
are unchanged; the recipe supplies their inputs.

Robot spawning supports the intersection of the recipe loader's accepted types
and the robot engine's existing types: Claude, Codex, Gemini, Antigravity, Grok,
Oh My Pi, and OpenCode. Other recipe types, persona entries, unsupported model/effort
controls, and existing unsupported readiness/assignment combinations fail
explicitly rather than being dropped. Persona recipes remain available through
`ntm spawn --recipe`. This change does not add a new persona-delivery mechanism
or make the robot engine support every type accepted by the human spawn path.

When an embedding caller supplies no configuration, recipe commands use the
built-in default templates so recipe models can actually be honored. This does
not load user configuration implicitly or change that caller's admission policy.
Non-preset spawning does not start reading recipe files and retains its existing
configuration and directory resolution semantics.
