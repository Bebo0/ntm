package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

func TestModelDelegationIsBuiltInDefault(t *testing.T) {
	for agentType, want := range map[string]bool{
		// Empty compiled-in default: delegation is the intended config.
		"cc":       true, // ntm#334
		"claude":   true,
		"grok":     true,
		"oc":       true,
		"opencode": true,
		"omp":      true,
		"cursor":   true, // no [models] key exists to set
		// Non-empty compiled-in default: an empty model means the user
		// blanked it, so the notice still explains the placeholder.
		"cod":    false,
		"gmi":    false,
		"ollama": false,
		// Plugin agent types keep the notice.
		"hermes": false,
		"":       false,
	} {
		if got := modelDelegationIsBuiltInDefault(agentType); got != want {
			t.Errorf("modelDelegationIsBuiltInDefault(%q) = %v, want %v", agentType, got, want)
		}
	}
}

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
		coordinator := newSpawnIdentityCoordinator(projectKey, session)
		coordinator.prepareAgent(context.Background(), spawnedAgentInfo{
			paneIndex: 1, paneID: "%7", paneTitle: session + "__cc_1", agentType: "cc",
		})
		if status := coordinator.finalStatus(); status == nil || status.AgentsRegistered != 1 {
			t.Fatalf("bare claude pane must still register: %+v", status)
		}
	})
	if strings.Contains(out, "No model resolved") || strings.Contains(out, "default_claude") {
		t.Fatalf("bare --cc pane printed the delegation notice:\n%s", out)
	}

	out = captureNoticeStdout(t, func() {
		coordinator := newSpawnIdentityCoordinator(projectKey, session)
		coordinator.prepareAgent(context.Background(), spawnedAgentInfo{
			paneIndex: 2, paneID: "%8", paneTitle: session + "__hermes_1", agentType: "hermes",
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
