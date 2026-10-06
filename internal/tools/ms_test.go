package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// indexTestMSSkill indexes one skill into an isolated ms root (HOME, MS_ROOT
// and MS_CONFIG all point into a temp dir) with the installed ms.
func indexTestMSSkill(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ms"); err != nil {
		t.Skip("ms not installed")
	}

	root := t.TempDir()
	skillDir := filepath.Join(root, "skills", "commit-workflow")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skill := "---\nname: commit-workflow\ntags: [git, commit]\n---\n\n# Commit Workflow\n\n" +
		"Stage related changes together and write a commit message that explains why.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", root)
	t.Setenv("MS_ROOT", filepath.Join(root, ".ms"))
	t.Setenv("MS_CONFIG", filepath.Join(root, ".ms", "config.toml"))
	for _, args := range [][]string{{"--robot", "init"}, {"--robot", "index"}} {
		cmd := exec.Command("ms", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ms %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

func TestMSSearchAndShowReadInstalledMS(t *testing.T) {
	indexTestMSSkill(t)
	adapter := NewMSAdapter()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	matches, err := adapter.Search(ctx, "commit")
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(matches) == 0 || matches[0].ID != "commit-workflow" ||
		!strings.Contains(matches[0].Description, "Stage related changes") || matches[0].Score <= 0 {
		t.Fatalf("Search() = %+v, want the commit-workflow skill with description and score", matches)
	}

	raw, err := adapter.Show(ctx, "commit-workflow")
	if err != nil {
		t.Fatalf("Show() error: %v", err)
	}
	var skill struct {
		ID    string `json:"id"`
		Layer string `json:"layer"`
	}
	if err := json.Unmarshal(raw, &skill); err != nil || skill.ID != "commit-workflow" || skill.Layer != "project" {
		t.Fatalf("Show() = %s (decode err %v), want the skill object itself", raw, err)
	}

	_, err = adapter.Show(ctx, "no-such-skill")
	if err == nil || !strings.Contains(err.Error(), "Skill not found") || strings.Contains(err.Error(), "WARN") {
		t.Fatalf("Show(missing) error = %v, want ms's not-found line without log noise", err)
	}
}

func TestCLIErrorLine(t *testing.T) {
	t.Parallel()

	stderr := "2026-10-06T20:36:10Z  WARN fsqlite_core::connection: WAL-FEC requires a caller-owned native runtime\n" +
		"Error: Skill not found: skill not found: x\n"
	if got := cliErrorLine(stderr); got != "Error: Skill not found: skill not found: x" {
		t.Fatalf("cliErrorLine() = %q", got)
	}
	if got := cliErrorLine("  plain failure \n"); got != "plain failure" {
		t.Fatalf("cliErrorLine() without an Error: line = %q", got)
	}
}
