package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/ntm/internal/spawnidentity"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

// TestSpawnIdentityCoordinator_BareClaudeIsNotToldToSetDefaultModel: since
// ntm#334 a bare --cc pane resolves no model by design, so registering it with
// Agent Mail must not print "set models.default_claude" on every spawn (that
// advice undoes the fix). A plugin type without a default still gets it.
func TestSpawnIdentityCoordinator_BareClaudeIsNotToldToSetDefaultModel(t *testing.T) {
	isolateIdentityDirs(t)
	srv, _ := fakeSpawnMailServer(t, "BraveFalcon")
	enableFakeAgentMail(t, srv.URL)

	projectKey := t.TempDir()
	const session = "delegated_notice"
	panes := []tmux.Pane{
		{ID: "%7", Index: 1, PID: 107, Command: "zsh", Title: session + "__cc_1", Type: tmux.AgentClaude},
		{ID: "%8", Index: 2, PID: 108, Command: "zsh", Title: session + "__hermes_1"},
	}
	stubPaneProbe(t, panes, nil)

	out := captureNoticeStdout(t, func() {
		coordinator := newSpawnIdentityCoordinator(projectKey, session, false)
		coordinator.PrepareAgent(context.Background(), spawnidentity.Agent{
			PaneIndex: 1, PaneID: "%7", PaneTitle: session + "__cc_1", AgentType: "cc",
		})
		if status := coordinator.Status(); status == nil || status.AgentsRegistered != 1 {
			t.Fatalf("bare claude pane must still register: %+v", status)
		}
	})
	if strings.Contains(out, "No model resolved") || strings.Contains(out, "default_claude") {
		t.Fatalf("bare --cc pane printed the delegation notice:\n%s", out)
	}

	out = captureNoticeStdout(t, func() {
		coordinator := newSpawnIdentityCoordinator(projectKey, session, false)
		coordinator.PrepareAgent(context.Background(), spawnidentity.Agent{
			PaneIndex: 2, PaneID: "%8", PaneTitle: session + "__hermes_1", AgentType: "hermes",
		})
	})
	if !strings.Contains(out, "No model resolved for pane 2 (hermes)") {
		t.Fatalf("plugin pane without a model lost the delegation notice:\n%s", out)
	}
}

func captureNoticeStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		_ = w.Close()
		os.Stdout = old
		<-done
		_ = r.Close()
	}
	defer restore() // t.Fatalf inside f must not leave os.Stdout redirected
	f()
	restore()
	return buf.String()
}
