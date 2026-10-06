package status

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// DetectCompaction classifies one capture with no history: the banner patterns
// alone. Production detection goes through CompactionDetector.Check, which
// also establishes that a banner is new.
func DetectCompaction(output string, agentType string) *CompactionEvent {
	matches := compactionLineEvents(compactionLines(output), agentType)
	if len(matches) == 0 {
		return nil
	}
	event := matches[len(matches)-1].event
	event.DetectedAt = time.Now()
	return &event
}

func TestDetectCompaction_ClaudeExactMatch(t *testing.T) {
	// Completion banners, not arbitrary prose about context management.
	tests := []struct {
		name      string
		output    string
		agentType string
		wantMatch bool
		wantText  string
	}{
		{"exact Claude Code compaction message", "Some output\nConversation compacted\nMore output", "claude", true, "Conversation compacted"},
		{"exact match with cc alias", "Conversation compacted", "cc", true, "Conversation compacted"},
		{"session continuation message", "This session is being continued from a previous conversation that ran out of context", "claude", true, ""},
		{"no compaction - normal output", "def hello():\n    print('hello world')\n", "claude", false, ""},
		{"no compaction - empty output", "", "claude", false, ""},
		{"terminal decoration", "\x1b[32m⎿ Conversation compacted (ctrl+o to expand)\x1b[0m\r\n", "cc", true, "Conversation compacted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := DetectCompaction(tt.output, tt.agentType)
			if tt.wantMatch {
				if event == nil {
					t.Fatal("expected compaction to be detected")
				}
				if tt.wantText != "" && event.MatchedText != tt.wantText {
					t.Errorf("matched text = %q, want %q", event.MatchedText, tt.wantText)
				}
			} else if event != nil {
				t.Errorf("expected no compaction, got: %+v", event)
			}
		})
	}
}

func TestDetectCompaction_AllAgentTypes(t *testing.T) {
	tests := []struct {
		name, output, agentType string
		wantMatch               bool
	}{
		// These old wildcard matches are negative regressions, not completion banners.
		{"claude context compacted", "The context was compacted", "claude", false},
		{"claude ran out of context", "I ran out of context", "claude", false},
		{"claude session continued", "session is being continued from a previous", "cc", false},
		{"codex context limit", "context limit reached", "codex", false},
		{"codex truncated", "conversation truncated", "cod", false},
		{"gemini context window", "context window exceeded", "gemini", false},
		{"gemini reset", "conversation reset due to limits", "gmi", false},
		{"claude pattern on codex", "Conversation compacted", "codex", false},
		{"codex pattern on claude", "context limit reached", "claude", false},
		{"generic continuing summary - claude", "continuing from summary", "claude", false},
		{"generic continuing summary - codex", "continuing from summary", "codex", false},
		{"codex completion", "• Context compacted", "cod", true},
		{"gemini completion", "✦ Chat history compressed from 100,000 to 10,000 tokens.", "gmi", true},
		{"unknown agent", "Conversation compacted", "unknown", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := DetectCompaction(tt.output, tt.agentType)
			if (event != nil) != tt.wantMatch {
				t.Errorf("match = %+v, want %t", event, tt.wantMatch)
			}
		})
	}
}

func TestDetectCompaction_NormalizesAliases(t *testing.T) {
	tests := []struct{ name, output, agentType string }{
		{"claude dash code", "Conversation compacted", "claude-code"},
		{"codex cli", "Context compacted", "codex-cli"},
		{"openai codex alias", "Context compacted", " openai-codex "},
		{"google gemini alias", "Chat history compressed from 100 to 50 tokens.", "Google-Gemini"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if event := DetectCompaction(tt.output, tt.agentType); event == nil {
				t.Fatalf("expected compaction for agent type %q", tt.agentType)
			}
		})
	}
}

func TestDetectCompaction_NoFalsePositives(t *testing.T) {
	normalOutputs := []string{
		"Running tests...\nAll 42 tests passed",
		"git status\nOn branch main\nnothing to commit",
		"npm install\ninstalled 1234 packages",
		"Building project...\nBuild complete",
		"Error: undefined is not a function",
		"fatal: not a git repository", "Connection refused", "Rate limit exceeded",
		"def compact_data(x):\n    return x.strip()",
		"class ConversationManager:\n    def reset(self): pass",
		"We should test Conversation compacted in the detector.",
		"The conversation was summarized by this function.",
		"Reset the context after the next request.",
		"This context has a limit of 100 tokens.",
		"`Conversation compacted`", "- Conversation compacted", "> Context compacted",
		"```text\nConversation compacted\n```",
		"~~~\nContext compacted\n~~~",
		"✦ Chat history compression failed.",
	}
	for _, output := range normalOutputs {
		t.Run(output[:min(30, len(output))], func(t *testing.T) {
			for _, kind := range []string{"claude", "codex", "gemini", "cc", "cod", "gmi"} {
				if event := DetectCompaction(output, kind); event != nil {
					t.Errorf("false positive for %s in %q: %+v", kind, output, event)
				}
			}
		})
	}
}

func TestCompactionDetector(t *testing.T) {
	detector := NewCompactionDetector(time.Minute)
	if event := detector.Check("normal output", "claude", "%0"); event != nil {
		t.Error("normal output must not detect compaction")
	}
	detector.Check("previous output", "claude", "%1")
	event := detector.Check("previous output\nConversation compacted", "claude", "%1")
	if event == nil || event.PaneID != "%1" {
		t.Fatalf("event = %+v, want new compaction for %%1", event)
	}
	if len(detector.Events()) != 1 || len(detector.EventsForPane("%1")) != 1 || len(detector.EventsForPane("%0")) != 0 {
		t.Fatalf("wrong event history: %+v", detector.Events())
	}
}

