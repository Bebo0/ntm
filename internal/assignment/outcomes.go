package assignment

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/agent"
)

// OutcomeStats summarizes the finished assignments of one agent type across
// every session ledger: what `ntm agents stats` reports and what the agent
// profile matcher's performance bonus reads.
type OutcomeStats struct {
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	// TotalCompletionTime and TimedCompletions give the mean time from start
	// (or assignment, when no start was observed) to completion.
	TotalCompletionTime time.Duration `json:"total_completion_time"`
	TimedCompletions    int           `json:"timed_completions"`
	LastActivity        time.Time     `json:"last_activity"`
}

// SuccessRate is completed / (completed + failed); ok is false with no
// finished assignments.
func (s OutcomeStats) SuccessRate() (rate float64, ok bool) {
	finished := s.Completed + s.Failed
	if finished == 0 {
		return 0, false
	}
	return float64(s.Completed) / float64(finished), true
}

// AvgCompletionTime is the mean start-to-completion time of the timed
// completions (zero when none).
func (s OutcomeStats) AvgCompletionTime() time.Duration {
	if s.TimedCompletions == 0 {
		return 0
	}
	return s.TotalCompletionTime / time.Duration(s.TimedCompletions)
}

// outcomeArchiveName is the per-session append-only file that keeps finished
// assignments after the ledger clears them.
const outcomeArchiveName = "outcomes.jsonl"

// outcomeRecord is the archived part of a finished assignment.
type outcomeRecord struct {
	BeadID      string           `json:"bead_id"`
	AgentType   string           `json:"agent_type"`
	Status      AssignmentStatus `json:"status"`
	AssignedAt  time.Time        `json:"assigned_at"`
	StartedAt   *time.Time       `json:"started_at,omitempty"`
	CompletedAt *time.Time       `json:"completed_at,omitempty"`
	FailedAt    *time.Time       `json:"failed_at,omitempty"`
}

// appendOutcomeRecord archives a completed or failed assignment the ledger is
// about to forget. It is best-effort: statistics are informational and must
// never block a clear, so a write failure is logged, not returned.
func appendOutcomeRecord(storePath string, a *Assignment) {
	if a == nil || (a.Status != StatusCompleted && a.Status != StatusFailed) {
		return
	}
	line, err := json.Marshal(outcomeRecord{
		BeadID: a.BeadID, AgentType: a.AgentType, Status: a.Status, AssignedAt: a.AssignedAt,
		StartedAt: a.StartedAt, CompletedAt: a.CompletedAt, FailedAt: a.FailedAt,
	})
	if err == nil {
		var f *os.File
		f, err = os.OpenFile(filepath.Join(filepath.Dir(storePath), outcomeArchiveName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			_, err = f.Write(append(line, '\n'))
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
		}
	}
	if err != nil {
		slog.Warn("assignment outcome not archived", "bead_id", a.BeadID, "error", err)
	}
}

// AggregateOutcomes summarizes completed and failed assignments by agent type
// (claude, codex, gemini, antigravity, ... — long names) across every session
// under StorageDir(): the terminal assignments still in each ledger plus the
// ones archived when the ledger cleared them. A reassigned assignment counts
// for neither side: the agent neither finished nor failed it. Sessions whose
// ledger or archive cannot be read are skipped and counted.
func AggregateOutcomes() (stats map[string]OutcomeStats, unreadable int, err error) {
	stats = make(map[string]OutcomeStats)
	entries, err := os.ReadDir(StorageDir())
	if os.IsNotExist(err) {
		return stats, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sessionDir := filepath.Join(StorageDir(), entry.Name())
		if _, statErr := os.Stat(filepath.Join(sessionDir, assignmentsDirName+fileExtension)); statErr == nil {
			store, loadErr := LoadStoreStrictReadOnly(entry.Name())
			if loadErr != nil {
				unreadable++
			} else {
				for _, a := range store.List() {
					if a != nil {
						addOutcome(stats, a)
					}
				}
			}
		}
		if archiveErr := addArchivedOutcomes(stats, filepath.Join(sessionDir, outcomeArchiveName)); archiveErr != nil {
			unreadable++
		}
	}
	return stats, unreadable, nil
}

// addArchivedOutcomes folds one session's outcome archive into stats. A
// missing archive is not an error; a malformed line (a torn final append) is
// skipped.
func addArchivedOutcomes(stats map[string]OutcomeStats, path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		var rec outcomeRecord
		if json.Unmarshal(scanner.Bytes(), &rec) != nil {
			continue
		}
		addOutcome(stats, &Assignment{
			BeadID: rec.BeadID, AgentType: rec.AgentType, Status: rec.Status, AssignedAt: rec.AssignedAt,
			StartedAt: rec.StartedAt, CompletedAt: rec.CompletedAt, FailedAt: rec.FailedAt,
		})
	}
	return scanner.Err()
}

func addOutcome(stats map[string]OutcomeStats, a *Assignment) {
	kind := outcomeAgentType(a.AgentType)
	if kind == "" {
		return
	}
	s := stats[kind]
	switch a.Status {
	case StatusCompleted:
		s.Completed++
		if a.CompletedAt != nil {
			start := a.AssignedAt
			if a.StartedAt != nil {
				start = *a.StartedAt
			}
			if !start.IsZero() && a.CompletedAt.After(start) {
				s.TotalCompletionTime += a.CompletedAt.Sub(start)
				s.TimedCompletions++
			}
			if a.CompletedAt.After(s.LastActivity) {
				s.LastActivity = *a.CompletedAt
			}
		}
	case StatusFailed:
		s.Failed++
		if a.FailedAt != nil && a.FailedAt.After(s.LastActivity) {
			s.LastActivity = *a.FailedAt
		}
	default:
		return
	}
	stats[kind] = s
}

// outcomeAgentType normalizes a recorded agent type (cc, claude, cod, ...) to
// the long name agent profiles use.
func outcomeAgentType(recorded string) string {
	recorded = strings.TrimSpace(recorded)
	if recorded == "" {
		return ""
	}
	switch agent.AgentType(recorded).Canonical() {
	case agent.AgentTypeClaudeCode:
		return "claude"
	case agent.AgentTypeCodex:
		return "codex"
	case agent.AgentTypeGemini:
		return "gemini"
	case agent.AgentTypeAntigravity:
		return "antigravity"
	default:
		return strings.ToLower(string(agent.AgentType(recorded).Canonical()))
	}
}
