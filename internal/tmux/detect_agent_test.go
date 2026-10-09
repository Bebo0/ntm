package tmux

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/ntm/internal/agent"
)

func TestOMPMetadataDiscovery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix fake tmux executable")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "tmux")
	fixture := `#!/bin/sh
case "$*" in
  *'#{@omp_run}'*'#{@omp_instance}'*'#{@omp_socket}'*) ;;
  *) exit 91 ;;
esac
case "$*" in
  *'#{window_activity}'*) printf '%s\n' "$NTM_TEST_OMP_ACTIVITY" ;;
  *'#{session_name}'*) printf '%s\n' "$NTM_TEST_OMP_ALL" ;;
  *) printf '%s\n' "$NTM_TEST_OMP_PANE" ;;
esac
`
	if err := os.WriteFile(binary, []byte(fixture), 0700); err != nil {
		t.Fatal(err)
	}
	fields := []string{"%7", "0", "modelctl — Qwen", "docker", "160", "48", "1", "0", "2", "run-1", "instance-1", "/run/user/1000/control/omp.sock"}
	row := strings.Join(fields, FieldSeparator)
	activity := append([]string{}, fields[:7]...)
	activity = append(activity, "1700000000")
	activity = append(activity, fields[7:]...)
	t.Setenv("NTM_TMUX_BINARY", binary)
	t.Setenv("NTM_TEST_OMP_PANE", row)
	t.Setenv("NTM_TEST_OMP_ALL", "omp-test"+FieldSeparator+row)
	t.Setenv("NTM_TEST_OMP_ACTIVITY", strings.Join(activity, FieldSeparator))
	check := func(p Pane) {
		t.Helper()
		if p.Type != AgentOMP || p.OMPRun != "run-1" || p.OMPInstance != "instance-1" || p.OMPSocket != fields[11] {
			t.Fatalf("OMP ownership/control binding lost after OSC title change: %+v", p)
		}
		if p.ID != "%7" || p.WindowIndex != 2 || p.Index != 0 || p.Title != fields[2] || p.Command != "docker" || p.Width != 160 || p.Height != 48 {
			t.Fatalf("pane identity or presentation was rewritten: %+v", p)
		}
	}
	client := NewClient("")
	panes, err := client.GetPanesContext(context.Background(), "omp-test")
	if err != nil || len(panes) != 1 {
		t.Fatalf("GetPanes: %v, %v", panes, err)
	}
	check(panes[0])
	all, err := client.GetAllPanesContext(context.Background())
	if err != nil || len(all["omp-test"]) != 1 {
		t.Fatalf("GetAllPanes: %v, %v", all, err)
	}
	check(all["omp-test"][0])
	active, err := client.GetPanesWithActivityContext(context.Background(), "omp-test")
	if err != nil || len(active) != 1 {
		t.Fatalf("GetPanesWithActivity: %v, %v", active, err)
	}
	check(active[0].Pane)
}

func TestOMPMetadataIdentityPrecedence(t *testing.T) {
	for _, tc := range []struct {
		title, run, instance, socket string
		want                         AgentType
	}{
		{"overwritten", "run-1", "instance-1", "/tmp/omp.sock", AgentOMP},
		{"test__cc_1", "run-1", "instance-1", "/tmp/omp.sock", AgentOMP},
		{"overwritten", "", "", "", AgentUser},
		{"control", "run-1", "", "", AgentUser},
		{"test__cc_1", "", "", "", AgentClaude},
	} {
		p, err := parsePaneFromParts([]string{"%7", "0", tc.title, "docker", "160", "48", "0"}, []string{"0", "2", tc.run, tc.instance, tc.socket})
		if err != nil || p.Type != tc.want {
			t.Fatalf("%+v -> %+v, %v", tc, p, err)
		}
		if p.OMPRun != tc.run || p.OMPInstance != tc.instance || p.OMPSocket != tc.socket {
			t.Fatalf("metadata lost: %+v", p)
		}
		if tc.want == AgentOMP && (p.NTMIndex != 0 || p.Variant != "" || len(p.Tags) != 0) {
			t.Fatalf("stale title identity retained: %+v", p)
		}
	}
}

func TestDetectAgentFromCommand_FalsePositives(t *testing.T) {
	tests := []struct {
		command  string
		expected agent.AgentType
	}{
		{"cursor", agent.AgentTypeCursor},
		{"/usr/bin/cursor", agent.AgentTypeCursor},
		{"cursor run", agent.AgentTypeCursor}, // If pane_current_command includes args (rare)

		// Potential false positives with simple Contains
		{"my-cursor-script.sh", agent.AgentTypeUser},
		{"vim cursor.c", agent.AgentTypeUser}, // If tmux reports "vim cursor.c"
		{"libncurses", agent.AgentTypeUser},   // "curses" contains "curs"? No, but "cursor" contains "curs"
		{"recursor", agent.AgentTypeUser},

		{"windsurf", agent.AgentTypeWindsurf},
		{"/opt/windsurf/bin/windsurf", agent.AgentTypeWindsurf},
		{"tailwindsurfing", agent.AgentTypeUser}, // False positive candidate
	}

	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			got := detectAgentFromCommand(tt.command)
			if got != tt.expected {
				t.Errorf("detectAgentFromCommand(%q) = %v, want %v", tt.command, got, tt.expected)
			}
		})
	}
}

func TestOMPProcessIdentity(t *testing.T) {
	for _, command := range []string{"omp", "/home/bro/.local/bin/omp --model qwen"} {
		if got := detectAgentFromCommand(command); got != AgentOMP {
			t.Fatalf("%q recognized as %q", command, got)
		}
	}
	for _, argv := range [][]string{{"rg", "omp"}, {"bash", "omp"}, {"compiler"}} {
		if got := detectAgentFromArgv(argv); got == AgentOMP {
			t.Fatalf("false OMP recognition: %q", argv)
		}
	}
	if got := detectAgentFromArgv([]string{"/usr/bin/omp", "--model", "qwen"}); got != AgentOMP {
		t.Fatalf("OMP executable lost: %q", got)
	}
}
