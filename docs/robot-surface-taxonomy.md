# Robot Surface Taxonomy

> **Authoritative reference** for ntm robot mode surfaces and lane responsibilities.
> All robot commands MUST be justified against this taxonomy.

**Status:** AUTHORITATIVE
**Bead:** bd-j9jo3.1.1
**Created:** 2026-03-22

---

## 1. Purpose

This document defines which robot surfaces exist, what each surface owns, what each surface must NOT do, and how surfaces compose into the canonical operator loop. Future implementation work MUST cite this taxonomy rather than restate intent.

**Design Goal:** Fewer stronger surfaces, better shared data, better restart safety, clearer degraded-state semantics.

**Anti-Goal:** Command sprawl via overlapping surfaces with implicit responsibilities.

---

## 2. The Canonical Operator Loop

The operator loop is the fundamental interaction pattern for AI agents consuming ntm's robot API:

```
1. BOOTSTRAP    snapshot    → establish baseline state, get cursor
2. SUMMARIZE    status      → cheap high-level view (sessions, agent counts)
3. REPLAY       events      → raw event stream since cursor
4. TRIAGE       attention   → prioritized what-needs-attention items
5. INSPECT      tail/inspect/diagnose → drill down on specific items
6. ACT          send/spawn/interrupt  → take action
7. WAIT         wait        → block until condition met
8. REPEAT       → goto REPLAY (or BOOTSTRAP on cursor expiry)
```

Each lane has exactly one canonical surface. Surfaces outside this loop are either **inspection extensions** or **utility bridges**.

---

## 3. Surface Lanes

### 3.1 Bootstrap Lane

**Canonical Surface:** `--robot-snapshot`

**Job:**
Establish complete baseline state for an operator that has no prior context. Returns everything an operator needs to reason about the system: sessions, agents, beads, alerts, mail, and an initial cursor for subsequent event polling.

**Must:**
- Return a cursor that can be used with `--robot-events`
- Include full state for all major subsystems
- Include source freshness and degraded-state markers
- Be the single authoritative cold-start entry point

