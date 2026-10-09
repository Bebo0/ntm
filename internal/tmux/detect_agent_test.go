package tmux

import (
	"testing"

	"github.com/Dicklesworthstone/ntm/internal/agent"
)

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
