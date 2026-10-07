package cli

import (
	"reflect"
	"testing"

	"github.com/Dicklesworthstone/ntm/internal/agentmail"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

// Liveness derivation and identity publish keys moved with the coordinator
// to internal/spawnidentity (coordinator_test.go there).

func TestNextPaneIndices_CountsRetitledLivePaneViaRegistry(t *testing.T) {
	// ntm#256 reproduction: pane %5 was spawned as sess__cc_1 and then
	// retitled. Title parsing alone yields no cc index; the registry knows
	// %5 holds sess__cc_1 and %5 is live, so the next cc index must be 2.
	panes := []tmux.Pane{
		{ID: "%4", PID: 1, Title: "sess__user_0"},
		{ID: "%5", PID: 4242, Title: "my custom title"},
		{ID: "%6", PID: 6, Title: "sess__cod_1"},
	}
	registry := agentmail.NewSessionAgentRegistry("sess", "/proj")
	registry.AddAgent("sess__cc_1", "%5", "GreenCastle")
	registry.SetPanePID("%5", 4242)

	got := nextPaneIndices(panes, registry)
	if got["cc"] != 1 {
		t.Fatalf("cc max index = %d, want 1 (occupied by retitled live pane)", got["cc"])
	}
	if got["cod"] != 1 {
		t.Fatalf("cod max index = %d, want 1 (from live title)", got["cod"])
	}
}

func TestNextPaneIndices_IgnoresDeadRegistryEntries(t *testing.T) {
	// A registry entry whose pane is gone (or re-incarnated with a new pid)
	// must not reserve a slot: same-session respawn keeps its low numbers.
	panes := []tmux.Pane{
		{ID: "%8", PID: 8, Title: "sess__user_0"},
		{ID: "%5", PID: 777, Title: "something else"}, // %5 reused by a new process
	}
	registry := agentmail.NewSessionAgentRegistry("sess", "/proj")
	registry.AddAgent("sess__cc_3", "%5", "GreenCastle")
	registry.SetPanePID("%5", 4242)
	registry.AddAgent("sess__cod_7", "%2", "BlueLake") // pane absent

	got := nextPaneIndices(panes, registry)
	if got["cc"] != 0 || got["cod"] != 0 {
		t.Fatalf("dead registry entries reserved slots: %v", got)
	}
}

func TestNextPaneIndices_NilRegistryAndTitleMax(t *testing.T) {
	panes := []tmux.Pane{
		{ID: "%1", Title: "sess__cc_1"},
		{ID: "%2", Title: "sess__cc_3_opus"},
		{ID: "%3", Title: "sess__cc_2[api]"},
		{ID: "%4", Title: "not an ntm title"},
	}
	got := nextPaneIndices(panes, nil)
	want := map[string]int{"cc": 3}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nextPaneIndices(nil registry) = %v, want %v", got, want)
	}
}
