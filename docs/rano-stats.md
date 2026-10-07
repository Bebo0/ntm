# Rano connection observations

`ntm --robot-rano-stats`, the dashboard's Network Activity panel and the
process-triage monitor's network signal all read rano's recorded connection
history through rano's supported JSONL export. rano has no `stats` subcommand
(its CLI dispatches only `update`, `report`, `export`, `config`, `diff` and
`status`; monitoring is the bare `rano [--pid ...] [--sqlite PATH]` loop), so
the earlier `rano stats --all --json` call could never succeed (`bd-r11uk`).

## Point ntm at rano's database

rano's monitor writes every connect/close event to an SQLite database. Its
default is `observer.sqlite` in the monitor's working directory; `rano --sqlite
PATH` or `sqlite=PATH` in `~/.config/rano/config.conf` moves it. ntm never
starts the monitor, so tell ntm where that database is:

```toml
[integrations.rano]
sqlite_path = "~/.local/share/rano/observer.sqlite"  # default: "observer.sqlite"
```

Relative paths resolve against ntm's working directory (rano's own rule); a
leading `~` is expanded. Then:

```sh
rano --sqlite ~/.local/share/rano/observer.sqlite   # in its own pane or service
ntm --robot-rano-stats --rano-window=5m
ntm --robot-rano-stats --rano-window=1h --panes=0,1
```

If no database exists at that path, rano has recorded nothing ntm can read.
`--robot-rano-stats` then fails with `DEPENDENCY_MISSING` (exit 1), naming the
path in `error` and `database`, and the exporter is not run. The dashboard panel
shows the same reason as "Network stats unavailable". Neither reports a missing
source as zero traffic. A failed export, capture-limit overflow or malformed
export is likewise a failure, never a partial result.

## What the output measures

Successful results identify `measurement: "connection_events"`,
`attribution: "current_process_tree"` and the `database` that was read. Each
pane and the overall `total` contain `connection_count`. Pane results also
expose `last_connection`, `pane_id`, `window_index`, sorted process IDs and
counts by rano's own per-connection provider tag under
`providers.<provider>.connections`.

Only `connect` events count. Matching `close` events do not count a second time;
alert events and sockets with no attributed PID do not become process traffic.
Missing provider tags count as `unknown`. Repeated queries recompute their
window rather than accumulating the same events in ntm. Multiple observers can
record one physical connection more than once: these are counts of persisted
events, not a deduplicated inventory of sockets.

rano records sockets, not HTTP requests or transferred bytes, so neither exists
in the output; `unavailable_metrics` lists `http_requests`, `bytes_in` and
`bytes_out`. A reused HTTP/2 connection may carry many requests: connection
counts are not API usage, quota or billing estimates.

The dashboard panel shows the same evidence for the dashboard's session over
the last five minutes: connections per agent pane, the age of the last
connection, an activity indicator and (on taller panels) the total and the
per-provider breakdown. It uses the same pane attribution as the robot command.

## Collection and attribution

One export serves the whole fleet:

```sh
rano export --format jsonl --sqlite DB --fields ts,event,pid,comm,provider \
  --since START --until END
```

It requests no command lines, remote addresses or domains. The adapter timeout
and an outer robot query budget bound execution. Stdout is limited to 10 MiB,
stderr to 64 KiB and individual JSONL records to 64 KiB (writer-only capture
wrappers keep `io.Copy` from bypassing the caps). Arbitrary subprocess stderr is
not copied into the response.

Windows accept positive Go durations plus whole-number `d` and `w` units; empty
means five minutes, and day/week multiplication is overflow-checked. The query
fixes its start and end once, asks the exporter for a slightly wider interval to
avoid whole/fractional-second string-ordering losses, and applies the exact
half-open `[start, end)` interval after parsing timestamps.

Process attribution uses the current tmux and `/proc` process tree. Durable tmux
`%N` IDs, not mutable pane titles, join the observations. Equal titles and
repeated pane indices across windows stay separate; an agent in pane zero is
included, while user, service and dead panes are excluded. This is not
historical process-incarnation attribution: a PID reused within the window may
be associated with its present owner, and an exited process may no longer be
attributable. Unmapped processes are omitted.

rano's exporter may add optional schema columns to an old database, so reading
is not a strictly read-only SQLite operation.

## Verified upstream contract

`Dicklesworthstone/rano` at `7f342a3d195dea97f083602274fd2758ff46c88e` (rano
0.2.1), `src/main.rs`: `parse_cli`, `ExportArgs::default`, `parse_export_args`,
`EXPORT_FIELDS`, `run_export`, `build_export_query`, `format_jsonl_row`
(alphabetical keys, NULL columns omitted), `system_time_to_rfc3339`
(whole-second UTC `Z` stamps), `synthesized_alert_event` (alert rows have no
pid) and `parse_status_args` (`status` has no `--json`). The export has no PID
flag; per-PID lookups filter the aggregate result.
