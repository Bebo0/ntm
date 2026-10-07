package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/ntm/internal/codeblock"
)

func TestParseFileSpec(t *testing.T) {
	tests := []struct {
		input     string
		wantPath  string
		wantStart int
		wantEnd   int
	}{
		{"file.go", "file.go", 0, 0},
		{"src/auth.py", "src/auth.py", 0, 0},
		{"file.go:10-50", "file.go", 10, 50},
		{"file.go:10-", "file.go", 10, 0},
		{"file.go:-50", "file.go", 0, 50},
		{"file.go:25", "file.go", 25, 25},
		{"/abs/path/file.go:1-10", "/abs/path/file.go", 1, 10},
		{"file.go:abc-1", "file.go:abc-1", 0, 0},                 // not a line range suffix
		{`C:\proj-1\main.go`, `C:\proj-1\main.go`, 0, 0},         // Windows drive path (no range)
		{`C:\proj-1\main.go:12-34`, `C:\proj-1\main.go`, 12, 34}, // Windows drive path with range
		{`C:\proj-1\main.go:12-`, `C:\proj-1\main.go`, 12, 0},    // Windows drive path with open range
		{`C:\proj-1\main.go:-34`, `C:\proj-1\main.go`, 0, 34},    // Windows drive path with end-only range
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			spec, err := ParseFileSpec(tt.input)
			if err != nil {
				t.Fatalf("ParseFileSpec(%q) error: %v", tt.input, err)
			}
			if spec.Path != tt.wantPath {
				t.Errorf("Path = %q, want %q", spec.Path, tt.wantPath)
			}
			if spec.StartLine != tt.wantStart {
				t.Errorf("StartLine = %d, want %d", spec.StartLine, tt.wantStart)
			}
			if spec.EndLine != tt.wantEnd {
				t.Errorf("EndLine = %d, want %d", spec.EndLine, tt.wantEnd)
			}
		})
	}
}