**Must NOT:**
- Require prior state to interpret
- Return incremental deltas (that's events/digest)
- Be cheap enough for frequent polling (that's status)

**When to call:** Session start, context loss, CURSOR_EXPIRED recovery.

---

### 3.2 Summarize Lane

**Canonical Surface:** `--robot-status`

**Job:**
Provide a cheap high-level view suitable for frequent polling. Returns session list, agent counts, and basic health indicators. Does NOT return full state.

**Must:**
- Be fast (<100ms target)
- Return minimal but sufficient data for "is anything obviously wrong"
- Support pagination for large session counts
- Be idempotent and side-effect free

**Must NOT:**
- Return full bead state (use snapshot)
- Return full mail state (use snapshot or mail)
- Return event cursors (use snapshot)
- Duplicate bootstrap responsibilities

**When to call:** Heartbeat polling, quick health check.

---

### 3.3 Replay Lane

**Canonical Surface:** `--robot-events`

**Job:**
Return raw events since a given cursor. This is the primitive event stream for operators that want full replay fidelity.

**Must:**
- Accept a cursor and return events with cursor > input
- Return events in monotonic cursor order
- Include cursor in each event for resumability
- Support category/severity/actionability filters
- Return `CURSOR_EXPIRED` when cursor is too old

**Must NOT:**
- Summarize or aggregate (that's digest)
- Prioritize or rank (that's attention)
- Block waiting for events (use wait or attention)

**When to call:** Log replay, audit trail, raw event processing.

---

### 3.4 Triage Lane

**Canonical Surfaces:** `--robot-attention`, `--robot-digest`

#### 3.4.1 `--robot-attention`

**Job:**
The ONE OBVIOUS TENDING PRIMITIVE. Wait for attention-worthy events, then return a prioritized digest. This is the blocking version suitable for operator loops that want to sleep until something needs attention.

**Must:**
- Block until attention-worthy events exist (or timeout)
- Return prioritized items ranked by urgency
- Include actionable `next_actions` for each item
- Return cursor for resumption

**Must NOT:**
- Return immediately if nothing needs attention (that's digest)
- Return raw events without prioritization (that's events)

**When to call:** Main operator loop when waiting for work.

#### 3.4.2 `--robot-digest`

**Job:**
Non-blocking attention summary. Returns what-changed counts and top items without waiting. Token-efficient alternative to attention for tight loops.

**Must:**
- Return immediately (non-blocking)
- Include event counts by category/actionability
- Include top-N attention items
- Be cheap enough for frequent polling

**Must NOT:**
- Block waiting for events (that's attention)
- Return full event payloads (that's events)

**When to call:** Quick check between actions, dashboard refresh.

---

### 3.5 Inspect Lane

**Canonical Surfaces:**
- `--robot-tail` (pane output)
- `--robot-inspect-pane` (detailed pane state)
- `--robot-diagnose` (health analysis with recommendations)
- `--robot-context` (context window usage)
- `--robot-activity` (agent idle/busy/error states)
- `--robot-files` (file changes with attribution)
- `--robot-diff` (activity comparison over time)

**Job:**
Answer focused follow-up questions about specific entities. Operators use inspect surfaces after triage surfaces identify items needing attention.

**Must:**
- Accept scope parameters (session, pane, entity ID)
- Return detailed data for the scoped entity
- Support actionable output (line numbers, commands)

**Must NOT:**
- Return global state (use snapshot/status)
- Prioritize across entities (that's triage)
- Take actions (that's the act lane)

**When to call:** After triage identifies something needing attention.

---

### 3.6 Act Lane

**Canonical Surfaces:**
- `--robot-send` (send prompts to agents)
- `--robot-spawn` (create sessions with agents)
- `--robot-interrupt` (send Ctrl+C, optionally with new task)
- `--robot-assign` (assign beads to agents)
- `--robot-route` (route prompts to best available agent)
- `--robot-restart-pane` (restart crashed agent)
- `--robot-overlay` (open dashboard for human handoff)

**Job:**
Execute operator decisions. All state changes flow through explicit actuation commands.

**Must:**
- Require explicit invocation (no auto-actuation from inspection)
- Support `--dry-run` for preview
- Return result with success/failure and details
- Be idempotent where possible (spawn with existing session name)

**Must NOT:**
- Embed planning heuristics
- Auto-select targets without explicit parameters
- Conflate inspection with action

**When to call:** After operator decides what to do based on triage/inspect.

---

### 3.7 Wait Lane

**Canonical Surface:** `--robot-wait`

**Job:**
Block until a named condition is met. Provides synchronization primitive for operator loops.

**Must:**
- Accept enumerated condition names (no free-form DSL)
- Return when condition met OR timeout
- Include triggering event in response
- Return cursor for resumption

**Must NOT:**
- Execute actions on condition (operator decides)
- Support arbitrary predicates (conditions are enumerated)

**Conditions (exhaustive):**
`idle`, `any_idle`, `busy`, `all_busy`, `any_output`, `any_error`, `action_required`, `bead_ready`, `mail_received`

**When to call:** After actuation, waiting for result.

---

## 4. Extension Surfaces

These surfaces extend the core operator loop with domain-specific capabilities.

### 4.1 Beads Surfaces

| Surface | Job |
|---------|-----|
| `--robot-beads-list` | List beads with status/priority filters |
| `--robot-bead-show` | Show single bead details |
| `--robot-bead-claim` | Mark bead as in_progress |
| `--robot-bead-close` | Close completed bead |
| `--robot-bead-create` | Create new bead |

**Lane:** Beads surfaces are INSPECTION for list/show, ACTUATION for claim/close/create.

### 4.2 BV (Graph Analysis) Surfaces

| Surface | Job |
|---------|-----|
| `--robot-triage` | BV triage with recommendations (meta-triage over beads) |
| `--robot-plan` | Execution plan with parallel tracks |
| `--robot-graph` | Dependency graph metrics |
| `--robot-suggest` | Hygiene suggestions |
| `--robot-forecast` | ETA predictions |
| `--robot-impact` | File impact analysis |
| `--robot-label-*` | Label-scoped analysis |
| `--robot-file-*` | File-scoped analysis |

**Lane:** All BV surfaces are TRIAGE extensions. They help operators decide what to work on.

### 4.3 CASS (Cross-Agent Search) Surfaces

| Surface | Job |
|---------|-----|
| `--robot-cass-search` | Search past conversations |
| `--robot-cass-context` | Get relevant past context |
| `--robot-cass-status` | CASS health check |

**Lane:** CASS surfaces are INSPECTION extensions for historical data.

### 4.4 Ensemble Surfaces

| Surface | Job |
|---------|-----|
| `--robot-ensemble` | Get ensemble state |
| `--robot-ensemble-spawn` | Create multi-agent ensemble |
| `--robot-ensemble-*` | Ensemble management |

**Lane:** Ensemble surfaces are ACTUATION extensions for multi-agent orchestration.

### 4.5 Pipeline Surfaces

| Surface | Job |
|---------|-----|
| `--robot-pipeline-run` | Execute named workflow |
| `--robot-pipeline` | Get workflow status |
| `--robot-pipeline-list` | List available workflows |
| `--robot-pipeline-cancel` | Cancel running workflow |

**Lane:** Pipeline surfaces are ACTUATION extensions for workflow execution.

---

## 5. Utility Surfaces

Utility surfaces are tool bridges and helpers. They do NOT participate in the core operator loop.

### 5.1 Discovery

| Surface | Job |
|---------|-----|
| `--robot-help` | AI agent integration guide |
| `--robot-capabilities` | Machine-discoverable API |
| `--robot-version` | Version and build info |
| `--robot-tools` | Tool inventory |
| `--robot-recipes` | Spawn presets |

### 5.2 Tool Bridges

| Surface | Tool |
|---------|------|
| `--robot-jfp-*` | JeffreysPrompts |
| `--robot-cass-*` | CASS |
| `--robot-ms-*` | Meta Skill |
| `--robot-dcg-*` | Destructive Command Guard |
| `--robot-slb-*` | Two-person approvals |
| `--robot-acfs-*` | Flywheel setup |
| `--robot-giil-*` | Image fetch |
| `--robot-ru-*` | Repo updater |
| `--robot-rch-*` | Remote compilation |

### 5.3 Persistence

| Surface | Job |
|---------|-----|
| `--robot-save` | Save session state |
| `--robot-restore` | Restore session state |
| `--robot-replay` | Replay saved state |

### 5.4 Formatting

| Surface | Job |
|---------|-----|
| `--robot-terse` | Minimal single-line state |
| `--robot-markdown` | State as markdown tables |
| `--robot-dashboard` | Dashboard as markdown |

---

## 6. Non-Goals

This taxonomy explicitly rejects:

1. **DuckDB or analytical store** - SQLite only. ntm is tmux-first, not fleet-analytics-first.

2. **Fleet-first worldview** - ntm manages named sessions in a single tmux instance. Cross-machine fleet management is out of scope.

3. **Reflexive new overview command** - Do not create `--robot-overview` or similar. Snapshot IS the bootstrap surface. Status IS the cheap summary. If snapshot is weak, fix snapshot.

4. **Command sprawl** - Every new surface must be justified against this taxonomy. If a surface cannot be assigned to exactly one lane, it should probably be a mode of an existing surface.

5. **Hidden planner behavior** - Actuation flows through explicit commands. No surface should auto-execute actions based on inspection results.

6. **Ambiguous command-local state dumps** - Surfaces should project from a shared model. Multiple surfaces returning overlapping but inconsistent state views is forbidden.

---

## 7. Schema and Freshness

All major surfaces MUST include:

### 7.1 Schema Identification

```json
{
  "schema_version": "1.0.0",
  "schema_id": "ntm:snapshot:v1"
}
```

### 7.2 Source Freshness

```json
{
  "sources": {
    "sessions": {"fresh": true, "age_ms": 42},
    "beads": {"fresh": true, "age_ms": 100},
    "mail": {"fresh": false, "degraded": true, "reason": "server unreachable"}
  }
}
```

### 7.3 Degraded-State Markers

When a source is degraded, affected sections include:

```json
{
  "agent_mail": {
    "_degraded": true,
    "_degraded_reason": "Agent Mail server unreachable",
    "_degraded_since": "2026-03-22T02:00:00Z",
    "unread": null
  }
}
```

---

## 8. Surface Justification Template

When proposing a new robot surface, answer:

1. **Lane:** Which lane does this belong to? (bootstrap/summarize/replay/triage/inspect/act/wait/extension/utility)

2. **Job:** What is the single responsibility?

3. **Existing alternative:** Why can't an existing surface serve this need?

4. **Overlap risk:** What surfaces could this overlap with? How is overlap prevented?

5. **Shared model:** Does this project from the shared runtime model, or does it assemble state locally?

6. **Degraded behavior:** How does this surface behave when sources are degraded?

If these questions cannot be answered clearly, the surface should not be added.

---

## 9. Command Count by Lane

| Lane | Commands | Purpose |
|------|----------|---------|
| Bootstrap | 1 | snapshot |
| Summarize | 1 | status |
| Replay | 1 | events |
| Triage | 2 | attention, digest |
| Inspect | 7 | tail, inspect-pane, diagnose, context, activity, files, diff |
| Act | 7 | send, spawn, interrupt, assign, route, restart-pane, overlay |
| Wait | 1 | wait |
| Beads | 5 | list, show, claim, close, create |
| BV | 12 | triage, plan, graph, suggest, forecast, impact, search, label-*, file-* |
| CASS | 3 | search, context, status |
| Ensemble | 5 | state, spawn, modes, presets, suggest |
| Pipeline | 4 | run, status, list, cancel |
| Utility | ~40 | discovery, tool bridges, persistence, formatting |

**Total:** ~89 purpose-justified commands (excludes deprecated/alias flags)

---

## 10. Appendix: Lane Diagram

```
                    BOOTSTRAP
                        │
                        ▼
                    snapshot
                        │
          ┌─────────────┴─────────────┐
          │                           │
          ▼                           ▼
      SUMMARIZE                    REPLAY
          │                           │
          ▼                           ▼
       status                      events
          │                           │
          └─────────────┬─────────────┘
                        │
                        ▼
                     TRIAGE
                        │
            ┌───────────┴───────────┐
            │                       │
            ▼                       ▼
       attention                 digest
            │                       │
            └───────────┬───────────┘
                        │
                        ▼
                     INSPECT
                        │
    ┌───────┬───────┬───┴───┬───────┬───────┐
    ▼       ▼       ▼       ▼       ▼       ▼
  tail  inspect diagnose context activity files
                        │
                        ▼
                       ACT
                        │
    ┌───────┬───────┬───┴───┬───────┬───────┐
    ▼       ▼       ▼       ▼       ▼       ▼
  send   spawn interrupt assign  route  overlay
                        │
                        ▼
                      WAIT
                        │
                        ▼
                      wait
                        │
                        ▼
               ┌────────┴────────┐
               │                 │
               ▼                 ▼
         (back to REPLAY)   (back to BOOTSTRAP
                             on CURSOR_EXPIRED)
```

---

## Appendix: Changelog

- **2026-03-22:** Initial taxonomy (bd-j9jo3.1.1)

---

*Reference: bd-j9jo3.1.1*
