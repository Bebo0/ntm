package robot

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGetXFSearch_MissingBinary(t *testing.T) {
	t.Setenv("PATH", "")

	output, err := GetXFSearch(XFSearchOptions{Query: "test query"})
	if err != nil {
		t.Fatalf("GetXFSearch returned error: %v", err)
	}
	if output.Success {
		t.Fatalf("expected failure when xf missing")
	}
	if output.ErrorCode != ErrCodeDependencyMissing {
		t.Fatalf("expected %s, got %s", ErrCodeDependencyMissing, output.ErrorCode)
	}
}

func TestGetXFSearch_EmptyQuery(t *testing.T) {
	output, err := GetXFSearch(XFSearchOptions{Query: ""})
	if err != nil {
		t.Fatalf("GetXFSearch returned error: %v", err)
	}
	if output.Success {
		t.Fatalf("expected failure when query empty")
	}
	if output.ErrorCode != ErrCodeInvalidFlag {
		t.Fatalf("expected %s, got %s", ErrCodeInvalidFlag, output.ErrorCode)
	}
}

func TestGetXFSearch_WhitespaceQuery(t *testing.T) {
	output, err := GetXFSearch(XFSearchOptions{Query: "   "})
	if err != nil {
		t.Fatalf("GetXFSearch returned error: %v", err)
	}
	if output.Success {
		t.Fatalf("expected failure when query is whitespace")
	}
	if output.ErrorCode != ErrCodeInvalidFlag {
		t.Fatalf("expected %s, got %s", ErrCodeInvalidFlag, output.ErrorCode)
	}
}

func TestGetXFSearch_Success(t *testing.T) {
	tmpDir := t.TempDir()

	results := `[{"result_type":"tweet","id":"tweet-123","text":"error handling in go","created_at":"2024-01-15T00:00:00Z","score":0.95}]`
	script := fmt.Sprintf(`#!/bin/sh
if echo "$@" | grep -q -- "--version"; then
  echo "xf 0.2.1"
  exit 0
fi
echo '%s'
`, strings.ReplaceAll(results, "'", "'\\''"))

	stubPath := filepath.Join(tmpDir, "xf")
	if err := os.WriteFile(stubPath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write xf stub: %v", err)
	}
	t.Setenv("PATH", tmpDir)

	output, err := GetXFSearch(XFSearchOptions{
		Query: "error handling",
		Limit: 10,
		Mode:  "semantic",
		Sort:  "relevance",
	})
	if err != nil {
		t.Fatalf("GetXFSearch returned error: %v", err)
	}
	if !output.Success {
		t.Fatalf("expected success, got error: %s (code: %s)", output.Error, output.ErrorCode)
	}
	if output.Query != "error handling" {
		t.Fatalf("expected query 'error handling', got %q", output.Query)
	}
	if output.Count != 1 {
		t.Fatalf("expected count 1, got %d", output.Count)
	}
	if output.Mode != "semantic" {
		t.Fatalf("expected mode 'semantic', got %q", output.Mode)
	}
	if output.Sort != "relevance" {
		t.Fatalf("expected sort 'relevance', got %q", output.Sort)
	}
	if len(output.Hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(output.Hits))
	}
	if output.Hits[0].ID != "tweet-123" {
		t.Fatalf("expected hit ID 'tweet-123', got %q", output.Hits[0].ID)
	}
}

