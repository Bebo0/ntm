# Passive process-triage health observations

The existing PT health monitor uses the supported passive command:

```sh
pt agent watch --once --threshold low --format jsonl
```

This replaces the nonexistent `pt classify --json --pid ...` contract in the
live monitor (pt-core has no `classify` subcommand, and no `pt watch --session`
either; that dead adapter method is gone). Existing `ntm serve` and dashboard
monitor startup use it without new NTM flags. Construction does not start PT.
Availability checks require the passive watch options, rather than accepting a
version string as proof that classification is supported.

In `ntm serve`, monitor classification changes and alerts are published to the
durable attention feed that `--robot-attention` and `--robot-snapshot --since`
read from other processes. The robot agent-health reader reuses a monitor
running in its own process, or takes a standalone passive snapshot as described
below. Standalone snapshots do not publish monitor alerts to the attention feed.

Each poll runs one bounded whole-host watch iteration and filters its results
to the processes attributed to agent panes. It does not call `agent apply`, pass
`--robot`/`--yes`, install a notification command, or start a resident PT daemon.
Unlike `agent plan`, this upstream path does not create a new plan session on
every poll. PT still reads its configured priors, guardrails and minimum process
age; NTM does not turn those protections off to get more classifications.

## What the result means

Watch is a thresholded abandonment detector, not a complete health classifier.
Its `confidence` is abandonment probability. A `kill` recommendation becomes an
advisory suspected-abandonment signal (`stuck` in the existing NTM vocabulary).
It is never authorization to kill, interrupt, restart or assign a process.
`spare`, `review`, and omitted PIDs remain `unknown`, not `useful` or zero-risk.
The original recommendation and probability accompany reported candidates;
non-candidates have no invented probability. No zombie verdict is inferred from
this interface.

Monitor events and robot health details retain `source: "pt_agent_watch"`, the
raw recommendation and abandonment probability. Robot details expose
`observed_at` from the completed monitor sample. A running monitor with no
matching observations yields `pt_status: "no_observations"`, not an available
healthy sample.

Standalone `ntm --robot-agent-health=<session>` reads now obtain one bounded
passive snapshot when no resident monitor is running in the current process.
No extra flag or daemon is required. The response identifies
`pt_observation_mode: "snapshot"` or `"monitor"`; `source: "pt_agent_watch"`
continues to identify the evidence provider. Snapshot histories begin at the
current sample, so their duration starts at zero rather than inventing a prior
stuck interval. Subsequent standalone calls do not accumulate history.

A running resident monitor is reused without starting a duplicate host scan.
Disabled PT, empty/shell-only selections, dead agent CLIs and unavailable local
observations do not invoke PT. PT discovery and command execution share the existing
PT timeout (10 seconds by default); a nonpositive programmatic timeout uses that
default. Filesystem process-tree traversal remains best-effort rather than a
hard real-time cancellation guarantee. `pt_status: "timed_out"`, `"sample_failed"`, `"unavailable"` and
`"no_observations"` explicitly distinguish degraded reads. A PT failure does not
discard successful local health results or attach a partial PT verdict. Raw
subprocess errors are not returned in the robot health response.

One-shot reads use the same synchronous sampler and pane aggregation as the
resident monitor. They do not start workers, modify the global monitor, emit
its callbacks, or persist state. They also do not query Rano: a health read must
not incidentally open or migrate an observer database. Rano enrichment remains
available in the configured resident monitor. There is no persistent health
cache or automatic remediation.

## Pane and lifecycle ownership

Every successful poll produces one observation per durable tmux pane ID. Shell
and child results are combined deterministically: an unreported sibling cannot
hide a suspected-abandonment signal. Equal-priority results select the stronger
evidence, then the lower PID. Counts, histories and alerts advance once per pane,
not once per child. Session/window/index metadata follows that pane into robot
lookups; equal pane indices across windows are not silently interchangeable.
Callbacks carry the observed session, including for the all-session monitor.
The root `pane_pid` is retained separately from the representative child PID.
Robot readers require it to match the local terminal observation, so respawned
panes cannot inherit results through reused pane indices. A changed root also
resets resident observation history. These checks do not prove historical PID
incarnation identity or eliminate every sampling race.

Recent Rano `last_connection` evidence (`use_rano_data`, read from the database
named by `[integrations.rano] sqlite_path`; see `docs/rano-stats.md`) can
downgrade a suspected-stuck process to `waiting`. It is not an HTTP request or
byte counter. Unavailable Rano data, including a missing database, does not
invalidate a successful PT sample. Failed PT/topology samples clear
cached classifications so unavailable observations do not accrue stuck duration.
A changed representative process resets its clock and history. Returned history
slices are detached from the monitor's mutable state.

Stop cancels the active sample and joins the worker before returning. Slow polls
are followed by the configured interval, not immediate catch-up scans. Existing
alert thresholds remain; this repair adds no automatic remediation.

## Bounds and limitations

Watch stdout is bounded to 10 MiB, stderr to 64 KiB and each JSONL record to
64 KiB. Capture uses writer-only wrappers so fast-copy methods cannot bypass the
limits. Nonzero exits, malformed/truncated streams, duplicate process records
and invalid probabilities invalidate the complete sample. Raw process command
lines and subprocess stderr are not copied into classification responses.
Cancellation and deadline errors retain their identities.

The verified contract is `Dicklesworthstone/process_triage` at
`8ac066c38b2e14223b6c9cf11a6b12a81c6c0e1c`, `crates/pt-core/src/main.rs`:
`AgentWatchArgs`, `run_agent_watch`, `evaluate_watch_candidate`, and
`parse_watch_threshold`. Other/older PT binaries without these options remain
unavailable; NTM never falls back to a mutating planner.

This still scans the whole host and uses current process-tree attribution, not
historical PID-incarnation proof. Protected, young and below-threshold processes
are omitted by PT, so many healthy agents correctly remain unclassified.
Process activity can change after sampling. Terminal readiness, dispatch safety,
provider quota, and restart eligibility remain separate mechanisms.