func TestDetectLanguage(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"file.go", "go"},
		{"src/auth.py", "python"},
		{"app.js", "javascript"},
		{"app.tsx", "typescript"},
		{"Makefile", "makefile"},
		{"Dockerfile", "dockerfile"},
		{"unknown.xyz", ""},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := codeblock.DetectLanguage(tt.path)
			if got != tt.want {
				t.Errorf("DetectLanguage(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestInjectFiles(t *testing.T) {
	// Create temp test file
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.go")
	content := "package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}"
	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	specs := []FileSpec{{Path: testFile}}
	result, err := InjectFiles(specs, "Review this code")
	if err != nil {
		t.Fatalf("InjectFiles error: %v", err)
	}

	// Check structure
	if !strings.Contains(result, "# File:") {
		t.Error("Missing file header")
	}
	if !strings.Contains(result, "```go") {
		t.Error("Missing language tag")
	}
	if !strings.Contains(result, content) {
		t.Error("Missing file content")
	}
	if !strings.Contains(result, "---\n\nReview this code") {
		t.Error("Missing prompt separator")
	}
}

func TestFileContextBlockIsExactlyWhatInjectFilesPrepends(t *testing.T) {
	tmpDir := t.TempDir()
	first := filepath.Join(tmpDir, "a.go")
	second := filepath.Join(tmpDir, "b.txt")
	if err := os.WriteFile(first, []byte("package a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("one\ntwo\nthree\n"), 0644); err != nil {
		t.Fatal(err)
	}
	specs := []FileSpec{{Path: first}, {Path: second, StartLine: 2, EndLine: 3}}

	block, err := FileContextBlock(specs)
	if err != nil {
		t.Fatalf("FileContextBlock error: %v", err)
	}
	want := "# File: " + first + "\n```go\npackage a\n```\n\n" +
		"# File: " + second + " (lines 2-3)\n```\ntwo\nthree\n```\n\n---\n\n"
	if block != want {
		t.Fatalf("FileContextBlock = %q, want %q", block, want)
	}
	for _, body := range []string{"Agent #1 body", "Agent #2 body", ""} {
		injected, err := InjectFiles(specs, body)
		if err != nil {
			t.Fatalf("InjectFiles error: %v", err)
		}
		if injected != block+body {
			t.Fatalf("InjectFiles(%q) = %q, want block+body", body, injected)
		}
	}

	empty, err := FileContextBlock(nil)
	if err != nil || empty != "" {
		t.Fatalf("FileContextBlock(nil) = %q, %v; want empty", empty, err)
	}
	if _, err := FileContextBlock([]FileSpec{{Path: filepath.Join(tmpDir, "missing.go")}}); err == nil {
		t.Fatal("FileContextBlock with a missing file succeeded, want error")
	}
}

func TestInjectFilesWithLineRange(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	lines := "line1\nline2\nline3\nline4\nline5\n"
	if err := os.WriteFile(testFile, []byte(lines), 0644); err != nil {
		t.Fatal(err)
	}

	specs := []FileSpec{{Path: testFile, StartLine: 2, EndLine: 4}}
	result, err := InjectFiles(specs, "Check these lines")
	if err != nil {
		t.Fatalf("InjectFiles error: %v", err)
	}

	if !strings.Contains(result, "line2") {
		t.Error("Should contain line2")
	}
	if !strings.Contains(result, "line4") {
		t.Error("Should contain line4")
	}
	if strings.Contains(result, "line1\n") {
		t.Error("Should not contain line1")
	}
	if strings.Contains(result, "line5") {
		t.Error("Should not contain line5")
	}
}

// bd-4we2d: readFileRange's no-range branch used unbounded io.ReadAll(f).
// Even though InjectFiles tries to Stat() first, the file can grow (or a
// symlink can be swapped) between Stat and Open+Read, allocating the full
// post-growth content into memory before the post-check fires. The fix
// wraps the read with io.LimitReader(f, MaxFileSize+1) and rejects
// overflow inside readFileRange itself — independent of the pre-Stat
// check.
func TestReadFileRange_RejectsOversizeFile(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "big.txt")
	// Write MaxFileSize+1 bytes — should trip the cap by exactly 1 byte.
	body := make([]byte, MaxFileSize+1)
	for i := range body {
		body[i] = 'a'
	}
	if err := os.WriteFile(testFile, body, 0644); err != nil {
		t.Fatal(err)
	}

	_, err := readFileRange(FileSpec{Path: testFile})
	if err == nil {
		t.Fatalf("readFileRange returned nil error for oversize file, want cap error")
	}
	if !strings.Contains(err.Error(), "exceeds limit") {
		t.Errorf("error = %q, want it to mention the cap", err.Error())
	}
}

// Pin the happy path: a file exactly at MaxFileSize must still read
// successfully. This guards against accidentally tightening the cap to
// MaxFileSize-1 when wiring io.LimitReader.
func TestReadFileRange_AllowsExactlyAtCap(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "atcap.txt")
	body := make([]byte, MaxFileSize)
	for i := range body {
		body[i] = 'b'
	}
	if err := os.WriteFile(testFile, body, 0644); err != nil {
		t.Fatal(err)
	}

	got, err := readFileRange(FileSpec{Path: testFile})
	if err != nil {
		t.Fatalf("readFileRange at exactly MaxFileSize errored: %v", err)
	}
	// Trailing newlines are stripped; we wrote no '\n' so length should match.
	if len(got) != MaxFileSize {
		t.Errorf("returned content len = %d, want %d", len(got), MaxFileSize)
	}
}

func TestIsBinary(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"empty", "", false},
		{"text", "Hello, World!", false},
		{"with newlines", "line1\nline2\nline3", false},
		{"with tabs", "col1\tcol2\tcol3", false},
		{"null byte", "hello\x00world", true},
		{"binary with nulls", string([]byte{0x89, 0x50, 0x4E, 0x47, 0x00, 0x00, 0x00}), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isBinary(tt.content)
			if got != tt.want {
				t.Errorf("isBinary() = %v, want %v", got, tt.want)
			}
		})
	}
}
