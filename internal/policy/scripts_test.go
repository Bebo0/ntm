package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeHookScriptReadsCurrentStdinPayload(t *testing.T) {
	for _, want := range []string{
		"HOOK_INPUT=\"$(cat)\"",
		"'.tool_name // empty'",
		"'.tool_input.command // empty'",
		"exit 2",
	} {
		if !strings.Contains(ClaudeHookScript, want) {
			t.Fatalf("claude hook script missing %q", want)
		}
	}
	if strings.Contains(ClaudeHookScript, "exit 1\nfi\n\nexit 0") {
		t.Fatal("claude hook script still uses non-blocking exit 1 for denied commands")
	}
}

// Every installed script asks the check to record its refusals and keeps no
// log of its own (bd-cl6me).
func TestInstalledScriptsCheckInHookMode(t *testing.T) {
	for name, script := range map[string]string{"git": GitWrapperScript, "rm": RmWrapperScript, "claude": ClaudeHookScript} {
		if !strings.Contains(script, "--json --hook") {
			t.Errorf("%s script does not run the check in hook mode", name)
		}
		if strings.Contains(script, "blocked.jsonl") {
			t.Errorf("%s script still writes the blocked log itself", name)
		}
	}
}

// The shell hooks before bd-cl6me logged with `jq -n` (pretty-printed, one
// object over several lines), which the line reader dropped entirely, so `ntm
// safety blocked` showed none of them. Those entries read back now, and a
// malformed line still does not hide the entries after it.
func TestReadBlockedLogReadsPrettyPrintedHookEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked.jsonl")
	log := `{
  "timestamp": "2026-10-06T00:28:03Z",
  "command": "git reset --hard",
  "reason": "Hard reset loses uncommitted changes",
  "action": "block"
}
not json at all
{"timestamp":"2026-10-06T00:29:00Z","session":"s","command":"rm -rf build","action":"block"}
{
  "timestamp": "2026-10-06T00:30:00Z",
  "command": "git push --force",
  "action": "approve"
}
`
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := ReadBlockedLog(path)
	if err != nil {
		t.Fatalf("ReadBlockedLog: %v", err)
	}
	if len(entries) != 3 || entries[0].Command != "git reset --hard" || entries[1].Session != "s" || entries[2].Action != ActionApprove {
		t.Fatalf("entries = %+v, want the pretty, compact and pretty entries in order", entries)
	}
}

func TestAppendBlockedRoundTripsThroughReadBlockedLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "blocked.jsonl")
	first := BlockedEntry{Timestamp: time.Now().UTC().Truncate(time.Second), Session: "s", Agent: "%1",
		Command: "rm -rf \"$HOME\"\nnext", Reason: "recursive delete", Action: ActionBlock}
	second := BlockedEntry{Timestamp: first.Timestamp, Command: "git push --force", Action: ActionApprove}
	for _, entry := range []BlockedEntry{first, second} {
		if err := AppendBlocked(path, entry); err != nil {
			t.Fatalf("AppendBlocked: %v", err)
		}
	}
	entries, err := ReadBlockedLog(path)
	if err != nil {
		t.Fatalf("ReadBlockedLog: %v", err)
	}
	if len(entries) != 2 || entries[0].Command != first.Command || entries[0].Session != "s" || entries[1].Action != ActionApprove {
		t.Fatalf("entries = %+v, want both appended entries intact", entries)
	}
}
