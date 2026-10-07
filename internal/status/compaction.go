package status

import (
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/agent"
)

// CompactionPattern defines provider-specific completion banners. Context-limit
// warnings and prose about compaction are not evidence that compaction finished.
type CompactionPattern struct {
	Agent    string
	Patterns []*regexp.Regexp
}

// CompactionEvent represents an observed compaction completion banner.
type CompactionEvent struct {
	PaneID      string    `json:"pane_id"`
	AgentType   string    `json:"agent_type"`
	DetectedAt  time.Time `json:"detected_at"`
	MatchedText string    `json:"matched_text"`
	Pattern     string    `json:"pattern"`
}

var (
	compactionPatterns []CompactionPattern
	patternsOnce       sync.Once
)

// Match complete banner lines, not arbitrary sentences containing these words.
// The optional glyphs are terminal message markers, not Markdown list markers.
// Keep capture group 1 free of presentation glyphs for stable evidence.
var claudePatterns = []string{
	`(?i)^[ \t]*(?:[⏺●•⎿][ \t]*)?(Conversation compacted)(?:[ \t]+\(ctrl\+o to expand\))?[.!]?[ \t]*$`,
	`(?i)^[ \t]*(This session is being continued from a previous conversation that ran out of context)[.]?[ \t]*$`,
}

// Codex renders "Context compacted" as a completed history cell. A limit or
// reset error alone must not trigger an unsolicited recovery prompt.
var codexPatterns = []string{
	`(?i)^[ \t]*(?:[•●][ \t]*)?(Context compacted)[.!]?[ \t]*$`,
}

// Gemini's CompressionMessage renders this only for CompressionStatus.COMPRESSED.
var geminiPatterns = []string{
	`(?i)^[ \t]*(?:✦[ \t]*)?(Chat history compressed from [0-9,]+ to [0-9,]+ tokens)[.]?[ \t]*$`,
}

func initPatterns() {
	patternsOnce.Do(func() {
		compactionPatterns = []CompactionPattern{
			{Agent: "cc", Patterns: compilePatterns(claudePatterns)},
			{Agent: "cod", Patterns: compilePatterns(codexPatterns)},
			{Agent: "gmi", Patterns: compilePatterns(geminiPatterns)},
		}
	})
}

func compilePatterns(patterns []string) []*regexp.Regexp {
	result := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		result = append(result, regexp.MustCompile(p))
	}
	return result
}

func normalizedCompactionAgentType(agentType string) string {
	switch canonical := agent.AgentType(agentType).Canonical(); canonical {
	case agent.AgentTypeClaudeCode:
		return "cc"
	case agent.AgentTypeCodex:
		return "cod"
	case agent.AgentTypeGemini:
		return "gmi"
	case agent.AgentTypeAntigravity:
		return "agy"
	default:
		return strings.ToLower(strings.TrimSpace(agentType))
	}
}

