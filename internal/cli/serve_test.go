package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/state"
)

// TestServeStateMaintenanceCollectsStaleRuntimeRows: the state store's GC had
// no caller, so its tables only grew (bd-a25g6). serve's maintenance loop must
// prune a row whose staleness window passed while keeping a fresh one.
func TestServeStateMaintenanceCollectsStaleRuntimeRows(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate state store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	now := time.Now().UTC()
	for _, sess := range []*state.RuntimeSession{
		{Name: "gone", CollectedAt: now.Add(-time.Hour), StaleAfter: now.Add(-30 * time.Minute)},
		{Name: "live", CollectedAt: now, StaleAfter: now.Add(time.Minute)},
	} {
		if err := store.UpsertRuntimeSession(sess); err != nil {
			t.Fatalf("seed runtime session %s: %v", sess.Name, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveStateMaintenance(ctx, store, time.Hour, time.Hour)
	}()
	t.Cleanup(func() { cancel(); <-done })

	deadline := time.Now().Add(5 * time.Second)
	for {
		gone, err := store.GetRuntimeSession("gone")
		if err != nil {
			t.Fatalf("read runtime session: %v", err)
		}
		if gone == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("serve maintenance never collected the stale runtime session")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if live, err := store.GetRuntimeSession("live"); err != nil || live == nil {
		t.Fatalf("fresh runtime session was collected (session=%v, err=%v)", live, err)
	}
}

// TestCollectStateGarbageOccasionallyThrottlesCLIPasses: hosts that never run
// serve must still collect (bd-7dhqw), but robot commands run constantly, so
// a pass happens only when the stamp next to the DB is older than the interval.
func TestCollectStateGarbageOccasionallyThrottlesCLIPasses(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate state store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	seedStale := func(name string) {
		t.Helper()
		now := time.Now().UTC()
		if err := store.UpsertRuntimeSession(&state.RuntimeSession{Name: name, CollectedAt: now.Add(-time.Hour), StaleAfter: now.Add(-30 * time.Minute)}); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	present := func(name string) bool {
		t.Helper()
		sess, err := store.GetRuntimeSession(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return sess != nil
	}
	stamp := filepath.Join(dir, "state.db.gc")
	now := time.Now()

	seedStale("first")
	if !collectStateGarbageOccasionally(store, stamp, time.Hour, now) || present("first") {
		t.Fatal("first pass with no stamp must collect the stale session")
	}

	seedStale("second")
	if collectStateGarbageOccasionally(store, stamp, time.Hour, now.Add(30*time.Minute)) || !present("second") {
		t.Fatal("a pass inside the interval must be skipped")
	}
	if !collectStateGarbageOccasionally(store, stamp, time.Hour, now.Add(61*time.Minute)) || present("second") {
		t.Fatal("a pass after the interval must collect again")
	}
}

func TestServeCmdRejectsUnexpectedArguments(t *testing.T) {
	cmd := newServeCmd()
	cmd.SetArgs([]string{"unexpected"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("serve accepted an unexpected positional argument")
	}
	// serve declares cobra.NoArgs, whose message is `unknown command %q for %q`.
	// "accepts 0 arg(s)" is ExactArgs(0)'s wording and never applied here; the
	// contract under test is that the positional is rejected at all.
	if !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("error = %q, want Cobra no-arguments error", err)
	}
}