func TestCompactionDetector_HasRecentCompaction(t *testing.T) {
	d := NewCompactionDetector(time.Minute)
	if d.HasRecentCompaction("%1", time.Minute) {
		t.Fatal("unexpected initial event")
	}
	d.Check("previous output", "claude", "%1")
	d.Check("previous output\nConversation compacted", "claude", "%1")
	if !d.HasRecentCompaction("%1", time.Minute) || d.HasRecentCompaction("%2", time.Minute) {
		t.Fatal("incorrect per-pane recent event")
	}
}

func TestCompactionDetector_Clear(t *testing.T) {
	d := NewCompactionDetector(time.Minute)
	d.Check("previous output", "claude", "%1")
	d.Check("previous output\nConversation compacted", "claude", "%1")
	if len(d.Events()) != 1 {
		t.Fatal("expected one event before clear")
	}
	d.Clear()
	if len(d.Events()) != 0 || d.Check("previous output\nConversation compacted", "claude", "%1") != nil {
		t.Fatal("clear must discard events and prime again, not replay history")
	}
}

func TestCompactionEvent_Fields(t *testing.T) {
	event := DetectCompaction("Conversation compacted", "claude")
	if event == nil || event.AgentType != "claude" || event.MatchedText != "Conversation compacted" || event.DetectedAt.IsZero() || event.Pattern == "" {
		t.Fatalf("invalid event fields: %+v", event)
	}
}

func TestCompactionDetectorOnlyNewOutput(t *testing.T) {
	const banner = "Conversation compacted"
	for _, tc := range []struct {
		name    string
		outputs []string
		want    []bool
	}{
		{"baseline is historical", []string{"old progress\n" + banner, "old progress\n" + banner}, []bool{false, false}},
		{"append once", []string{"progress anchor", "progress anchor\n" + banner, "progress anchor\n" + banner + "\nnew work"}, []bool{false, true, false}},
		{"footer edits", []string{"progress anchor\nspinner 1\n" + banner, "progress anchor\nspinner 2\n" + banner, "progress anchor\nspinner 3\n" + banner}, []bool{false, false, false}},
		{"second identical banner is new", []string{"progress anchor\n" + banner, "progress anchor\n" + banner + "\nnext progress\n" + banner}, []bool{false, true}},
		{"scroll-off", []string{banner + "\nremaining progress\nmore progress", "remaining progress\nmore progress\n" + banner}, []bool{false, true}},
		{"unalignable redraw", []string{"initial progress", "unrelated output\n" + banner, "unrelated output\n" + banner + "\nwork"}, []bool{false, false, false}},
		{"short prompts are not anchors", []string{"\n>\n" + banner, "\n>\nnew text\n" + banner}, []bool{false, false}},
		{"empty capture does not rearm", []string{"progress anchor\n" + banner, "", "progress anchor\n" + banner}, []bool{false, false, false}},
		{"styling is not new", []string{"progress anchor\n" + banner, "progress anchor\n\x1b[31m" + banner + "\x1b[0m"}, []bool{false, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewCompactionDetector(time.Minute)
			count := 0
			for i, output := range tc.outputs {
				event := d.Check(output, "claude", "%7")
				if (event != nil) != tc.want[i] {
					t.Fatalf("observation %d: got %+v, want new=%t", i, event, tc.want[i])
				}
				if tc.want[i] {
					count++
				}
			}
			if len(d.Events()) != count {
				t.Fatal("history records sightings instead of occurrences")
			}
		})
	}
}

func TestCompactionDetectorConcurrentPollsAndPaneLifetime(t *testing.T) {
	d := NewCompactionDetector(time.Minute)
	d.Check("progress anchor", "cc", "%1")
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.Check("progress anchor\nConversation compacted", "claude-code", "%1")
		}()
	}
	wg.Wait()
	if len(d.Events()) != 1 {
		t.Fatalf("concurrent polls created %d events", len(d.Events()))
	}
	// A different agent in the same pane is a new baseline, not an event.
	if d.Check("progress anchor\nContext compacted", "cod", "%1") != nil {
		t.Fatal("agent replacement replayed history")
	}
	d.ForgetPane("%1")
	if len(d.Events()) != 0 || d.Check("progress anchor\nConversation compacted", "cc", "%1") != nil {
		t.Fatal("forgotten pane retained state or replayed its new baseline")
	}
}

func TestCompactionDetectorBoundedEvidence(t *testing.T) {
	d := NewCompactionDetector(time.Minute)
	large := strings.Repeat("old progress\n", 10000) + "retained progress"
	d.Check(large, "cc", "%1")
	if len(d.observations["%1"].lines) > 256 {
		t.Fatal("retained an unbounded capture")
	}
	if d.Check(large+"\nConversation compacted", "cc", "%1") == nil {
		t.Fatal("bounded rolling capture lost a new banner")
	}
	for i := 0; i < 50; i++ {
		if d.Check(large+"\nConversation compacted\n"+fmt.Sprint(i), "cc", "%1") != nil {
			t.Fatal("rolling footer change replayed the banner")
		}
	}
	if DetectCompaction(strings.Repeat("x", 70000)+"Conversation compacted", "cc") != nil {
		t.Fatal("byte truncation manufactured a banner from a partial line")
	}
}
