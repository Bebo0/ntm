package state

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/sqliteutil"
)

// Tests for the durable idempotent operation records (send #245, interrupt).

func TestClaimSendOperationLifecycle(t *testing.T) {
	store := testStore(t)

	op := &SendOperation{
		OperationID:   "op-1",
		SessionName:   "proj",
		BindingHash:   "bind-a",
		PayloadSHA256: "sha-a",
		PayloadBytes:  42,
	}

	stored, claimed, err := store.ClaimSendOperation(op)
	if err != nil {
		t.Fatalf("first claim error = %v", err)
	}
	if !claimed {
		t.Fatal("first claim not claimed; want claimed=true")
	}
	if stored.Status != SendOperationInProgress {
		t.Errorf("claimed status = %q, want in_progress", stored.Status)
	}
	if stored.BindingHash != "bind-a" || stored.PayloadSHA256 != "sha-a" || stored.PayloadBytes != 42 {
		t.Errorf("claimed record lost binding fields: %+v", stored)
	}

	// A second claim with the same ID observes the existing row (race-safe
	// duplicate detection) and does not overwrite its binding.
	dup := &SendOperation{
		OperationID:   "op-1",
		SessionName:   "proj",
		BindingHash:   "bind-DIFFERENT",
		PayloadSHA256: "sha-DIFFERENT",
		PayloadBytes:  7,
	}
	existing, claimedAgain, err := store.ClaimSendOperation(dup)
	if err != nil {
		t.Fatalf("duplicate claim error = %v", err)
	}
	if claimedAgain {
		t.Fatal("duplicate claim reported claimed=true; want false")
	}
	if existing.BindingHash != "bind-a" {
		t.Errorf("duplicate claim overwrote binding: %+v", existing)
	}

	// Completion records the outcome durably.
	if err := store.CompleteSendOperation("op-1", "proj", `{"success":true}`, time.Now()); err != nil {
		t.Fatalf("complete error = %v", err)
	}
	got, err := store.GetSendOperation("op-1", "proj")
	if err != nil {
		t.Fatalf("get error = %v", err)
	}
	if got.Status != SendOperationCompleted || got.OutcomeJSON != `{"success":true}` {
		t.Errorf("completed record = %+v, want completed with stored outcome", got)
	}
	if got.CompletedAt == nil {
		t.Error("completed record missing completed_at")
	}

	// Completing again is a no-op (status guard), preserving the original outcome.
	if err := store.CompleteSendOperation("op-1", "proj", `{"success":false}`, time.Now()); err != nil {
		t.Fatalf("re-complete error = %v", err)
	}
	got2, _ := store.GetSendOperation("op-1", "proj")
	if got2.OutcomeJSON != `{"success":true}` {
		t.Errorf("re-complete overwrote outcome: %q", got2.OutcomeJSON)
	}
}

// Operation kinds share one per-session operation-ID namespace: the kind is
// recorded on claim, an empty kind claims as a send, and a later claim of the
// same ID under a different kind observes (never overwrites) the original
// row so the caller can reject it as a conflict.
func TestClaimSendOperationRecordsKind(t *testing.T) {
	store := testStore(t)

	stored, claimed, err := store.ClaimSendOperation(&SendOperation{
		OperationID: "op-int", SessionName: "proj", Kind: OperationKindInterrupt, BindingHash: "bind-int",
	})
	if err != nil || !claimed {
		t.Fatalf("interrupt claim = (claimed=%v, err=%v), want fresh claim", claimed, err)
	}
	if stored.Kind != OperationKindInterrupt {
		t.Fatalf("claimed kind = %q, want %q", stored.Kind, OperationKindInterrupt)
	}

	defaulted, claimed, err := store.ClaimSendOperation(&SendOperation{
		OperationID: "op-legacy", SessionName: "proj", BindingHash: "bind-send",
	})
	if err != nil || !claimed {
		t.Fatalf("kindless claim = (claimed=%v, err=%v), want fresh claim", claimed, err)
	}
	if defaulted.Kind != OperationKindSend {
		t.Fatalf("kindless claim kind = %q, want %q", defaulted.Kind, OperationKindSend)
	}

	existing, claimedAgain, err := store.ClaimSendOperation(&SendOperation{
		OperationID: "op-int", SessionName: "proj", Kind: OperationKindSend, BindingHash: "bind-send",
	})
	if err != nil {
		t.Fatalf("cross-kind claim error = %v", err)
	}
	if claimedAgain {
		t.Fatal("cross-kind claim reported claimed=true; operation IDs must share one namespace per session")
	}
	if existing.Kind != OperationKindInterrupt || existing.BindingHash != "bind-int" {
		t.Fatalf("cross-kind claim overwrote the original row: %+v", existing)
	}

	ops, err := store.GetSendOperationsByID("op-int")
	if err != nil || len(ops) != 1 || ops[0].Kind != OperationKindInterrupt {
		t.Fatalf("GetSendOperationsByID(op-int) = (%+v, %v), want one interrupt row", ops, err)
	}
}

