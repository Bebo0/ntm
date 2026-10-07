# Incident Taxonomy and Promotion Rules

> **Authoritative definition of ntm incident classification, promotion, and lifecycle.**
> Incidents are durable escalations that give recurring problems memory and lifecycle.
> This document defines what becomes an incident, how it evolves, and when it resolves.

**Status:** RATIFIED
**Bead:** bd-j9jo3.5.1
**Created:** 2026-03-22
**Part of:** bd-j9jo3 (robot-redesign epic)
**Depends on:** projection-section-model.md (bd-j9jo3.1.2), freshness-contract.md (bd-j9jo3.1.4)

---

## 1. Purpose

Incidents exist to reduce operator cognitive load by:

1. **Remembering persistent problems** - Not just the latest symptom
2. **Aggregating related events** - One incident, not 50 alerts
3. **Tracking resolution** - Was this fixed? When? By whom?
4. **Enabling post-mortems** - What happened over time?

**Design Goal:** Make incident behavior boring and predictable. Promotion rules are explicit, not heuristic folklore.

**Anti-Goal:** Incidents becoming another noisy warning channel that operators ignore.

---

## 2. Alert vs Incident

| Aspect | Alert | Incident |
|--------|-------|----------|
| **Duration** | Momentary (event-scoped) | Durable (spans events) |
| **Persistence** | In attention feed only | Persisted to SQLite |
| **Identity** | Event cursor | Stable incident ID |
| **State** | None | open/acknowledged/resolved/dismissed |
| **Resolution** | Clears automatically | Requires explicit action |
| **History** | Lost after GC | Retained for review |

---

## 3. Incident Families

### 3.1 Agent Incidents

| Family | Fingerprint | Promotion Rule |
|--------|-------------|----------------|
| `agent.crash_loop` | session:agent:crash | 3+ crashes in 30 minutes |
| `agent.chronic_error` | session:agent:error | Error state >5 minutes |
| `agent.stuck_busy` | session:agent:stuck | Busy >30 minutes without output |
| `agent.compaction_loop` | session:agent:compact | 3+ compactions in 1 hour |
| `agent.context_critical` | session:agent:context | Context >95% for >5 minutes |

### 3.2 Session Incidents

| Family | Fingerprint | Promotion Rule |
|--------|-------------|----------------|
| `session.unhealthy` | session:health | Health score <0.5 for >10 minutes |
| `session.orphaned` | session:orphan | Detached >24 hours with errors |
| `session.all_idle` | session:idle | All agents idle >1 hour with ready work |

### 3.3 Quota Incidents

| Family | Fingerprint | Promotion Rule |
|--------|-------------|----------------|
| `quota.exceeded` | provider:exceeded | Rate limit hit |
| `quota.chronic_pressure` | provider:pressure | Usage >90% for >30 minutes |
| `quota.no_recovery` | provider:stuck | Exceeded state >2 hours |

### 3.4 Coordination Incidents

| Family | Fingerprint | Promotion Rule |
|--------|-------------|----------------|
| `coordination.conflict_unresolved` | conflict:id | File conflict >15 minutes |
| `coordination.mail_backlog` | mail:backlog | >10 unread urgent messages >1 hour |
| `coordination.reservation_deadlock` | reservation:deadlock | Circular reservation wait >10 minutes |

### 3.5 Source Incidents

| Family | Fingerprint | Promotion Rule |
|--------|-------------|----------------|
| `source.outage` | source:id | Source unavailable >5 minutes |
| `source.chronic_degraded` | source:degraded | Degraded >30 minutes |
| `source.stale_critical` | source:stale | Critical source stale >15 minutes |

### 3.6 Work Incidents

| Family | Fingerprint | Promotion Rule |
|--------|-------------|----------------|
| `work.no_progress` | project:stalled | No bead progress >4 hours with active agents |
| `work.blocked_cascade` | project:blocked | >50% of work blocked by one item |

---

## 4. Severity Levels

| Severity | Meaning | Examples |
|----------|---------|----------|
| `critical` | Immediate operator action required | Crash loop, quota exceeded, source outage |
| `error` | Significant problem, action needed soon | Chronic error, stuck busy, conflict |
| `warning` | Potential problem, monitor closely | Context pressure, quota pressure, degraded source |

Escalation: warning persisting >1h → error; error persisting >4h → critical.

---

## 5. Fingerprinting

Fingerprint structure: `family:scope:discriminator`

Examples:
- `agent.crash_loop:myproject:cc_2`
- `quota.exceeded:anthropic`
- `source.outage:mail`

Deduplication: When event matches existing open incident fingerprint, update incident; do NOT create new.

---

## 6. Incident Lifecycle

States: `open` → `acknowledged` → `resolved` (or `dismissed`)

| State | Meaning |
|-------|---------|
| `open` | Active problem, operator attention needed |
| `acknowledged` | Operator aware, working on it |
| `resolved` | Problem fixed |
| `dismissed` | False positive or not actionable |

Auto-resolution: Some incidents auto-resolve when condition clears (e.g., crash_loop clears after 30 min without crashes).

---

## 7. Incident Record

```go
type Incident struct {
    IncidentID    string   `json:"incident_id"`    // inc_<uuid>
    Fingerprint   string   `json:"fingerprint"`
    Family        string   `json:"family"`
    Category      string   `json:"category"`
    Severity      string   `json:"severity"`
    Title         string   `json:"title"`
    Description   string   `json:"description"`
    State         string   `json:"state"`
    AcknowledgedBy string  `json:"acknowledged_by,omitempty"`
    AcknowledgedAt string  `json:"acknowledged_at,omitempty"`
    ResolvedBy    string   `json:"resolved_by,omitempty"`
    ResolvedAt    string   `json:"resolved_at,omitempty"`
    Resolution    string   `json:"resolution,omitempty"`
    SessionID     string   `json:"session_id,omitempty"`
    AgentID       string   `json:"agent_id,omitempty"`
    DetectedAt    string   `json:"detected_at"`
    LastEventAt   string   `json:"last_event_at"`
    EventCount    int      `json:"event_count"`
    RelatedEvents []string `json:"related_events"`
}
```

---

## 8. Promotion Rules

When attention event arrives:
1. Check if event matches promotion rule (family, count threshold, time window)
2. Generate fingerprint
3. If existing incident with fingerprint: update it
4. If recently resolved (<1h): reopen it
5. Else: create new incident

---

## 9. Surface Integration

- **snapshot**: Full incidents array
- **status**: Incident counts only
- **digest/attention**: New incidents, escalated incidents, long-running incidents
- **--robot-inspect-incident=ID**: Full incident drill-down

---

## 10. Retention

| State | Retention |
|-------|-----------|
| `open`, `acknowledged` | Indefinite |
| `resolved`, `dismissed` | 7 days |

---

## 11. Non-Goals

- Automatic remediation (Guardian's job)
- Complex scoring (simple severity levels)
- ML-based detection (explicit rules)
- External alerting (separate concern)

---

## 12. Related Documents

- **projection-section-model.md**: IncidentSection structure
- **freshness-contract.md**: Source outage detection
- **sqlite-runtime-tables.md**: Incident persistence schema
- **attention-feed-contract.md**: Events that promote to incidents
