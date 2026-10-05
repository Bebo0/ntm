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
ntm --robot-spawn=myproject --spawn-cc=2 --spawn-cod=1 --spawn-dry-run
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

## Scope

This is a check of recognized agent panes at admission time, not an atomic
capacity reservation, provider-seat discovery, or token-quota enforcement.
Concurrent independent spawns can still race between observation and launch;
this change does not introduce a cross-process lock or reserve future capacity.
Agents outside the selected tmux server are not counted. An `omp` or OpenCode
agent may use different providers internally; its type is not a subscription.

The configuration reaches `--robot-spawn` through its normal selected config
and `GetSpawn` callers that provide a config (including the authoritative
assignment-policy path). It does not change ordinary human `ntm spawn` pacing,
or make unconfigured REST calls start loading global configuration. Existing
launch readiness, work-source checks, and reservation gates remain separate.
