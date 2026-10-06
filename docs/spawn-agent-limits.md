# Fleet-wide agent-type admission limits

Robot spawn can enforce separate budgets for each canonical agent type across
all sessions visible to its tmux client. Configure limits in the selected NTM
configuration:

```toml
[spawn_pacing]
enabled = true

[spawn_pacing.agent_type_limits]
claude = 4
codex = 6
omp = 8
```

The accepted keys are `claude`, `codex`, `gemini`, `antigravity`, `grok`, `omp`,
and `opencode`. Use these canonical names, not CLI aliases such as `cc` or `cod`.
Unknown keys and negative limits are configuration errors. Zero or an omitted
key disables that additional per-type bound. The section is opt-in: its default
is empty, and the pre-existing `agent_caps` shared host budget is unchanged.
Both checks apply when both are configured; unused capacity for one type does
not override a configured limit for another.

```sh
ntm --robot-spawn=myproject --spawn-cc=2 --spawn-cod=1 --dry-run
```

A mixed request is evaluated as one batch before session creation, pane splits,
or agent launches. With four observed Claude agents and `claude = 4`, another
Claude agent is refused even if the shared host budget has headroom. A request
for Codex alone is not blocked solely because Claude is above its per-type
limit, although the shared host budget and pressure checks still apply.

The existing `admission` result includes sorted `agent_type_limits` rows with
running, requested, projected, limit, remaining headroom, and `blocks_request`.
A type-cap refusal uses `agent_type_limit_exceeded`. Dry runs remain previews:
they include the refusal/defer decision and proposed layout without launching.
An ordinary spawn returns the existing `RESOURCE_BUSY` error response.

When tmux enumeration fails, any configured count budget requires a defer with
`agent_inventory_unavailable`. The response carries
`agent_inventory_available: false` and its error; a failed or partial read is
not treated as an empty fleet. Per-type policies also reject missing or
inconsistent count evidence. A successful empty inventory remains valid.
Disabling spawn pacing disables both count-budget checks, but does not turn
failed observations into successful inventory evidence.

## Concurrent local spawning

Count-capped local `GetSpawn` calls now acquire a cross-process admission fence
**before** observing the fleet and retain it through the entire launch batch.
A contender waits for the current batch and then reads the fleet again, so two
cooperating processes cannot both spend the same observed headroom. This applies
to the shared host budget and per-type-only budgets, including expanded recipes.
The fence also survives the progress, launch-spacing, and per-agent readiness
wrappers: it stays held while those wrappers finish each launch.

The lock is released after the last launch, before starting the session monitor,
the final fleet readiness pass, or work assignment. Early refusals, errors,
cancellation, and panic unwinding release it too. Kernel file ownership is also
released if the NTM process exits; spawned agents do not inherit the descriptor.
Already launched panes retain their existing identities and are counted by the
next owner. No agent or pane is killed, and a partially successful batch is not
rolled back.

Admission waits up to 30 seconds for ownership, bounded further by caller/job
cancellation. Exhaustion returns `RESOURCE_BUSY` with admission reason
`spawn_admission_busy`; an inaccessible or unsupported fence returns
`spawn_admission_unavailable`. No fleet inventory is read or lifecycle mutation
performed after these failures. The response marks inventory unavailable rather
than claiming zero running agents. Caller cancellation retains `TIMEOUT`.
There is no automatic retry, FIFO fairness guarantee, or detached queue.

`admission.serialized` is true only for evaluations made while holding the fence.
Dry runs neither acquire ownership nor create lock files; they remain advisory
previews with `serialized: false`. Disabling pacing or configuring no positive
count limits keeps the existing unfenced behavior.

## Scope

The fence covers Linux/macOS processes sharing the same user cache directory
(`os.UserCacheDir`, normally `XDG_CACHE_HOME` on Linux). Its rendezvous is
`ntm/spawn-admission/fleet.lock` under that root, independent of the working
directory, selected NTM config, and session name. Even separate local tmux
servers sharing that root serialize launches, conservatively. Keep the cache
root consistent across participating processes and do not remove/replace the
lock file while NTM is running. Symlinks and unsafe lock-file types fail closed.

This is cooperating-launch serialization, not a durable capacity reservation,
provider-seat discovery, or token-quota enforcement. Human `ntm spawn`,
unconfigured/disabled callers, older binaries, manual tmux launches, and processes
using different cache roots do not participate. They can still change fleet
counts concurrently. Different requests can also select different limits; this
does not install one mandatory host-wide policy. Remote tmux and unsupported
platforms refuse count-capped execution rather than claiming a local lock protects
them; run the capped command on the target Linux/macOS host instead. Remote dry
runs remain advisory.

