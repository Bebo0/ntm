package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

// InterruptFixtureReadyMarker is the line the fixture pane prints once it is
// ready; it is the only pane output the fixture produces besides tty echo,
// so finding it anywhere persistent means pane contents leaked there.
const InterruptFixtureReadyMarker = "NTM_INTERRUPT_FIXTURE_READY"

// interruptFixtureScript traps SIGINT and appends one "INT" line per
// interrupt, and one "LINE:<text>" line per submitted input line, to the log
// named by $1. `read` is interrupted by the trapped SIGINT and the loop simply
// resumes, so the pane survives any number of interrupts.
const interruptFixtureScript = `#!/bin/bash
log="$1"
trap 'echo INT >> "$log"' INT
echo ` + InterruptFixtureReadyMarker + `
while true; do
  if IFS= read -r line; then
    printf 'LINE:%s\n' "$line" >> "$log"
  fi
done
`

// InterruptFixture is a real tmux session whose single pane records every
// interrupt it receives and every line submitted to it. It is the ground
// truth for interrupt/delivery side effects that a robot envelope cannot
// prove the absence of (for example: no second Ctrl+C on a replayed retry).
type InterruptFixture struct {
	Session string // tmux session name
	PaneID  string // tmux pane ID (%N)
	LogPath string // fixture event log
}

// StartInterruptFixture creates the fixture session in the current tmux
// server (callers own tmux isolation and throttling, e.g. via
// RequireTmuxThrottled) and waits until the pane is ready. The session is
// killed on test cleanup.
func StartInterruptFixture(t *testing.T, tag string) *InterruptFixture {
	t.Helper()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.log")
	scriptPath := filepath.Join(dir, "pane.sh")
	if err := os.WriteFile(scriptPath, []byte(interruptFixtureScript), 0o755); err != nil {
		t.Fatalf("write interrupt fixture script: %v", err)
	}

	session := fmt.Sprintf("ntm_intfx_%s_%d", tag, time.Now().UnixNano())
	paneID, err := tmux.DefaultClient.Run(
		"new-session", "-d", "-s", session, "-c", dir,
		"-P", "-F", "#{pane_id}", fmt.Sprintf("/bin/bash %s %s", scriptPath, logPath),
	)
	if err != nil {
		t.Fatalf("create interrupt fixture session: %v", err)
	}
	paneID = strings.TrimSpace(paneID)
	if paneID == "" {
		t.Fatal("create interrupt fixture session returned an empty pane ID")
	}
	t.Cleanup(func() { _ = tmux.KillSession(session) })

	deadline := time.Now().Add(5 * time.Second)
	for {
		output, captureErr := tmux.CapturePaneOutput(paneID, 20)
		if captureErr == nil && strings.Contains(output, InterruptFixtureReadyMarker) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for interrupt fixture: output=%q err=%v", output, captureErr)
		}
		time.Sleep(25 * time.Millisecond)
	}
	return &InterruptFixture{Session: session, PaneID: paneID, LogPath: logPath}
}

// Events counts the interrupts the fixture received and the submitted lines
// containing marker.
func (f *InterruptFixture) Events(t *testing.T, marker string) (interrupts, lines int) {
	t.Helper()
	data, err := os.ReadFile(f.LogPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0
		}
		t.Fatalf("read interrupt fixture log: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case line == "INT":
			interrupts++
		case strings.HasPrefix(line, "LINE:") && strings.Contains(line, marker):
			lines++
		}
	}
	return interrupts, lines
}

// WaitForEvents polls until the fixture recorded exactly the wanted counts,
// failing on overshoot or timeout.
func (f *InterruptFixture) WaitForEvents(t *testing.T, marker string, wantInterrupts, wantLines int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		interrupts, lines := f.Events(t, marker)
		if interrupts > wantInterrupts || lines > wantLines {
			t.Fatalf("fixture recorded %d interrupt(s) and %d %q line(s), want exactly %d and %d",
				interrupts, lines, marker, wantInterrupts, wantLines)
		}
		if interrupts == wantInterrupts && lines == wantLines {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out: fixture recorded %d interrupt(s) and %d %q line(s), want %d and %d",
				interrupts, lines, marker, wantInterrupts, wantLines)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// AssertQuiet waits long enough for a stray Ctrl+C or paste to land, then
// asserts the fixture counts still equal the wanted ones.
func (f *InterruptFixture) AssertQuiet(t *testing.T, marker string, wantInterrupts, wantLines int) {
	t.Helper()
	time.Sleep(750 * time.Millisecond)
	interrupts, lines := f.Events(t, marker)
	if interrupts != wantInterrupts || lines != wantLines {
		t.Fatalf("fixture recorded %d interrupt(s) and %d %q line(s), want still %d and %d — something touched the pane",
			interrupts, lines, marker, wantInterrupts, wantLines)
	}
}
