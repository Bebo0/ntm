# Rano connection observations

`ntm --robot-rano-stats` now consumes Rano's supported JSONL export instead of
calling the nonexistent `rano stats --all --json` command. This implements the
connection-observation portion of `bd-r11uk`; the separate process-triage command
contract is not changed.

Run NTM from the directory containing the observer's default `observer.sqlite`:

```sh
ntm --robot-rano-stats --rano-window=5m
ntm --robot-rano-stats --rano-window=1h --panes=0,1
```

NTM does not start an observer, enable packet capture, or request elevated
capabilities. Start Rano separately to record observations. A missing database,
unsupported export command, capture limit, failed process or malformed export
returns a failure, not a successful zero-traffic report. The existing operational
status probe does not prove that a recording process is running or that its
history is fresh.

## What the output measures

Successful results identify `measurement: "connection_events"` and
`attribution: "current_process_tree"`. Each pane and the overall total contain
`connection_count`. Pane results also expose `last_connection`, `pane_id`,
`window_index`, sorted process IDs and provider-tagged counts under
`providers.<provider>.connections`.

Only `connect` events count. Matching `close` events do not count a second time;
alert events and sockets with no attributed PID do not become process traffic.
Missing provider tags contribute to `unknown`. Repeated queries recompute their
window rather than accumulating the same events in NTM. Multiple observers can
record multiple observations of one physical connection: these are counts of
persisted events, not a deduplicated inventory of unique sockets.

Rano does not export HTTP request totals or byte-transfer counters. NTM does not
invent those measurements from connections. Real export results omit those
unmeasured fields and list `http_requests`, `bytes_in` and `bytes_out` in
`unavailable_metrics`. A reused HTTP/2 connection may carry many requests;
connection counts must not be used as API usage, quota or billing estimates.

## Collection and attribution

One export serves the entire fleet. It requests only `ts,event,pid,comm,provider`,
not command lines, remote addresses or domains. The existing adapter timeout and
an outer robot query budget bound execution. Stdout is limited to 10 MiB, stderr
to 64 KiB, and individual JSONL records to 64 KiB. Invalid or truncated output
invalidates the result; no partial prefix is reported as complete. Arbitrary
subprocess stderr is not copied into the response.

Windows accept positive Go durations plus whole-number `d` and `w` units. Empty
uses five minutes. Day/week multiplication is checked for overflow. The query
fixes its start and end once, asks the exporter for a slightly wider interval to
avoid fractional-second string-ordering losses, and applies the exact half-open
`[start, end)` interval after parsing timestamps.

Process attribution uses the current tmux and `/proc` process tree. Durable tmux
`%N` IDs, not mutable pane titles, join the observations. Equal titles and repeated
pane indices across windows stay separate; an agent in pane zero is included,
while user, service and dead panes are excluded. A failed tmux session capture
cannot make PID-map refresh report success. Untitled panes retain all of their
children's totals.

This is not historical process-incarnation attribution: a PID reused within the
window may be associated with its present owner, and an exited process may no
longer be attributable. Unmapped processes are omitted. The source is the default
Rano database in the caller's working directory, not automatically every
project's database. No provider-payload inspection or background polling is added.

## Verified upstream contract

The adapter follows `Dicklesworthstone/rano` at
`7f342a3d195dea97f083602274fd2758ff46c88e`, `src/main.rs`:
`ExportArgs::default`, `parse_export_args`, `EXPORT_FIELDS`, `run_export`,
`build_export_query` and `format_jsonl_row`. That exporter accepts
`export --format jsonl --fields ... --since ... --until ...`; it does not accept
`stats`, `--all`, `--window` or an export PID flag. Per-PID adapter calls filter
the same aggregate result. Rano's exporter may add optional schema columns to an
old SQLite database, so this is not a strictly read-only database operation.
