package cass

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

type mockExecutor struct {
	output []byte
	err    error
	args   [][]string
}

func (m *mockExecutor) Run(ctx context.Context, args ...string) ([]byte, error) {
	m.args = append(m.args, append([]string(nil), args...))
	return m.output, m.err
}

func TestNewClient(t *testing.T) {
	client := NewClient(WithTimeout(5 * time.Second))
	if client.timeout != 5*time.Second {
		t.Errorf("expected timeout 5s, got %v", client.timeout)
	}
}

func TestClient_Search(t *testing.T) {
	mockResp := `{"count": 1, "hits": [{"title": "test session", "score": 1.0}]}`
	client := NewClient(WithExecutor(&mockExecutor{output: []byte(mockResp), err: nil}))

	resp, err := client.Search(context.Background(), SearchOptions{Query: "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.Count != 1 {
		t.Errorf("expected count 1, got %d", resp.Count)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].Title != "test session" {
		t.Errorf("unexpected hits: %v", resp.Hits)
	}
}

func TestClient_TimelineDecodesCassEnvelope(t *testing.T) {
	// Shape of `cass timeline --json --group-by day` (cass 2026-10).
	resp := `{"range":{"start":1788731419970,"end":1791323419970},"total_sessions":2,"groups":{
	  "2026-09-22":[{"id":312,"agent":"claude_code","title":"miner","started_at":1790097662897,"ended_at":1790097845226,
	    "source_path":"/s/a.jsonl","message_count":39,"source_id":"local","origin_kind":"local","origin_host":null}],
	  "2026-09-21":[{"id":221,"agent":"pi_agent","title":"plan","started_at":1790009672964,"ended_at":1790009672972,
	    "source_path":"/s/b.jsonl","message_count":1,"source_id":"local"}]}}`
	mock := &mockExecutor{output: []byte(resp)}
	client := NewClient(WithExecutor(mock))

	got, err := client.Timeline(context.Background(), "30d", "day", "claude_code")
	if err != nil {
		t.Fatalf("Timeline() error: %v", err)
	}
	if want := []string{"timeline", "--json", "--since=30d", "--group-by=day", "--agent=claude_code"}; !slices.Equal(mock.args[0], want) {
		t.Fatalf("args = %v, want %v", mock.args[0], want)
	}
	day := got.Groups["2026-09-22"]
	if got.TotalSessions != 2 || len(got.Groups) != 2 || len(day) != 1 || day[0].Agent != "claude_code" ||
		day[0].MessageCount != 39 || !day[0].StartTime().Equal(time.UnixMilli(1790097662897)) {
		t.Fatalf("Timeline() = %+v", got)
	}
}

// TestTimelineReadsInstalledCass checks the installed cass's timeline against
// its own invariants, whatever sessions this machine has indexed.
func TestTimelineReadsInstalledCass(t *testing.T) {
	client := NewClient()
	if !client.IsInstalled() {
		t.Skip("cass not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	flat, err := client.Timeline(ctx, "30d", "none")
	if err != nil {
		t.Fatalf("Timeline(none) error: %v", err)
	}
	if flat.Sessions == nil || flat.TotalSessions != len(flat.Sessions) || flat.Range.End <= flat.Range.Start {
		t.Fatalf("Timeline(none): total=%d sessions=%d range=%+v", flat.TotalSessions, len(flat.Sessions), flat.Range)
	}
	for _, s := range flat.Sessions {
		if s.Agent == "" || s.StartedAt == 0 || s.SourcePath == "" {
			t.Fatalf("Timeline(none) session missing agent/start/path: %+v", s)
		}
	}

	byDay, err := client.Timeline(ctx, "30d", "day")
	if err != nil {
		t.Fatalf("Timeline(day) error: %v", err)
	}
	grouped := 0
	for _, sessions := range byDay.Groups {
		grouped += len(sessions)
	}
	if byDay.TotalSessions != grouped {
		t.Fatalf("Timeline(day): total=%d but groups hold %d sessions", byDay.TotalSessions, grouped)
	}
}

// TestStatusAndVersionReadInstalledCass: `cass status --json` reports the
// database size as database.db_bytes and has no version, which comes from
// `cass --version`.
func TestStatusAndVersionReadInstalledCass(t *testing.T) {
	client := NewClient()
	if !client.IsInstalled() {
		t.Skip("cass not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	version, err := client.Version(ctx)
	if err != nil || !regexp.MustCompile(`^\d+\.\d+\.\d+`).MatchString(version) {
		t.Fatalf("Version() = %q, %v; want a semver", version, err)
	}

	status, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if status.Database.Exists && status.Database.SizeMB() <= 0 {
		t.Fatalf("database exists but size is %v MB (db_bytes not read)", status.Database.SizeMB())
	}
	if status.Index.Exists && status.Index.IsReady() && status.Index.Documents <= 0 {
		t.Fatalf("ready index reports %d documents", status.Index.Documents)
	}
}

func TestClient_Status(t *testing.T) {
	mockResp := `{"healthy": true, "conversations": 42}`
	client := NewClient(WithExecutor(&mockExecutor{output: []byte(mockResp), err: nil}))

	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !status.Healthy {
		t.Error("expected healthy status")
	}
	if status.Conversations != 42 {
		t.Errorf("expected 42 conversations, got %d", status.Conversations)
	}
}

func TestClient_HealthUsesHealthCommand(t *testing.T) {
	executor := &mockExecutor{output: []byte(`{"healthy": true}`)}
	client := NewClient(WithExecutor(executor))

	if _, err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health() error: %v", err)
	}
	if len(executor.args) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(executor.args))
	}
	if got, want := executor.args[0], []string{"health", "--json"}; !slices.Equal(got, want) {
		t.Errorf("Health() command = %v, want %v", got, want)
	}
}

// Regression test for acfs#266: when the cass binary is installed but
// no index has been built, cass exits with code 3. The DefaultExecutor
// must surface this as ErrNotInitialized so downstream callers (ntm
// send's dedup-check) can degrade gracefully.
func TestDefaultExecutor_NotInitializedExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script-based fake binary is POSIX-only")
	}
	tmp, err := os.MkdirTemp("", "cass-fake-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })

	bin := filepath.Join(tmp, "cass")
	body := []byte("#!/bin/sh\nexit 3\n")
	if err := os.WriteFile(bin, body, 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	exe := &DefaultExecutor{BinaryPath: bin}
	_, err = exe.Run(context.Background(), "search", "--json", "anything")

	if !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("expected ErrNotInitialized for cass exit 3, got %v", err)
	}
}

// Verify that other non-zero exit codes still surface as the generic
// "cass execution failed" error — only exit 3 is treated specially.
func TestDefaultExecutor_OtherExitCodesUnchanged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script-based fake binary is POSIX-only")
	}
	tmp, err := os.MkdirTemp("", "cass-fake-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })

	bin := filepath.Join(tmp, "cass")
	body := []byte("#!/bin/sh\nexit 1\n")
	if err := os.WriteFile(bin, body, 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	exe := &DefaultExecutor{BinaryPath: bin}
	_, err = exe.Run(context.Background(), "search", "--json", "anything")

	if err == nil {
		t.Fatal("expected error for cass exit 1, got nil")
	}
	if errors.Is(err, ErrNotInitialized) {
		t.Fatalf("exit 1 should NOT be classified as ErrNotInitialized, got %v", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected wrapped *exec.ExitError, got %T: %v", err, err)
	}
}

func TestClient_NeedsReindex_CurrentSchemaUsesDocumentsField(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	mockResp := fmt.Sprintf(`{
		"healthy": true,
		"index": {
			"exists": true,
			"status": "ready",
			"fresh": true,
			"documents": 42,
			"last_indexed_at": %q
		},
		"database": {
			"exists": true,
			"opened": true,
			"path": "/home/user/.local/share/coding-agent-search/agent_search.db"
		}
	}`, now)

	client := NewClient(WithExecutor(&mockExecutor{output: []byte(mockResp), err: nil}))
	needs, reason := client.NeedsReindex(context.Background())
	if needs {
		t.Fatalf("NeedsReindex() = true, want false (reason=%q)", reason)
	}
	if reason != "" {
		t.Fatalf("NeedsReindex reason = %q, want empty", reason)
	}
}

func TestClient_NeedsReindex_CurrentSchemaOpenSkippedDoesNotForceEmpty(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	mockResp := fmt.Sprintf(`{
		"healthy": true,
		"index": {
			"exists": true,
			"status": "ready",
			"fresh": true,
			"last_indexed_at": %q
		},
		"database": {
			"exists": true,
			"opened": false,
			"open_skipped": true,
			"path": "/home/user/.local/share/coding-agent-search/agent_search.db"
		}
	}`, now)

	client := NewClient(WithExecutor(&mockExecutor{output: []byte(mockResp), err: nil}))
	needs, reason := client.NeedsReindex(context.Background())
	if needs {
		t.Fatalf("NeedsReindex() = true, want false when counts are skipped (reason=%q)", reason)
	}
	if reason != "" {
		t.Fatalf("NeedsReindex reason = %q, want empty", reason)
	}
}

func TestClient_NeedsReindex_CurrentSchemaStalenessUsesLastIndexedAt(t *testing.T) {
	old := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339Nano)
	mockResp := fmt.Sprintf(`{
		"healthy": true,
		"index": {
			"exists": true,
			"status": "ready",
			"fresh": true,
			"documents": 5,
			"last_indexed_at": %q
		},
		"database": {
			"exists": true,
			"opened": true,
			"path": "/home/user/.local/share/coding-agent-search/agent_search.db"
		}
	}`, old)

	client := NewClient(WithExecutor(&mockExecutor{output: []byte(mockResp), err: nil}))
	needs, reason := client.NeedsReindex(context.Background())
	if !needs {
		t.Fatalf("NeedsReindex() = false, want true for stale last_indexed_at (reason=%q)", reason)
	}
	if !strings.Contains(reason, "Index stale") {
		t.Fatalf("NeedsReindex reason = %q, want stale-index reason", reason)
	}
}
