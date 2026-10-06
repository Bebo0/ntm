package policy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DefaultBlockedLogSubPath is the default subdirectory for blocked command logs.
// This is relative to the user's home directory.
const DefaultBlockedLogSubPath = ".ntm/logs/blocked.jsonl"

// BlockedEntry represents a single blocked command log entry.
type BlockedEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Session   string    `json:"session,omitempty"`
	Agent     string    `json:"agent,omitempty"`
	Command   string    `json:"command"`
	Pattern   string    `json:"pattern"`
	Reason    string    `json:"reason"`
	Action    Action    `json:"action"` // block or approve (for logged approvals)
}

// defaultBlockedLogPath returns the default blocked log path in the user's home directory.
func defaultBlockedLogPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		// Fallback to current directory if home is unavailable
		return DefaultBlockedLogSubPath
	}
	return filepath.Join(home, DefaultBlockedLogSubPath)
}

// ReadBlockedLog reads all entries from a blocked log file.
func ReadBlockedLog(path string) ([]BlockedEntry, error) {
	if path == "" {
		path = defaultBlockedLogPath()
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading log file: %w", err)
	}

	// Entries are one JSON object per line. The shell hooks that wrote this
	// log before bd-cl6me used `jq -n` without -c, which pretty-prints one
	// object over several lines; those are collected until they parse. A line
	// that parses on its own always starts a fresh entry, so a malformed line
	// cannot swallow the entries after it.
	var entries []BlockedEntry
	var pending []byte
	for _, line := range splitLines(data) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry BlockedEntry
		if json.Unmarshal(line, &entry) == nil {
			entries = append(entries, entry)
			pending = nil
			continue
		}
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte("{")) {
			pending = append([]byte(nil), line...)
			continue
		}
		if pending == nil {
			continue // Skip malformed entries
		}
		pending = append(append(pending, '\n'), line...)
		if json.Unmarshal(pending, &entry) == nil {
			entries = append(entries, entry)
			pending = nil
		}
	}

	return entries, nil
}

// splitLines splits data into lines without allocating new strings.
func splitLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			lines = append(lines, data[start:i])
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}

// AppendBlocked adds one entry to a blocked log file (the default path when
// path is empty), creating the file and its directory as needed.
func AppendBlocked(path string, entry BlockedEntry) error {
	if path == "" {
		path = defaultBlockedLogPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating log directory: %w", err)
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encoding blocked entry: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening log file: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing log file: %w", err)
	}
	return f.Close()
}

// RecentBlocked returns blocked entries from the last n hours.
func RecentBlocked(path string, hours int) ([]BlockedEntry, error) {
	all, err := ReadBlockedLog(path)
	if err != nil {
		return nil, err
	}

	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour)
	var recent []BlockedEntry
	for _, e := range all {
		if e.Timestamp.After(cutoff) {
			recent = append(recent, e)
		}
	}
	return recent, nil
}