// A database migrated before kinds existed keeps its send rows readable and
// replayable: migration 025 backfills kind='send' and leaves the
// (operation_id, session_name) key the idempotency invariant depends on.
func TestSendOperationsKindMigrationBackfillsLegacyRows(t *testing.T) {
	db, err := sql.Open(sqliteutil.DriverName, sqliteutil.MemoryDSN("foreign_keys(1)"))
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = db.Close() })

	files, err := GetMigrationFiles()
	if err != nil {
		t.Fatalf("GetMigrationFiles: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE _migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatalf("create _migrations: %v", err)
	}
	// Bring the schema to the last pre-kind version exactly as an older ntm
	// would have left it.
	for _, filename := range files {
		var version int
		if _, err := fmt.Sscanf(filename, "%03d_", &version); err != nil {
			t.Fatalf("parse migration version %s: %v", filename, err)
		}
		if version >= 25 {
			break
		}
		content, err := ReadMigration(filename)
		if err != nil {
			t.Fatalf("read %s: %v", filename, err)
		}
		if _, err := db.Exec(content); err != nil {
			t.Fatalf("apply %s: %v", filename, err)
		}
		if _, err := db.Exec(`INSERT INTO _migrations (version, name) VALUES (?, ?)`, version, filename); err != nil {
			t.Fatalf("record %s: %v", filename, err)
		}
	}
	if _, err := db.Exec(`
		INSERT INTO send_operations (
			operation_id, session_name, binding_hash, payload_sha256,
			payload_bytes, status, outcome_json, created_at, completed_at
		) VALUES ('op-pre', 'proj', 'bind-pre', 'sha-pre', 3, ?, '{"success":true}', ?, ?)`,
		SendOperationCompleted, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatalf("seed legacy send row: %v", err)
	}

	if err := ApplyMigrations(db); err != nil {
		t.Fatalf("ApplyMigrations upgrade: %v", err)
	}
	store := &Store{db: db, path: ":memory:"}
	got, err := store.GetSendOperation("op-pre", "proj")
	if err != nil {
		t.Fatalf("GetSendOperation after upgrade: %v", err)
	}
	if got == nil || got.Kind != OperationKindSend || got.BindingHash != "bind-pre" || got.Status != SendOperationCompleted {
		t.Fatalf("legacy row after upgrade = %+v, want completed send row with original binding", got)
	}

	var tableSQL string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'send_operations'`).Scan(&tableSQL); err != nil {
		t.Fatalf("read send_operations schema: %v", err)
	}
	if !strings.Contains(tableSQL, "PRIMARY KEY (operation_id, session_name)") {
		t.Fatalf("send_operations lost its (operation_id, session_name) key after upgrade:\n%s", tableSQL)
	}
}

func TestGetSendOperationUnknownID(t *testing.T) {
	store := testStore(t)
	got, err := store.GetSendOperation("missing", "proj")
	if err != nil {
		t.Fatalf("get unknown error = %v", err)
	}
	if got != nil {
		t.Errorf("get unknown = %+v, want nil", got)
	}
}

func TestClaimSendOperationRequiresID(t *testing.T) {
	store := testStore(t)
	if _, _, err := store.ClaimSendOperation(&SendOperation{SessionName: "proj"}); err == nil {
		t.Fatal("claim without ID succeeded; want error")
	}
	if _, _, err := store.ClaimSendOperation(&SendOperation{OperationID: "op"}); err == nil {
		t.Fatal("claim without session succeeded; want error")
	}
}

// Sessions form independent operation-ID namespaces: the same ID claimed in
// two sessions creates two rows rather than colliding.
func TestClaimSendOperationSessionScoped(t *testing.T) {
	store := testStore(t)

	_, claimedA, err := store.ClaimSendOperation(&SendOperation{
		OperationID: "step-1", SessionName: "projA", BindingHash: "bind-a",
	})
	if err != nil || !claimedA {
		t.Fatalf("session A claim = (claimed=%v, err=%v), want fresh claim", claimedA, err)
	}
	_, claimedB, err := store.ClaimSendOperation(&SendOperation{
		OperationID: "step-1", SessionName: "projB", BindingHash: "bind-b",
	})
	if err != nil || !claimedB {
		t.Fatalf("session B claim = (claimed=%v, err=%v), want independent fresh claim", claimedB, err)
	}

	ops, err := store.GetSendOperationsByID("step-1")
	if err != nil {
		t.Fatalf("get by ID error = %v", err)
	}
	if len(ops) != 2 {
		t.Fatalf("rows for step-1 = %d, want 2", len(ops))
	}
}

func TestReleaseSendOperation(t *testing.T) {
	store := testStore(t)
	op := &SendOperation{OperationID: "op-r", SessionName: "proj", BindingHash: "b"}
	if _, _, err := store.ClaimSendOperation(op); err != nil {
		t.Fatalf("claim error = %v", err)
	}

	// Release frees the ID for a fresh claim.
	if err := store.ReleaseSendOperation("op-r", "proj"); err != nil {
		t.Fatalf("release error = %v", err)
	}
	_, claimed, err := store.ClaimSendOperation(op)
	if err != nil || !claimed {
		t.Fatalf("re-claim after release = (claimed=%v, err=%v), want fresh claim", claimed, err)
	}

	// A completed row is never released.
	if err := store.CompleteSendOperation("op-r", "proj", `{"success":true}`, time.Now()); err != nil {
		t.Fatalf("complete error = %v", err)
	}
	if err := store.ReleaseSendOperation("op-r", "proj"); err != nil {
		t.Fatalf("release completed error = %v", err)
	}
	got, _ := store.GetSendOperation("op-r", "proj")
	if got == nil || got.Status != SendOperationCompleted {
		t.Fatalf("completed row after release attempt = %+v, want intact", got)
	}
}

func TestTakeOverStaleSendOperation(t *testing.T) {
	store := testStore(t)
	stale := time.Now().UTC().Add(-time.Hour)
	if _, _, err := store.ClaimSendOperation(&SendOperation{
		OperationID: "op-t", SessionName: "proj", BindingHash: "b", CreatedAt: stale,
	}); err != nil {
		t.Fatalf("claim error = %v", err)
	}

	// A fresh claim (created now) must not be usurped.
	won, err := store.TakeOverStaleSendOperation("op-t", "proj", "b", time.Now().UTC().Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("takeover error = %v", err)
	}
	if won {
		t.Fatal("takeover won against a claim inside the staleness window")
	}

	// A stale claim with matching binding is taken over.
	won, err = store.TakeOverStaleSendOperation("op-t", "proj", "b", time.Now().UTC().Add(-time.Minute))
	if err != nil || !won {
		t.Fatalf("stale takeover = (won=%v, err=%v), want success", won, err)
	}

	// Mismatched binding never takes over.
	won, err = store.TakeOverStaleSendOperation("op-t", "proj", "OTHER", time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("mismatched takeover error = %v", err)
	}
	if won {
		t.Fatal("takeover won with a mismatched binding hash")
	}
}

func TestGCCompletedSendOperations(t *testing.T) {
	store := testStore(t)
	if _, _, err := store.ClaimSendOperation(&SendOperation{
		OperationID: "op-old", SessionName: "proj", BindingHash: "b",
	}); err != nil {
		t.Fatalf("claim error = %v", err)
	}
	if err := store.CompleteSendOperation("op-old", "proj", `{}`, time.Now().UTC().Add(-30*24*time.Hour)); err != nil {
		t.Fatalf("complete error = %v", err)
	}
	// An in_progress row is never GC'd regardless of age.
	if _, _, err := store.ClaimSendOperation(&SendOperation{
		OperationID: "op-live", SessionName: "proj", BindingHash: "b",
		CreatedAt: time.Now().UTC().Add(-60 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("claim error = %v", err)
	}

	pruned, err := store.GCCompletedSendOperations(0)
	if err != nil {
		t.Fatalf("gc error = %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1", pruned)
	}
	if got, _ := store.GetSendOperation("op-old", "proj"); got != nil {
		t.Error("old completed row survived GC")
	}
	if got, _ := store.GetSendOperation("op-live", "proj"); got == nil {
		t.Error("in_progress row was GC'd")
	}
}

func TestGCStaleOutputSeqWatermarks(t *testing.T) {
	store := testStore(t)
	old := time.Now().UTC().Add(-60 * 24 * time.Hour)
	fresh := time.Now().UTC()

	for _, wm := range []*OutputWatermark{
		{WatermarkType: "output_seq", Scope: "dead|%1", Consumer: "e1", CreatedAt: old, UpdatedAt: old},
		{WatermarkType: "output_seq", Scope: "live|%2", Consumer: "e2", CreatedAt: old, UpdatedAt: fresh},
		// Velocity baselines keep their pre-existing lifecycle: never GC'd here.
		{WatermarkType: "velocity", Scope: "dead|%1", CreatedAt: old, UpdatedAt: old},
	} {
		if err := store.SetWatermark(wm); err != nil {
			t.Fatalf("seed watermark %s/%s: %v", wm.WatermarkType, wm.Scope, err)
		}
	}

	pruned, err := store.GCStaleOutputSeqWatermarks(0)
	if err != nil {
		t.Fatalf("gc error = %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1", pruned)
	}
	if wm, _ := store.GetWatermark("output_seq", "dead|%1"); wm != nil {
		t.Error("stale output_seq row survived GC")
	}
	if wm, _ := store.GetWatermark("output_seq", "live|%2"); wm == nil {
		t.Error("recently-observed output_seq row was pruned")
	}
	if wm, _ := store.GetWatermark("velocity", "dead|%1"); wm == nil {
		t.Error("velocity watermark was pruned by output_seq GC")
	}
}