Agents outside the selected tmux server are not counted. An `omp` or OpenCode
agent may use different providers internally; its type is not a subscription.

The configuration reaches `--robot-spawn` through its normal selected config
and `GetSpawn` callers that provide a config (including the authoritative
assignment-policy path). It does not change ordinary human `ntm spawn` pacing.
Existing launch readiness, work-source checks, and reservation gates remain
separate.

## HTTP server policy

`ntm serve`, `ntm serve --web`, and `ntm web` bind their spawn backend to the same
global configuration selected by `--config`, `NTM_CONFIG`, or the default path.
Both `POST /api/v1/sessions/{sessionId}/agents/spawn` and `swarm_spawn` jobs now
load that policy, even without `assign_work`. Configured agent commands, model
defaults, fleet limits, and serialized admission therefore apply to API launches
as well as direct robot launches; HTTP no longer implicitly supplies a nil
configuration that bypasses these controls.

The selected config path is made absolute when the server is constructed. Its
contents and the launch project's overlay are loaded strictly at execution,
not frozen when a job is accepted. Missing explicitly selected files and invalid
global or project configuration fail with `INVALID_FLAG` before the spawn engine
can create panes or launch agents. An absent default-path file still uses the
built-in defaults. Each call loads its own configuration rather than reusing a
mutable merged config from another project. Existing project-overlay rules still
apply: repositories cannot override the operator's agent execution commands or
erase global assignment gates.

Synchronous launches without a directory use the server's selected project;
relative overrides resolve against that project. Queued jobs retain the absolute
directory captured at admission, even after the server changes project. The same
global config selection is forwarded to spawn's assignment preflight. Existing
partial results, cancellation, progress, readiness, and reservation checks are
not replaced. A preview still reports the admission decision without launching.

Embedding callers of `serve.New` must explicitly call
`ConfigureSpawnPolicy(selectedPath, requireSelectedFile)` during construction,
before restoring jobs or serving requests, to opt into this backend. `New` itself
remains side-effect-free and does not implicitly read user configuration. This
method is not a concurrent runtime policy setter. No HTTP parameter permits a
client to replace the server's selected global config path.

## Adding to a running fleet

`ntm add` now participates in the same admission policy and cross-process fence
as robot/HTTP spawning. This includes `ntm scale` scale-up and dashboard actions
that invoke add. No separate quota setting is introduced: the selected
`spawn_pacing` policy applies, and disabling it retains the unfenced add behavior.

Inside the add execution loop, the complete agent-spec batch is counted before
its per-instance expansion. Resolved model/persona entries contribute their
underlying agent counts; Cursor, Aider,
Ollama and plugin agents count toward the shared host budget even when no separate
per-type limit is configured for them. Existing configured type limits still
use canonical names. Pane forecasts treat add as an increment, not a request to
reuse existing panes. Overflowing or negative counts fail before expansion.

Existing pre-add hooks run before admission, without owning the fence: a hook
may itself launch work, so add must count afterwards rather than rely on an older
snapshot. Once admitted, add retains the fence across its entire existing loop,
including splits, startup delays, command preparation and inline prompt/readiness
work. It releases ownership before Agent Mail registration and post-add hooks.
Errors, cancellation and panic unwinding release it too. A refused admission
does not run NTM's auto-checkpoint, split panes, or launch agents; pre-add hook
effects are not rolled back. Successful or partial launches are never killed.

Admission conservatively includes every requested agent before CAAM seat
selection may skip individual launches. It does not promise that every admitted
agent will launch, reserve provider seats, or change persona/plugin execution.
Long inline startup or prompt work can hold the fence long enough for a competing
request's existing 30-second acquisition budget to expire.

Successful `ntm add --json` output includes the shared `admission` receipt.
Refusals include that decision in the existing lifecycle error envelope, with
`RESOURCE_BUSY` for cap/pressure/ownership failures and `TIMEOUT` for cancellation.
Composed callers receive the same typed error and `AddOutcome.Admission` without
nested JSON. As with robot spawning, only count-capped local Linux/macOS requests
are serialized; uncapped pressure checks are advisory snapshots. This does not
yet make human `ntm spawn`, manual launches, or different cache roots cooperate.