// compactionLines bounds retained terminal evidence without cutting into a line
// (which could turn the tail of ordinary prose into a synthetic banner).
func compactionLines(output string) []string {
	const maxBytes, maxLines = 64 * 1024, 256
	output = StripANSI(output)
	output = strings.ReplaceAll(output, "\r\n", "\n")
	if len(output) > maxBytes {
		output = output[len(output)-maxBytes:]
		if end := strings.IndexByte(output, '\n'); end >= 0 {
			output = output[end+1:]
		} else {
			return nil
		}
	}
	lines := strings.Split(output, "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	// Clone: a short retained slice must not retain an arbitrarily large raw
	// capture through substring backing storage.
	for i := range lines {
		lines[i] = strings.Clone(strings.TrimRight(lines[i], " \t\r"))
	}
	return lines
}

type compactionLineEvent struct {
	line  int
	event CompactionEvent
}

func compactionLineEvents(lines []string, agentType string) []compactionLineEvent {
	initPatterns()
	kind := normalizedCompactionAgentType(agentType)
	var matches []compactionLineEvent
	fence := ""
	for line, text := range lines {
		trimmed := strings.TrimSpace(text)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			marker := trimmed[:3]
			switch fence {
			case "":
				fence = marker
			case marker:
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		for _, cp := range compactionPatterns {
			if cp.Agent != kind {
				continue
			}
			for _, pattern := range cp.Patterns {
				match := pattern.FindStringSubmatch(text)
				if len(match) > 1 {
					matches = append(matches, compactionLineEvent{line: line, event: CompactionEvent{
						AgentType: agentType, MatchedText: match[1], Pattern: pattern.String(),
					}})
					break
				}
			}
		}
	}
	return matches
}

type compactionObservation struct {
	agentType string
	lines     []string
}

// CompactionDetector tracks new completion banners, not repeated sightings of
// old scrollback. The first capture (and any unalignable redraw) is a baseline.
// Evidence is process-local; restarting a monitor never replays old banners.
type CompactionDetector struct {
	mu           sync.Mutex
	events       []CompactionEvent
	maxAge       time.Duration
	observations map[string]compactionObservation
}

func NewCompactionDetector(maxAge time.Duration) *CompactionDetector {
	if maxAge == 0 {
		maxAge = 5 * time.Minute
	}
	return &CompactionDetector{
		events: make([]CompactionEvent, 0), maxAge: maxAge,
		observations: make(map[string]compactionObservation),
	}
}

// Check records only banners in an aligned new tail. Changing a footer, ANSI
// styling, or unrelated text around a visible banner does not make it new.
// A capture without shared nonblank context is rebased, not replayed. Multiple
// new banners in one capture produce one recovery opportunity, for the latest.
func (d *CompactionDetector) Check(output, agentType, paneID string) *CompactionEvent {
	if paneID == "" || output == "" {
		return nil // Missing evidence must not erase a previous baseline.
	}
	lines := compactionLines(output)
	kind := normalizedCompactionAgentType(agentType)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.observations == nil {
		d.observations = make(map[string]compactionObservation)
	}
	previous, known := d.observations[paneID]
	d.observations[paneID] = compactionObservation{agentType: kind, lines: lines}
	if !known || previous.agentType != kind {
		return nil
	}
	start, oldStart := compactionNewTail(previous.lines, lines)
	if start == len(lines) {
		return nil
	}
	// A rewritten tail may still contain an old banner. Match occurrences,
	// not entire snapshots, so changing a spinner before it cannot resend it.
	old := make(map[string]int)
	for _, match := range compactionLineEvents(previous.lines, agentType) {
		if match.line >= oldStart {
			old[strings.ToLower(match.event.MatchedText)]++
		}
	}
	var event *CompactionEvent
	for _, match := range compactionLineEvents(lines, agentType) {
		if match.line < start {
			continue
		}
		key := strings.ToLower(match.event.MatchedText)
		if old[key] > 0 {
			old[key]--
			continue
		}
		copy := match.event
		event = &copy
	}
	if event != nil {
		event.PaneID, event.DetectedAt = paneID, time.Now()
		d.events = append(d.events, *event)
		d.prune()
	}
	return event
}

// compactionNewTail returns the new suffix and the old rewritten suffix to
// compare it against. A suffix/prefix overlap handles scroll-off; a common
// prefix handles the final composer line being replaced as output arrives.
func compactionNewTail(old, current []string) (start, oldStart int) {
	prefix := 0
	for prefix < len(old) && prefix < len(current) && old[prefix] == current[prefix] {
		prefix++
	}
	meaningful := func(lines []string) bool {
		for _, line := range lines {
			if len(strings.TrimSpace(line)) >= 8 {
				return true
			}
		}
		return false
	}
	for overlap := min(len(old), len(current)); overlap > prefix; overlap-- {
		if !meaningful(current[:overlap]) {
			continue
		}
		same := true
		for i := 0; i < overlap; i++ {
			if old[len(old)-overlap+i] != current[i] {
				same = false
				break
			}
		}
		if same {
			return overlap, len(old) // All old matching occurrences precede the new tail.
		}
	}
	if prefix > 0 && meaningful(current[:prefix]) {
		return prefix, prefix
	}
	return len(current), len(old)
}

// ForgetPane discards a vanished or replaced pane's baseline and event history.
// Only a complete topology observation should authorize forgetting absent panes.
func (d *CompactionDetector) ForgetPane(paneID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.observations, paneID)
	kept := d.events[:0]
	for _, event := range d.events {
		if event.PaneID != paneID {
			kept = append(kept, event)
		}
	}
	d.events = kept
}

func (d *CompactionDetector) Events() []CompactionEvent {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prune()
	result := make([]CompactionEvent, len(d.events))
	copy(result, d.events)
	return result
}

func (d *CompactionDetector) EventsForPane(paneID string) []CompactionEvent {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prune()
	result := make([]CompactionEvent, 0)
	for _, e := range d.events {
		if e.PaneID == paneID {
			result = append(result, e)
		}
	}
	return result
}

func (d *CompactionDetector) HasRecentCompaction(paneID string, within time.Duration) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	cutoff := time.Now().Add(-within)
	for _, e := range d.events {
		if e.PaneID == paneID && e.DetectedAt.After(cutoff) {
			return true
		}
	}
	return false
}

func (d *CompactionDetector) Clear() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = make([]CompactionEvent, 0)
	d.observations = make(map[string]compactionObservation)
}

func (d *CompactionDetector) prune() {
	cutoff := time.Now().Add(-d.maxAge)
	kept := d.events[:0]
	for _, e := range d.events {
		if e.DetectedAt.After(cutoff) {
			kept = append(kept, e)
		}
	}
	d.events = kept
}
