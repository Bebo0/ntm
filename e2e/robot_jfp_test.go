//go:build e2e
// +build e2e

// Package e2e contains end-to-end tests for NTM robot mode commands.
// robot_jfp_test.go validates robot wrappers for the JeffreysPrompts CLI.
//
// Bead: bd-3ury8 - Task: E2E Tests: JFP robot wrapper
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type jfpEnvelope struct {
	Success   bool   `json:"success"`
	Timestamp string `json:"timestamp"`
	Error     string `json:"error,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
	Hint      string `json:"hint,omitempty"`
}

type jfpStatusOutput struct {
	jfpEnvelope
	JFPAvailable bool   `json:"jfp_available"`
	Healthy      bool   `json:"healthy"`
	Version      string `json:"version,omitempty"`
}

type jfpListOutput struct {
	jfpEnvelope
	Count   int             `json:"count"`
	Prompts json.RawMessage `json:"prompts"`
}

type jfpSearchOutput struct {
	jfpEnvelope
	Query   string          `json:"query"`
	Count   int             `json:"count"`
	Results json.RawMessage `json:"results"`
}

type jfpShowOutput struct {
	jfpEnvelope
	ID     string          `json:"id"`
	Prompt json.RawMessage `json:"prompt,omitempty"`
}

func runJFPCommand(t *testing.T, suite *TestSuite, ntmPath string, env []string, args ...string) ([]byte, int, time.Duration, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(ctx, ntmPath, args...)
	if env != nil {
		cmd.Env = env
	}
	output, err := cmd.CombinedOutput()
	duration := time.Since(start)

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	if ctx.Err() == context.DeadlineExceeded {
		suite.Logger().Log("[E2E-JFP] cmd=%s exit=%d duration=%s timeout=true", strings.Join(args, " "), exitCode, duration)
		slog.Error(fmt.Sprintf("[E2E-JFP] cmd=%s exit=%d timeout=true", strings.Join(args, " "), exitCode))
		return output, exitCode, duration, fmt.Errorf("command timed out after 60s")
	}

	suite.Logger().Log("[E2E-JFP] cmd=%s exit=%d duration=%s bytes=%d", strings.Join(args, " "), exitCode, duration, len(output))
	slog.Info(fmt.Sprintf("[E2E-JFP] cmd=%s exit=%d bytes=%d", strings.Join(args, " "), exitCode, len(output)))
	return output, exitCode, duration, err
}

func parseJFPJSON(t *testing.T, suite *TestSuite, label string, output []byte, v interface{}) {
	t.Helper()
	if err := json.Unmarshal(output, v); err != nil {
		t.Fatalf("[E2E-JFP] %s JSON parse failed: %v output=%s", label, err, string(output))
	}
	suite.Logger().LogJSON("[E2E-JFP] "+label+" parsed", v)
}

func stripPathEnv() []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "PATH=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "PATH=")
	return env
}

func extractPromptID(raw json.RawMessage) (string, int) {
	var items []map[string]interface{}
	if err := json.Unmarshal(raw, &items); err != nil {
		return "", 0
	}
	for _, item := range items {
		if id, ok := item["id"].(string); ok && id != "" {
			return id, len(items)
		}
		if id, ok := item["slug"].(string); ok && id != "" {
			return id, len(items)
		}
		if id, ok := item["name"].(string); ok && id != "" {
			return id, len(items)
		}
	}
	return "", len(items)
}

func TestE2E_RobotJFP_MissingBinary(t *testing.T) {
	CommonE2EPrerequisites(t)

	suite := NewTestSuite(t, "robot_jfp_missing")
	defer suite.Teardown()

	ntmPath, err := exec.LookPath("ntm")
	if err != nil {
		t.Skip("ntm not found on PATH")
	}
	if !supportsRobotFlagInHelp(ntmPath, "--robot-jfp-status") {
		t.Skip("robot-jfp-status not supported by current ntm binary")
	}

	output, exitCode, _, err := runJFPCommand(t, suite, ntmPath, stripPathEnv(), "--robot-jfp-status")
	if err != nil {
		suite.Logger().Log("[E2E-JFP] status missing binary err=%v", err)
	}

	var status jfpStatusOutput
	parseJFPJSON(t, suite, "status_missing", output, &status)

	suite.Logger().Log("[E2E-JFP] status missing binary exit=%d success=%t", exitCode, status.Success)
	if status.Success {
		t.Fatalf("[E2E-JFP] expected failure when jfp missing")
	}
	if status.ErrorCode != "DEPENDENCY_MISSING" {
		t.Fatalf("[E2E-JFP] expected DEPENDENCY_MISSING, got=%s", status.ErrorCode)
	}
	slog.Error(fmt.Sprintf("[E2E-JFP] error=%s hint=%s", status.Error, status.Hint))
}

func TestE2E_RobotJFP_ListSearchShowInstalled(t *testing.T) {
	CommonE2EPrerequisites(t)

	if _, err := exec.LookPath("jfp"); err != nil {
		t.Skip("jfp not installed; skipping list/search/show/installed")
	}

	suite := NewTestSuite(t, "robot_jfp_live")
	defer suite.Teardown()

	ntmPath, err := exec.LookPath("ntm")
	if err != nil {
		t.Skip("ntm not found on PATH")
	}
	if !supportsRobotFlagInHelp(ntmPath, "--robot-jfp-list") {
		t.Skip("robot-jfp-list not supported by current ntm binary")
	}

	// List
	output, exitCode, _, err := runJFPCommand(t, suite, ntmPath, nil, "--robot-jfp-list")
	if err != nil {
		t.Fatalf("[E2E-JFP] list failed: %v", err)
	}

	var list jfpListOutput
	parseJFPJSON(t, suite, "list", output, &list)
	slog.Info(fmt.Sprintf("[E2E-JFP] cmd=robot-jfp-list exit=%d count=%d", exitCode, list.Count))

	if !list.Success {
		slog.Error(fmt.Sprintf("[E2E-JFP] error=%s hint=%s", list.Error, list.Hint))
		t.Fatalf("[E2E-JFP] list failed: %s", list.Error)
	}
	if !json.Valid(list.Prompts) {
		t.Fatalf("[E2E-JFP] list prompts JSON invalid")
	}

	promptID, promptCount := extractPromptID(list.Prompts)
	suite.Logger().Log("[E2E-JFP] list count=%d promptID=%s", promptCount, promptID)

	// Search
	if supportsRobotFlagInHelp(ntmPath, "--robot-jfp-search") {
		output, exitCode, _, err = runJFPCommand(t, suite, ntmPath, nil, "--robot-jfp-search=debugging")
		if err != nil {
			t.Fatalf("[E2E-JFP] search failed: %v", err)
		}

		var search jfpSearchOutput
		parseJFPJSON(t, suite, "search", output, &search)
		slog.Info(fmt.Sprintf("[E2E-JFP] cmd=robot-jfp-search exit=%d count=%d", exitCode, search.Count))

		if !search.Success {
			slog.Error(fmt.Sprintf("[E2E-JFP] error=%s hint=%s", search.Error, search.Hint))
			t.Fatalf("[E2E-JFP] search failed: %s", search.Error)
		}
		if !json.Valid(search.Results) {
			t.Fatalf("[E2E-JFP] search results JSON invalid")
		}
	} else {
		suite.Logger().Log("[E2E-JFP] skipping search: flag not supported")
	}

	// Show (if we found an ID)
	if promptID != "" && supportsRobotFlagInHelp(ntmPath, "--robot-jfp-show") {
		output, exitCode, _, err = runJFPCommand(t, suite, ntmPath, nil, fmt.Sprintf("--robot-jfp-show=%s", promptID))
		if err != nil {
			t.Fatalf("[E2E-JFP] show failed: %v", err)
		}

		var show jfpShowOutput
		parseJFPJSON(t, suite, "show", output, &show)
		slog.Info(fmt.Sprintf("[E2E-JFP] cmd=robot-jfp-show exit=%d count=%d", exitCode, 1))

		if !show.Success {
			slog.Error(fmt.Sprintf("[E2E-JFP] error=%s hint=%s", show.Error, show.Hint))
			t.Fatalf("[E2E-JFP] show failed: %s", show.Error)
		}
		if !json.Valid(show.Prompt) {
			t.Fatalf("[E2E-JFP] show prompt JSON invalid")
		}
	} else {
		suite.Logger().Log("[E2E-JFP] skipping show: no prompt ID or flag unsupported")
	}
}

func supportsRobotFlagInHelp(ntmPath, flag string) bool {
	cmd := exec.Command(ntmPath, "--help")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), flag)
}