// TestGetXFSearch_PassesModeAndSortToInstalledXF indexes a two-tweet archive
// into an isolated XF_DB/XF_INDEX and checks that --xf-mode/--xf-sort change
// what the installed xf returns, not just what ntm echoes back.
func TestGetXFSearch_PassesModeAndSortToInstalledXF(t *testing.T) {
	if _, err := exec.LookPath("xf"); err != nil {
		t.Skip("xf not installed")
	}
	root := t.TempDir()
	data := filepath.Join(root, "archive", "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	tweets := `window.YTD.tweets.part0 = [
  {"tweet": {"id_str": "1234567890123456789", "created_at": "Wed Jan 08 12:00:00 +0000 2025",
    "full_text": "Hello world! This is a test tweet about Rust programming.", "lang": "en",
    "entities": {"hashtags": [], "user_mentions": [], "urls": []}}},
  {"tweet": {"id_str": "1234567890123456790", "created_at": "Thu Jan 09 14:30:00 +0000 2025",
    "full_text": "Learning about Tantivy search engine.", "lang": "en",
    "entities": {"hashtags": [], "user_mentions": [], "urls": []}}}
]`
	manifest := `window.YTD.manifest.part0 = {"userInfo": {"accountId": "1", "userName": "u", "displayName": "U"},
  "archiveInfo": {"sizeBytes": "1", "generationDate": "2025-01-01T00:00:00Z", "isPartialArchive": false}}`
	for name, content := range map[string]string{"tweets.js": tweets, "manifest.js": manifest} {
		if err := os.WriteFile(filepath.Join(data, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XF_DB", filepath.Join(root, "xf.db"))
	t.Setenv("XF_INDEX", filepath.Join(root, "index"))
	if out, err := exec.Command("xf", "index", filepath.Join(root, "archive")).CombinedOutput(); err != nil {
		t.Fatalf("xf index: %v\n%s", err, out)
	}

	ids := func(opts XFSearchOptions) []string {
		t.Helper()
		out, err := GetXFSearch(opts)
		if err != nil || !out.Success {
			t.Fatalf("GetXFSearch(%+v) = %+v, %v", opts, out, err)
		}
		got := make([]string, 0, len(out.Hits))
		for _, h := range out.Hits {
			got = append(got, h.ID)
		}
		return got
	}

	// Hybrid search also matches by embedding; lexical needs the keyword.
	if got := ids(XFSearchOptions{Query: "zzznomatch", Mode: "hybrid"}); len(got) == 0 {
		t.Fatalf("hybrid search returned nothing; the archive fixture did not index")
	}
	if got := ids(XFSearchOptions{Query: "zzznomatch", Mode: "lexical"}); len(got) != 0 {
		t.Fatalf("lexical search for an absent word = %v, want none (mode not passed to xf)", got)
	}
	oldest := []string{"1234567890123456789", "1234567890123456790"}
	if got := ids(XFSearchOptions{Query: "rust OR tantivy", Mode: "lexical", Sort: "date"}); !reflect.DeepEqual(got, oldest) {
		t.Fatalf("sort=date = %v, want %v", got, oldest)
	}
	newest := []string{"1234567890123456790", "1234567890123456789"}
	if got := ids(XFSearchOptions{Query: "rust OR tantivy", Mode: "lexical", Sort: "date-desc"}); !reflect.DeepEqual(got, newest) {
		t.Fatalf("sort=date-desc = %v, want %v", got, newest)
	}

	bad, err := GetXFSearch(XFSearchOptions{Query: "rust", Mode: "fuzzy"})
	if err != nil || bad.Success || bad.ErrorCode != ErrCodeInvalidFlag {
		t.Fatalf("GetXFSearch(mode=fuzzy) = %+v, %v; want INVALID_FLAG", bad, err)
	}
}

func TestGetXFSearch_DefaultLimit(t *testing.T) {
	tmpDir := t.TempDir()

	script := `#!/bin/sh
if echo "$@" | grep -q -- "--version"; then
  echo "xf 0.2.1"
  exit 0
fi
echo '[]'
`
	stubPath := filepath.Join(tmpDir, "xf")
	if err := os.WriteFile(stubPath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write xf stub: %v", err)
	}
	t.Setenv("PATH", tmpDir)

	output, err := GetXFSearch(XFSearchOptions{Query: "test"})
	if err != nil {
		t.Fatalf("GetXFSearch returned error: %v", err)
	}
	if !output.Success {
		t.Fatalf("expected success, got error: %s", output.Error)
	}
	if output.Count != 0 {
		t.Fatalf("expected 0 results, got %d", output.Count)
	}
}

func TestGetXFSearch_JSONOutput(t *testing.T) {
	tmpDir := t.TempDir()

	script := `#!/bin/sh
if echo "$@" | grep -q -- "--version"; then
  echo "xf 0.2.1"
  exit 0
fi
echo '[]'
`
	stubPath := filepath.Join(tmpDir, "xf")
	if err := os.WriteFile(stubPath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write xf stub: %v", err)
	}
	t.Setenv("PATH", tmpDir)

	output, err := GetXFSearch(XFSearchOptions{Query: "test"})
	if err != nil {
		t.Fatalf("GetXFSearch returned error: %v", err)
	}

	data, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("failed to marshal output: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal output: %v", err)
	}

	for _, key := range []string{"success", "timestamp", "query", "count", "hits"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("missing expected key %q in JSON output", key)
		}
	}
}

func TestGetXFStatus_MissingBinary(t *testing.T) {
	t.Setenv("PATH", "")

	output, err := GetXFStatus()
	if err != nil {
		t.Fatalf("GetXFStatus returned error: %v", err)
	}
	if output.Success {
		t.Fatalf("expected failure when xf missing")
	}
	if output.ErrorCode != ErrCodeDependencyMissing {
		t.Fatalf("expected %s, got %s", ErrCodeDependencyMissing, output.ErrorCode)
	}
	if output.XFAvailable {
		t.Fatalf("expected xf_available=false")
	}
}
