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
