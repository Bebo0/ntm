//go:build e2e
// +build e2e

// Package e2e contains end-to-end tests for NTM robot mode commands.
// robot_bulk_assign_test.go validates --robot-bulk-assign with various configurations.
//
// Bead: bd-1klou - Task: E2E Tests: Robot Bulk Assign
package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/assignment"
	"github.com/Dicklesworthstone/ntm/internal/sqliteutil"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
	"github.com/Dicklesworthstone/ntm/tests/testutil"
)

// BulkAssignOutput represents the JSON response from --robot-bulk-assign.
type BulkAssignOutput struct {
	Success          bool                   `json:"success"`
	Session          string                 `json:"session"`
	Strategy         string                 `json:"strategy"`
	Timestamp        string                 `json:"timestamp"`
	Assignments      []BulkAssignAssignment `json:"assignments"`
	Summary          BulkAssignSummary      `json:"summary"`
	UnassignedBeads  []string               `json:"unassigned_beads"`
	UnassignedPanes  []string               `json:"unassigned_panes"`
	DryRun           bool                   `json:"dry_run,omitempty"`
	AllocationSource string                 `json:"allocation_source,omitempty"`
	Error            string                 `json:"error,omitempty"`
	ErrorCode        string                 `json:"error_code,omitempty"`
}

// BulkAssignAssignment represents a single pane-to-bead allocation.
type BulkAssignAssignment struct {
	Pane              string `json:"pane"`
	PaneID            string `json:"pane_id"`
	Bead              string `json:"bead"`
	BeadTitle         string `json:"bead_title"`
	Reason            string `json:"reason"`
	AgentType         string `json:"agent_type"`
	Status            string `json:"status"`
	PromptSent        bool   `json:"prompt_sent"`
	Claimed           bool   `json:"claimed"`
	ClaimActor        string `json:"claim_actor,omitempty"`
	IdempotencyKey    string `json:"idempotency_key,omitempty"`
	DispatchReceiptID string `json:"dispatch_receipt_id,omitempty"`
	Error             string `json:"error,omitempty"`
}

// BulkAssignSummary aggregates assignment stats.
type BulkAssignSummary struct {
	TotalPanes int `json:"total_panes"`
	Assigned   int `json:"assigned"`
	Skipped    int `json:"skipped"`
	Failed     int `json:"failed"`
}

// runBulkAssignCmd executes ntm --robot-bulk-assign with the given flags.
// Uses the shared E2E command timeout to remain bounded on loaded hosts.
// These planning/envelope scenarios run without Agent Mail and with beads
// that name no files, so they opt out of the default file reservations the
// way the `ntm assign` e2e scenarios pass --reserve-files=false.
func runBulkAssignCmd(t *testing.T, suite *TestSuite, session string, flags ...string) (*BulkAssignOutput, []byte, error) {
	t.Helper()

	args := []string{fmt.Sprintf("--robot-bulk-assign=%s", session), "--reserve-files=false"}
	args = append(args, flags...)

	// Use context with timeout to prevent indefinite hangs (bd-brzap fix)
	ctx, cancel := context.WithTimeout(context.Background(), defaultRunTimeout)
	defer cancel()

	ntmPath, resolveErr := ensureE2ENTMBin()
	if resolveErr != nil {
		return nil, nil, fmt.Errorf("resolve E2E ntm binary: %w", resolveErr)
	}
	cmd := exec.CommandContext(ctx, ntmPath, args...)
	group, groupErr := testutil.NewProcessGroupForTest(ctx, cmd)
	if groupErr != nil {
		return nil, nil, fmt.Errorf("create owned process group: %w", groupErr)
	}
	cmd.Cancel = func() error {
		return group.Signal(os.Kill)
	}
	cmd.WaitDelay = 2 * time.Second
	output, commandErr := cmd.CombinedOutput()
	if closeErr := group.Close(); closeErr != nil {
		return nil, output, fmt.Errorf("close owned process group: %w", closeErr)
	}

	// Check for timeout
	if ctx.Err() == context.DeadlineExceeded {
		suite.Logger().Log("[E2E-BULK-ASSIGN] Command timed out after %s: args=%v output=%s", defaultRunTimeout, args, truncateString(string(output), 1000))
		return nil, output, fmt.Errorf("command timed out after %s", defaultRunTimeout)
	}

	suite.Logger().Log("[E2E-BULK-ASSIGN] args=%v bytes=%d", args, len(output))

	var result BulkAssignOutput
	if jsonErr := json.Unmarshal(output, &result); jsonErr != nil {
		suite.Logger().Log("[E2E-BULK-ASSIGN] JSON parse failed: %v output=%s", jsonErr, string(output))
		return nil, output, commandErr
	}

	suite.Logger().LogJSON("[E2E-BULK-ASSIGN] Result", result)
	return &result, output, commandErr
}

func runRobotFixtureTool(t *testing.T, fixture *robotProcessFixture, timeout time.Duration, path string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = fixture.projectDir
	cmd.Env = append([]string(nil), fixture.env...)
	group, groupErr := testutil.NewProcessGroupForTest(ctx, cmd)
	if groupErr != nil {
		t.Fatalf("create owned process group for %s %q: %v", path, args, groupErr)
	}
	cmd.Cancel = func() error {
		return group.Signal(os.Kill)
	}
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	commandErr := cmd.Run()
	if closeErr := group.Close(); closeErr != nil {
		t.Fatalf("close owned process group for %s %q: %v", path, args, closeErr)
	}
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("%s %q timed out after %v; stdout=%s stderr=%s", path, args, timeout, stdout.Bytes(), stderr.Bytes())
	}
	if commandErr != nil {
		t.Fatalf("%s %q: %v stdout=%s stderr=%s", path, args, commandErr, stdout.Bytes(), stderr.Bytes())
	}
	return stdout.Bytes()
}

func prepareBulkAssignAgentPanes(t *testing.T, fixture *robotProcessFixture, total int) []tmux.Pane {
	t.Helper()
	if total < 1 {
		t.Fatalf("[E2E-BULK-ASSIGN] pane total=%d, want at least one", total)
	}

	for index := 1; index < total; index++ {
		paneID := strings.TrimSpace(fixture.mustTMUXOutput(t,
			"split-window", "-d", "-h", "-t", fixture.session,
			"-c", fixture.projectDir, "-P", "-F", "#{pane_id}",
			"/bin/bash --noprofile --norc -i",
		))
		if paneID == "" {
			t.Fatalf("[E2E-BULK-ASSIGN] create pane %d of %d returned no physical ID", index+1, total)
		}
		// Rebalance after every split so large fixtures do not repeatedly divide
		// one shrinking horizontal cell before the final layout pass.
		fixture.mustTMUXOutput(t, "select-layout", "-t", fixture.session, "tiled")
	}

	fixture.mustTMUXOutput(t, "select-layout", "-t", fixture.session, "tiled")
	listOutput := fixture.mustTMUXOutput(t,
		"list-panes", "-t", fixture.session,
		"-F", "#{pane_id}\t#{window_index}\t#{pane_index}",
	)
	panes := make([]tmux.Pane, 0, total)
	for _, line := range strings.Split(strings.TrimSpace(listOutput), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			t.Fatalf("[E2E-BULK-ASSIGN] malformed private tmux pane row %q", line)
		}
		windowIndex, err := strconv.Atoi(fields[1])
		if err != nil {
			t.Fatalf("[E2E-BULK-ASSIGN] parse window index in %q: %v", line, err)
		}
		paneIndex, err := strconv.Atoi(fields[2])
		if err != nil {
			t.Fatalf("[E2E-BULK-ASSIGN] parse pane index in %q: %v", line, err)
		}
		panes = append(panes, tmux.Pane{ID: fields[0], WindowIndex: windowIndex, Index: paneIndex})
	}
	panes = tmux.SortPanesByTopology(panes)
	if len(panes) != total {
		t.Fatalf("[E2E-BULK-ASSIGN] listed panes=%+v, want %d", panes, total)
	}

	seenPaneIDs := make(map[string]struct{}, total)
	for index := range panes {
		if _, duplicate := seenPaneIDs[panes[index].ID]; duplicate {
			t.Fatalf("[E2E-BULK-ASSIGN] duplicate physical pane ID %q", panes[index].ID)
		}
		seenPaneIDs[panes[index].ID] = struct{}{}
		panes[index].NTMIndex = index + 1
		panes[index].Title = fmt.Sprintf("%s__cod_%d", fixture.session, index+1)
		fixture.mustTMUXOutput(t, "select-pane", "-t", panes[index].ID, "-T", panes[index].Title)
	}
	return panes
}

func prepareBulkAssignBeads(t *testing.T, fixture *robotProcessFixture, total int) []string {
	t.Helper()
	brPath, err := exec.LookPath("br")
	if err != nil {
		t.Fatalf("real br is required for hermetic bulk assignment E2E: %v", err)
	}
	runRobotFixtureTool(t, fixture, defaultRunTimeout, brPath, "init", "--prefix=rbe2e", "--json")
	beads := make([]string, 0, total)
	seen := make(map[string]struct{}, total)
	for index := 0; index < total; index++ {
		beadID := strings.TrimSpace(string(runRobotFixtureTool(t, fixture, defaultRunTimeout, brPath,
			"create", fmt.Sprintf("Hermetic bulk topology %d", index+1), "--type=task", "--priority=1", "--silent",
		)))
		if beadID == "" || strings.ContainsAny(beadID, " \t\r\n") {
			t.Fatalf("[E2E-BULK-ASSIGN] unexpected br create output %q", beadID)
		}
		if _, duplicate := seen[beadID]; duplicate {
			t.Fatalf("[E2E-BULK-ASSIGN] duplicate hermetic bead ID %q", beadID)
		}
		seen[beadID] = struct{}{}
		beads = append(beads, beadID)
	}
	return beads
}

// runBulkAssignFixture runs one bulk assignment against a hermetic fixture.
// Like runBulkAssignCmd, it opts out of the default file reservations: the
// fixture has no Agent Mail and its beads name no files.
func runBulkAssignFixture(t *testing.T, fixture *robotProcessFixture, flags ...string) (BulkAssignOutput, robotBoundaryProcessResult) {
	t.Helper()
	args := []string{"--robot-bulk-assign=" + fixture.session, "--robot-format=json", "--reserve-files=false"}
	args = append(args, flags...)
	process := runBuiltRobotProcessWithin(t, defaultRunTimeout, fixture.ntmPath, fixture.projectDir, fixture.env, args...)
	if len(bytes.TrimSpace(process.stderr)) != 0 {
		t.Fatalf("[E2E-BULK-ASSIGN] unexpected stderr: exit=%d stdout=%s stderr=%s", process.exitCode, process.stdout, process.stderr)
	}
	var result BulkAssignOutput
	decodeSingleRobotJSON(t, process.stdout, &result)
	return result, process
}

func installBulkAssignPlanningFixtures(
	t *testing.T,
	fixture *robotProcessFixture,
	baseEnv []string,
	triagePayload, planPayload, planMode, labelMode string,
) ([]string, string, string) {
	t.Helper()
	realBR, err := exec.LookPath("br")
	if err != nil {
		t.Fatalf("real br is required for bulk planning fixture: %v", err)
	}
	binDir := filepath.Join(fixture.root, "bulk-planning-bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatalf("create bulk planning fixture bin: %v", err)
	}
	triagePath := filepath.Join(fixture.root, "bulk-triage.json")
	planPath := filepath.Join(fixture.root, "bulk-plan.json")
	bvTrace := filepath.Join(fixture.root, "bulk-bv.trace")
	brTrace := filepath.Join(fixture.root, "bulk-br.trace")
	if err := os.WriteFile(triagePath, []byte(triagePayload), 0o600); err != nil {
		t.Fatalf("write bulk triage payload: %v", err)
	}
	if err := os.WriteFile(planPath, []byte(planPayload), 0o600); err != nil {
		t.Fatalf("write bulk plan payload: %v", err)
	}
	bvScript := strings.Join([]string{
		"#!/bin/sh",
		`printf '%s\n' "$*" >> "$NTM_E2E_BULK_BV_TRACE"`,
		`case "$1" in`,
		`  --robot-triage) cat "$NTM_E2E_BULK_TRIAGE" ;;`,
		`  --robot-plan)`,
		`    if [ "$NTM_E2E_BULK_PLAN_MODE" = "failure" ]; then`,
		`      printf 'injected bulk plan verification failure\n' >&2`,
		`      exit 70`,
		`    fi`,
		`    cat "$NTM_E2E_BULK_PLAN"`,
		`    ;;`,
		`  *) printf 'unexpected bv args: %s\n' "$*" >&2; exit 64 ;;`,
		`esac`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(binDir, "bv"), []byte(bvScript), 0o700); err != nil {
		t.Fatalf("write bulk bv fixture: %v", err)
	}
	brScript := strings.Join([]string{
		"#!/bin/sh",
		`printf '%s\n' "$*" >> "$NTM_E2E_BULK_BR_TRACE"`,
		`command_name="${1:-}"`,
		`if [ "$command_name" = "--lock-timeout" ]; then`,
		`  if [ "${2:-}" != "5000" ] || [ "$#" -lt 3 ]; then`,
		`    printf 'unexpected br lock prefix: %s\n' "$*" >&2`,
		`    exit 64`,
		`  fi`,
		`  command_name=$3`,
		`fi`,
		`if [ "$NTM_E2E_BULK_LABEL_MODE" = "empty" ]; then`,
		`  case "$command_name" in`,
		`    ready|list) printf '[]\n'; exit 0 ;;`,
		`  esac`,
		`fi`,
		"exec " + tmux.ShellQuote(realBR) + ` "$@"`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(binDir, "br"), []byte(brScript), 0o700); err != nil {
		t.Fatalf("write bulk br fixture: %v", err)
	}
	env := mergeRobotProcessEnv(baseEnv, map[string]string{
		"PATH":                    binDir + string(os.PathListSeparator) + atomicAssignmentEnvValue(fixture.env, "PATH"),
		"NTM_E2E_BULK_TRIAGE":     triagePath,
		"NTM_E2E_BULK_PLAN":       planPath,
		"NTM_E2E_BULK_PLAN_MODE":  planMode,
		"NTM_E2E_BULK_LABEL_MODE": labelMode,
		"NTM_E2E_BULK_BV_TRACE":   bvTrace,
		"NTM_E2E_BULK_BR_TRACE":   brTrace,
	})
	return env, bvTrace, brTrace
}

func readBulkAssignFixtureBead(t *testing.T, fixture *robotProcessFixture, beadID string) []byte {
	t.Helper()
	brPath, err := exec.LookPath("br")
	if err != nil {
		t.Fatalf("real br is required for bulk Beads assertion: %v", err)
	}
	return runRobotFixtureTool(t, fixture, defaultRunTimeout, brPath, "show", beadID, "--json")
}

func assertBulkAssignTraceEmpty(t *testing.T, path, surface string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatalf("read bulk %s trace: %v", surface, err)
	}
	if len(bytes.TrimSpace(data)) != 0 {
		t.Fatalf("bulk safety failure reached %s: %s", surface, data)
	}
}

func assertBulkAssignFixtureHasNoProjectionRows(t *testing.T, fixture *robotProcessFixture) {
	t.Helper()
	statePath := filepath.Join(fixture.configDir, "ntm", "state.db")
	database, err := sql.Open(sqliteutil.DriverName, sqliteutil.FileDSN(statePath, "busy_timeout(5000)", "foreign_keys(ON)"))
	if err != nil {
		t.Fatalf("open bulk runtime state store: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("close bulk runtime state store: %v", err)
		}
	})
	for _, table := range []string{
		"runtime_sessions",
		"runtime_agents",
		"runtime_work",
		"runtime_coordination",
		"runtime_quota",
		"source_health",
	} {
		var count int
		if err := database.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			t.Fatalf("count bulk runtime projection table %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("bulk safety failure persisted %d row(s) in %s before policy validation", count, table)
		}
	}
}

func assertBulkAssignFixtureHasNoLedger(t *testing.T, fixture *robotProcessFixture, beadID string) {
	t.Helper()
	for _, path := range []string{
		filepath.Join(fixture.root, "home", ".ntm", "sessions", fixture.session, "assignments.json"),
		filepath.Join(fixture.root, "home", ".ntm", "sessions", fixture.session, "assignments.json.bak"),
	} {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("inspect unexpected bulk assignment ledger %s: %v", path, err)
		}
		t.Fatalf("bulk safety failure created ledger %s while rejecting %s: mode=%s size=%d", path, beadID, info.Mode(), info.Size())
	}
}

func TestE2E_RobotBulkAssign_RequiresSession(t *testing.T) {
	CommonE2EPrerequisites(t)

	suite := NewTestSuite(t, "bulk_assign_requires_session")
	defer suite.Teardown()

	// Test that empty session (--robot-bulk-assign=) falls through to help
	// This is expected behavior - the flag needs a value
	ntmPath, err := ensureE2ENTMBin()
	if err != nil {
		t.Fatalf("resolve E2E ntm binary: %v", err)
	}
	helpEnv := mergeRobotProcessEnv(os.Environ(), map[string]string{"NO_COLOR": "1", "TERM": "dumb"})
	process := runBuiltRobotProcessWithin(t, defaultRunTimeout, ntmPath, "", helpEnv, "--robot-bulk-assign=")
	if process.exitCode != 0 || len(bytes.TrimSpace(process.stderr)) != 0 {
		t.Fatalf("[E2E-BULK-ASSIGN] empty session help exit=%d stdout=%s stderr=%s",
			process.exitCode, process.stdout, process.stderr)
	}
	output := append([]byte(nil), process.stdout...)

	suite.Logger().Log("[E2E-BULK-ASSIGN] empty session shows help: bytes=%d", len(output))

	// With an empty value, NTM must render its custom root help rather than trigger
	// a robot command or emit an unrelated success document.
	help := string(output)
	for _, marker := range []string{
		"Named Tmux Session Manager for AI Agents",
		"Create session and launch agents",
		"Interactive session dashboard",
		"ntm spawn myproject --cc=2 --cod=2",
		"Aliases: cnt sat ant rnt lnt snt",
	} {
		if !strings.Contains(help, marker) {
			t.Fatalf("[E2E-BULK-ASSIGN] empty session output omitted help marker %q: %s", marker, help[:min(500, len(help))])
		}
	}
	if json.Valid(bytes.TrimSpace(output)) {
		t.Fatalf("[E2E-BULK-ASSIGN] empty session unexpectedly emitted a robot JSON document: %s", output)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestE2E_RobotBulkAssign_RequiresBVOrAllocation(t *testing.T) {
	CommonE2EPrerequisites(t)

	fixture := newRobotProcessFixture(t, "bulk-requires-source")
	result, process := runBulkAssignFixture(t, fixture)
	if process.exitCode != 1 || result.Success || result.ErrorCode != "INVALID_FLAG" ||
		result.Assignments == nil || result.UnassignedBeads == nil || result.UnassignedPanes == nil ||
		result.Summary != (BulkAssignSummary{}) ||
		(!strings.Contains(result.Error, "--from-bv") && !strings.Contains(result.Error, "--allocation")) {
		t.Fatalf("[E2E-BULK-ASSIGN] missing-source envelope: exit=%d result=%+v", process.exitCode, result)
	}
}

func TestE2E_RobotBulkAssign_DryRunMode(t *testing.T) {
	CommonE2EPrerequisites(t)

	fixture := newRobotProcessFixture(t, "bulk-dry-run")
	pane := prepareBulkAssignAgentPanes(t, fixture, 1)[0]
	beadID := prepareBulkAssignBeads(t, fixture, 1)[0]
	allocation, err := json.Marshal(map[string]string{pane.ID: beadID})
	if err != nil {
		t.Fatalf("[E2E-BULK-ASSIGN] marshal dry-run allocation: %v", err)
	}
	result, process := runBulkAssignFixture(t, fixture, "--allocation="+string(allocation), "--dry-run")
	if process.exitCode != 0 || !result.Success || result.ErrorCode != "" || !result.DryRun ||
		result.AllocationSource != "explicit" || result.Summary.TotalPanes != 1 ||
		result.Summary.Assigned != 0 || result.Summary.Skipped != 0 || result.Summary.Failed != 0 ||
		len(result.Assignments) != 1 || len(result.UnassignedBeads) != 0 || len(result.UnassignedPanes) != 0 {
		t.Fatalf("[E2E-BULK-ASSIGN] dry-run envelope: exit=%d result=%+v", process.exitCode, result)
	}
	assignment := result.Assignments[0]
	if assignment.Pane != strconv.Itoa(pane.Index) || assignment.PaneID != pane.ID || assignment.Bead != beadID ||
		assignment.Status != "planned" || assignment.PromptSent || assignment.Claimed || assignment.Error != "" {
		t.Fatalf("[E2E-BULK-ASSIGN] dry-run assignment=%+v", assignment)
	}
}

func TestE2E_RobotBulkAssign_ExplicitAllocation(t *testing.T) {
	CommonE2EPrerequisites(t)

	fixture := newRobotProcessFixture(t, "bulk-explicit")
	panes := prepareBulkAssignAgentPanes(t, fixture, 2)
	beads := prepareBulkAssignBeads(t, fixture, 2)
	allocation := map[string]string{panes[0].ID: beads[0], panes[1].ID: beads[1]}
	encoded, err := json.Marshal(allocation)
	if err != nil {
		t.Fatalf("[E2E-BULK-ASSIGN] marshal explicit allocation: %v", err)
	}
	result, process := runBulkAssignFixture(t, fixture, "--allocation="+string(encoded), "--dry-run")
	if process.exitCode != 0 || !result.Success || result.Session != fixture.session || !result.DryRun ||
		result.AllocationSource != "explicit" || result.ErrorCode != "" || result.Summary.TotalPanes != 2 ||
		result.Summary.Failed != 0 || len(result.Assignments) != 2 || len(result.UnassignedPanes) != 0 {
		t.Fatalf("[E2E-BULK-ASSIGN] explicit envelope: exit=%d result=%+v", process.exitCode, result)
	}
	expected := map[string]tmux.Pane{beads[0]: panes[0], beads[1]: panes[1]}
	for _, assignment := range result.Assignments {
		pane, ok := expected[assignment.Bead]
		if !ok || assignment.Pane != strconv.Itoa(pane.Index) || assignment.PaneID != pane.ID ||
			assignment.Status != "planned" || assignment.PromptSent || assignment.Claimed || assignment.Error != "" {
			t.Fatalf("[E2E-BULK-ASSIGN] explicit assignment=%+v", assignment)
		}
		delete(expected, assignment.Bead)
	}
	if len(expected) != 0 {
		t.Fatalf("[E2E-BULK-ASSIGN] explicit allocation omitted mappings: %v", expected)
	}
}

func TestE2E_RobotBulkAssign_SkipPanes(t *testing.T) {
	CommonE2EPrerequisites(t)

	fixture := newRobotProcessFixture(t, "bulk-skip-panes")
	panes := prepareBulkAssignAgentPanes(t, fixture, 3)
	beads := prepareBulkAssignBeads(t, fixture, 3)
	allocation := make(map[string]string, len(panes))
	expected := make(map[string]tmux.Pane, len(panes))
	for index := range panes {
		allocation[panes[index].ID] = beads[index]
		expected[beads[index]] = panes[index]
	}
	encoded, err := json.Marshal(allocation)
	if err != nil {
		t.Fatalf("[E2E-BULK-ASSIGN] marshal skip allocation: %v", err)
	}
	skipped := map[string]struct{}{panes[0].ID: {}, panes[1].ID: {}}
	result, process := runBulkAssignFixture(t, fixture,
		"--allocation="+string(encoded),
		"--skip="+panes[0].ID+","+panes[1].ID,
		"--dry-run",
	)
	if process.exitCode != 1 || result.Success || result.ErrorCode != "PANE_NOT_FOUND" || !result.DryRun ||
		result.AllocationSource != "explicit" || result.Summary.TotalPanes != 3 ||
		result.Summary.Assigned != 0 || result.Summary.Skipped != 0 || result.Summary.Failed != 2 ||
		len(result.Assignments) != 3 || len(result.UnassignedPanes) != 0 {
		t.Fatalf("[E2E-BULK-ASSIGN] skip-panes envelope: exit=%d result=%+v", process.exitCode, result)
	}
	for _, assignment := range result.Assignments {
		pane, ok := expected[assignment.Bead]
		if !ok {
			t.Fatalf("[E2E-BULK-ASSIGN] unexpected skip assignment=%+v", assignment)
		}
		if _, isSkipped := skipped[pane.ID]; isSkipped {
			if assignment.Pane != pane.ID || assignment.PaneID != "" || assignment.Status != "failed" ||
				assignment.PromptSent || assignment.Claimed || !strings.Contains(strings.ToLower(assignment.Error), "not found") {
				t.Fatalf("[E2E-BULK-ASSIGN] skipped assignment=%+v", assignment)
			}
		} else if assignment.Pane != strconv.Itoa(pane.Index) || assignment.PaneID != pane.ID ||
			assignment.Status != "planned" || assignment.PromptSent || assignment.Claimed || assignment.Error != "" {
			t.Fatalf("[E2E-BULK-ASSIGN] retained assignment=%+v", assignment)
		}
		delete(expected, assignment.Bead)
	}
	if len(expected) != 0 {
		t.Fatalf("[E2E-BULK-ASSIGN] skip-panes omitted mappings: %v", expected)
	}
}

func TestE2E_RobotBulkAssign_Strategy(t *testing.T) {
	CommonE2EPrerequisites(t)
	brPath, err := exec.LookPath("br")
	if err != nil {
		t.Fatalf("real br is required for hermetic bulk strategy E2E: %v", err)
	}

	strategies := []string{"impact", "ready", "stale", "balanced"}

	for _, strategy := range strategies {
		t.Run(strategy, func(t *testing.T) {
			fixture := newRobotProcessFixture(t, "bulk-strategy-"+strategy)
			runRobotFixtureTool(t, fixture, defaultRunTimeout, brPath, "init", "--prefix=rbe2e", "--json")
			beadID := strings.TrimSpace(string(runRobotFixtureTool(t, fixture, defaultRunTimeout, brPath,
				"create", "Hermetic canonical strategy", "--type=task", "--priority=1", "--silent",
			)))
			if beadID == "" || strings.ContainsAny(beadID, " \t\r\n") {
				t.Fatalf("unexpected br create output %q", beadID)
			}
			allocation, err := json.Marshal(map[string]string{fixture.paneID: beadID})
			if err != nil {
				t.Fatalf("marshal strategy allocation: %v", err)
			}

			process := runBuiltRobotProcessWithin(t, defaultRunTimeout, fixture.ntmPath, fixture.projectDir, fixture.env,
				"--robot-bulk-assign="+fixture.session,
				"--allocation="+string(allocation),
				"--strategy="+strategy,
				"--dry-run",
				"--reserve-files=false",
				"--robot-format=json",
			)
			if process.exitCode != 0 || len(bytes.TrimSpace(process.stderr)) != 0 {
				t.Fatalf("strategy %s exit=%d stdout=%s stderr=%s", strategy, process.exitCode, process.stdout, process.stderr)
			}
			var result BulkAssignOutput
			decodeSingleRobotJSON(t, process.stdout, &result)
			if !result.Success || !result.DryRun || result.AllocationSource != "explicit" || result.Strategy != strategy {
				t.Fatalf("strategy %s envelope=%+v", strategy, result)
			}
			if len(result.Assignments) != 1 || result.Assignments[0].Bead != beadID ||
				result.Assignments[0].PaneID != fixture.paneID || result.Assignments[0].Status != "planned" {
				t.Fatalf("strategy %s assignments=%+v", strategy, result.Assignments)
			}
		})
	}
}

func TestE2E_RobotBulkAssign_StrategyFlagsBuiltProcess(t *testing.T) {
	CommonE2EPrerequisites(t)
	brPath, err := exec.LookPath("br")
	if err != nil {
		t.Skipf("br is required for hermetic bulk strategy E2E: %v", err)
	}

	tests := []struct {
		name         string
		flags        []string
		wantStrategy string
	}{
		{
			name:         "deprecated_bulk_strategy_only",
			flags:        []string{"--bulk-strategy=stale"},
			wantStrategy: "stale",
		},
		{
			name:         "deprecated_flag_wins_when_canonical_is_first",
			flags:        []string{"--strategy=ready", "--bulk-strategy=balanced"},
			wantStrategy: "balanced",
		},
		{
			name:         "deprecated_flag_wins_when_canonical_is_last",
			flags:        []string{"--bulk-strategy=balanced", "--strategy=ready"},
			wantStrategy: "balanced",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRobotProcessFixture(t, "bulk-strategy")
			runBR := func(args ...string) []byte {
				t.Helper()
				return runRobotFixtureTool(t, fixture, defaultRunTimeout, brPath, args...)
			}
			runBR("init", "--prefix=rbe2e", "--json")
			beadID := strings.TrimSpace(string(runBR("create", "Hermetic bulk strategy", "--type=task", "--priority=1", "--silent")))
			if beadID == "" || strings.ContainsAny(beadID, " \t\r\n") {
				t.Fatalf("unexpected br create output %q", beadID)
			}
			allocation, err := json.Marshal(map[string]string{fixture.paneID: beadID})
			if err != nil {
				t.Fatalf("marshal allocation: %v", err)
			}
			args := []string{
				"--robot-bulk-assign=" + fixture.session,
				"--allocation=" + string(allocation),
				"--dry-run",
				"--reserve-files=false",
				"--robot-format=json",
			}
			args = append(args, test.flags...)
			process := runBuiltRobotProcessWithin(t, defaultRunTimeout, fixture.ntmPath, fixture.projectDir, fixture.env, args...)
			if process.exitCode != 0 {
				t.Fatalf("bulk strategy process exit=%d, want 0; stdout=%s stderr=%s", process.exitCode, process.stdout, process.stderr)
			}
			if stderr := strings.TrimSpace(string(process.stderr)); stderr != "" &&
				(!strings.Contains(stderr, "--bulk-strategy") || !strings.Contains(strings.ToLower(stderr), "deprecated")) {
				t.Fatalf("unexpected bulk strategy stderr: %q", stderr)
			}

			var result BulkAssignOutput
			decodeSingleRobotJSON(t, process.stdout, &result)
			if !result.Success || !result.DryRun || result.Session != fixture.session {
				t.Fatalf("bulk strategy envelope = %+v", result)
			}
			if result.Strategy != test.wantStrategy {
				t.Fatalf("bulk strategy=%q, want %q for flags %q", result.Strategy, test.wantStrategy, test.flags)
			}
			if _, err := time.Parse(time.RFC3339Nano, result.Timestamp); err != nil {
				t.Fatalf("bulk strategy timestamp %q is not RFC3339: %v", result.Timestamp, err)
			}
		})
	}
}

func TestE2ERobotBulkAssignInvalidInputsAreSingleFailureDocuments(t *testing.T) {
	CommonE2EPrerequisites(t)
	tests := []struct {
		name string
		args []string
	}{
		{name: "invalid strategy", args: []string{"--from-bv", "--strategy=fastest", "--dry-run"}},
		{name: "negative stagger", args: []string{"--from-bv", "--bulk-stagger=-1ms", "--dry-run"}},
		{name: "empty allocation", args: []string{"--allocation={}", "--dry-run"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRobotProcessFixture(t, "bulk-invalid")
			args := []string{"--robot-bulk-assign=" + fixture.session, "--robot-format=json"}
			args = append(args, test.args...)
			process := runBuiltRobotProcessWithin(t, defaultRunTimeout, fixture.ntmPath, fixture.projectDir, fixture.env, args...)
			if process.exitCode != 1 || len(bytes.TrimSpace(process.stderr)) != 0 {
				t.Fatalf("exit=%d stdout=%s stderr=%s", process.exitCode, process.stdout, process.stderr)
			}
			var result BulkAssignOutput
			decodeSingleRobotJSON(t, process.stdout, &result)
			if result.Success || result.ErrorCode != "INVALID_FLAG" || result.Assignments == nil {
				t.Fatalf("failure envelope = %+v", result)
			}
		})
	}

	t.Run("duplicate bead allocation", func(t *testing.T) {
		fixture := newRobotProcessFixture(t, "bulk-duplicate-bead")
		secondPaneID := strings.TrimSpace(fixture.mustTMUXOutput(t,
			"split-window", "-d", "-t", fixture.session,
			"-c", fixture.projectDir, "-P", "-F", "#{pane_id}",
			"/bin/bash --noprofile --norc -i",
		))
		if secondPaneID == "" {
			t.Fatal("private tmux server returned an empty second pane ID")
		}
		fixture.mustTMUXOutput(t, "select-pane", "-t", secondPaneID, "-T", fixture.session+"__cod_2")
		allocation, err := json.Marshal(map[string]string{
			fixture.paneID: "bd-duplicate",
			secondPaneID:   "bd-duplicate",
		})
		if err != nil {
			t.Fatalf("marshal duplicate allocation: %v", err)
		}
		process := runBuiltRobotProcessWithin(t, defaultRunTimeout, fixture.ntmPath, fixture.projectDir, fixture.env,
			"--robot-bulk-assign="+fixture.session,
			"--allocation="+string(allocation),
			"--dry-run",
			"--robot-format=json",
		)
		if process.exitCode != 1 || len(bytes.TrimSpace(process.stderr)) != 0 {
			t.Fatalf("exit=%d stdout=%s stderr=%s", process.exitCode, process.stdout, process.stderr)
		}
		var result BulkAssignOutput
		decodeSingleRobotJSON(t, process.stdout, &result)
		if result.Success || result.ErrorCode != "INVALID_FLAG" || len(result.Assignments) != 2 {
			t.Fatalf("duplicate failure envelope=%+v", result)
		}
		if result.Summary.Failed != 2 || result.Summary.Assigned != 0 {
			t.Fatalf("duplicate allocation summary=%+v, want both panes failed", result.Summary)
		}
		for _, assignment := range result.Assignments {
			if assignment.Status != "failed" || assignment.Claimed || assignment.PromptSent ||
				!strings.Contains(assignment.Error, "same bead") || !strings.Contains(assignment.Error, "different physical panes") {
				t.Fatalf("duplicate allocation pane was not fully rejected: %+v", assignment)
			}
		}
	})
}

func TestE2ERobotBulkAssignMissingDependenciesAreTyped(t *testing.T) {
	CommonE2EPrerequisites(t)
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatalf("real tmux is required: %v", err)
	}
	brPath, err := exec.LookPath("br")
	if err != nil {
		t.Fatalf("real br is required: %v", err)
	}
	bvPath, err := exec.LookPath("bv")
	if err != nil {
		t.Fatalf("real bv is required: %v", err)
	}

	linkTool := func(t *testing.T, dir, source, name string) {
		t.Helper()
		if err := os.Symlink(source, filepath.Join(dir, name)); err != nil {
			t.Fatalf("link %s into isolated PATH: %v", name, err)
		}
	}
	assertMissing := func(t *testing.T, fixture *robotProcessFixture, env []string, wantText ...string) {
		t.Helper()
		process := runBuiltRobotProcessWithin(t, defaultRunTimeout, fixture.ntmPath, fixture.projectDir, env,
			"--robot-bulk-assign="+fixture.session,
			"--from-bv",
			"--strategy=ready",
			"--dry-run",
			"--robot-format=json",
		)
		if process.exitCode != 1 || len(bytes.TrimSpace(process.stderr)) != 0 {
			t.Fatalf("exit=%d stdout=%s stderr=%s", process.exitCode, process.stdout, process.stderr)
		}
		var result BulkAssignOutput
		decodeSingleRobotJSON(t, process.stdout, &result)
		if result.Success || result.ErrorCode != "DEPENDENCY_MISSING" || result.Assignments == nil {
			t.Fatalf("missing dependency envelope=%+v", result)
		}
		for _, want := range wantText {
			if !strings.Contains(result.Error, want) {
				t.Fatalf("missing dependency error=%q, want %q", result.Error, want)
			}
		}
	}

	t.Run("bv", func(t *testing.T) {
		fixture := newRobotProcessFixture(t, "bulk-missing-bv")
		pathDir := t.TempDir()
		linkTool(t, pathDir, tmuxPath, "tmux")
		env := mergeRobotProcessEnv(fixture.env, map[string]string{"PATH": pathDir})
		assertMissing(t, fixture, env, "bv is not installed")
	})

	t.Run("br", func(t *testing.T) {
		fixture := newRobotProcessFixture(t, "bulk-missing-br")
		runRobotFixtureTool(t, fixture, defaultRunTimeout, brPath, "init", "--prefix=rbe2e", "--json")
		runRobotFixtureTool(t, fixture, defaultRunTimeout, brPath,
			"create", "Missing br contract", "--type=task", "--priority=1", "--silent")
		pathDir := t.TempDir()
		linkTool(t, pathDir, tmuxPath, "tmux")
		linkTool(t, pathDir, bvPath, "bv")
		env := mergeRobotProcessEnv(fixture.env, map[string]string{"PATH": pathDir})
		assertMissing(t, fixture, env, "br", "executable file not found")
	})
}

func TestE2E_RobotBulkAssign_JSONStructure(t *testing.T) {
	CommonE2EPrerequisites(t)

	fixture := newRobotProcessFixture(t, "bulk-json-structure")
	pane := prepareBulkAssignAgentPanes(t, fixture, 1)[0]
	beadID := prepareBulkAssignBeads(t, fixture, 1)[0]
	allocation, err := json.Marshal(map[string]string{pane.ID: beadID})
	if err != nil {
		t.Fatalf("[E2E-BULK-ASSIGN] marshal JSON-structure allocation: %v", err)
	}
	process := runBuiltRobotProcessWithin(t, defaultRunTimeout, fixture.ntmPath, fixture.projectDir, fixture.env,
		"--robot-bulk-assign="+fixture.session,
		"--robot-format=json",
		"--allocation="+string(allocation),
		"--dry-run",
		"--reserve-files=false",
	)
	if process.exitCode != 0 || len(bytes.TrimSpace(process.stderr)) != 0 {
		t.Fatalf("[E2E-BULK-ASSIGN] JSON-structure process exit=%d stdout=%s stderr=%s", process.exitCode, process.stdout, process.stderr)
	}
	var result BulkAssignOutput
	decodeSingleRobotJSON(t, process.stdout, &result)
	if !result.Success || result.ErrorCode != "" || !result.DryRun || result.AllocationSource != "explicit" ||
		result.Session != fixture.session || len(result.Assignments) != 1 {
		t.Fatalf("[E2E-BULK-ASSIGN] JSON-structure envelope=%+v", result)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(process.stdout, &raw); err != nil {
		t.Fatalf("[E2E-BULK-ASSIGN] Invalid JSON: %v", err)
	}
	requiredFields := []string{
		"success", "session", "strategy", "timestamp", "assignments", "summary", "unassigned_beads", "unassigned_panes", "dry_run", "allocation_source",
	}
	for _, field := range requiredFields {
		if _, ok := raw[field]; !ok {
			t.Fatalf("[E2E-BULK-ASSIGN] Missing required field: %s", field)
		}
	}
	for _, field := range []string{"unassigned_beads", "unassigned_panes"} {
		if _, ok := raw[field].([]interface{}); !ok {
			t.Fatalf("[E2E-BULK-ASSIGN] Required field %s is not an array: %#v", field, raw[field])
		}
	}
	if _, present := raw["error"]; present {
		t.Fatalf("[E2E-BULK-ASSIGN] successful JSON unexpectedly contains error: %v", raw)
	}
}

func TestE2E_RobotBulkAssign_InvalidAllocationJSON(t *testing.T) {
	CommonE2EPrerequisites(t)

	fixture := newRobotProcessFixture(t, "bulk-invalid-json")
	result, process := runBulkAssignFixture(t, fixture, "--allocation={invalid json}")
	lowerError := strings.ToLower(result.Error)
	if process.exitCode != 1 || result.Success || result.ErrorCode != "INVALID_FLAG" ||
		result.Assignments == nil || result.UnassignedBeads == nil || result.UnassignedPanes == nil ||
		result.Summary != (BulkAssignSummary{}) ||
		(!strings.Contains(lowerError, "json") && !strings.Contains(lowerError, "parse")) {
		t.Fatalf("[E2E-BULK-ASSIGN] invalid-allocation envelope: exit=%d result=%+v", process.exitCode, result)
	}
}

func TestE2E_RobotBulkAssign_SummaryStats(t *testing.T) {
	CommonE2EPrerequisites(t)

	fixture := newRobotProcessFixture(t, "bulk-summary")
	panes := prepareBulkAssignAgentPanes(t, fixture, 3)
	beads := prepareBulkAssignBeads(t, fixture, 3)
	allocation := make(map[string]string, len(panes))
	for index := range panes {
		allocation[panes[index].ID] = beads[index]
	}
	encoded, err := json.Marshal(allocation)
	if err != nil {
		t.Fatalf("[E2E-BULK-ASSIGN] marshal summary allocation: %v", err)
	}
	result, process := runBulkAssignFixture(t, fixture, "--allocation="+string(encoded), "--dry-run")
	if process.exitCode != 0 || !result.Success || result.ErrorCode != "" || !result.DryRun ||
		result.AllocationSource != "explicit" || result.Summary.TotalPanes != 3 ||
		result.Summary.Assigned != 0 || result.Summary.Skipped != 0 || result.Summary.Failed != 0 ||
		len(result.Assignments) != 3 || len(result.UnassignedBeads) != 0 || len(result.UnassignedPanes) != 0 {
		t.Fatalf("[E2E-BULK-ASSIGN] summary envelope: exit=%d result=%+v", process.exitCode, result)
	}
}

func TestE2E_RobotBulkAssign_UnassignedPanes(t *testing.T) {
	CommonE2EPrerequisites(t)

	fixture := newRobotProcessFixture(t, "bulk-unassigned-panes")
	panes := prepareBulkAssignAgentPanes(t, fixture, 4)
	beads := prepareBulkAssignBeads(t, fixture, 1)

	// Test with fewer beads than panes
	allocation, err := json.Marshal(map[string]string{panes[0].ID: beads[0]})
	if err != nil {
		t.Fatalf("[E2E-BULK-ASSIGN] marshal single-pane allocation: %v", err)
	}
	result, process := runBulkAssignFixture(t, fixture,
		"--allocation="+string(allocation),
		"--dry-run")

	if process.exitCode != 0 || !result.Success || !result.DryRun || result.AllocationSource != "explicit" {
		t.Fatalf("[E2E-BULK-ASSIGN] positive unassigned-pane envelope: exit=%d result=%+v", process.exitCode, result)
	}
	if result.Summary.TotalPanes != 4 || result.Summary.Assigned != 0 || result.Summary.Skipped != 0 ||
		result.Summary.Failed != 0 || len(result.Assignments) != 1 || len(result.UnassignedPanes) != 3 {
		t.Fatalf("[E2E-BULK-ASSIGN] result=%+v, want one allocation plus three unassigned panes", result)
	}
	assignment := result.Assignments[0]
	if assignment.Pane != strconv.Itoa(panes[0].Index) || assignment.PaneID != panes[0].ID || assignment.Bead != beads[0] ||
		assignment.Status != "planned" || assignment.PromptSent || assignment.Claimed || assignment.Error != "" {
		t.Fatalf("[E2E-BULK-ASSIGN] assignment=%+v, want the allocated physical pane to resolve", result.Assignments[0])
	}
	expectedUnassigned := make(map[string]struct{}, 3)
	for _, pane := range panes[1:] {
		expectedUnassigned[strconv.Itoa(pane.Index)] = struct{}{}
	}
	for _, pane := range result.UnassignedPanes {
		if _, ok := expectedUnassigned[pane]; !ok {
			t.Fatalf("[E2E-BULK-ASSIGN] unexpected or duplicate unassigned pane %q in %v", pane, result.UnassignedPanes)
		}
		delete(expectedUnassigned, pane)
	}
	if len(expectedUnassigned) != 0 {
		t.Fatalf("[E2E-BULK-ASSIGN] missing exact unassigned panes %v from %v", expectedUnassigned, result.UnassignedPanes)
	}
}

func TestE2E_RobotBulkAssign_UnassignedBeads(t *testing.T) {
	CommonE2EPrerequisites(t)

	fixture := newRobotProcessFixture(t, "bulk-unassigned-beads")
	pane := prepareBulkAssignAgentPanes(t, fixture, 1)[0]
	beads := prepareBulkAssignBeads(t, fixture, 5)
	recommendations := make([]map[string]any, 0, len(beads))
	items := make([]map[string]any, 0, len(beads))
	for index, beadID := range beads {
		title := fmt.Sprintf("Verified unassigned bulk bead %d", index+1)
		recommendations = append(recommendations, map[string]any{
			"id": beadID, "title": title, "type": "task", "status": "open",
			"priority": 1, "score": float64(len(beads) - index), "action": "assign", "reasons": []string{"E2E"},
		})
		items = append(items, map[string]any{
			"id": beadID, "title": title, "status": "open", "priority": 1,
		})
	}
	triagePayload, err := json.Marshal(map[string]any{
		"generated_at": "2026-07-19T00:00:00Z",
		"data_hash":    "bulk-unassigned-e2e",
		"triage": map[string]any{
			"recommendations": recommendations,
		},
	})
	if err != nil {
		t.Fatalf("[E2E-BULK-ASSIGN] marshal unassigned triage: %v", err)
	}
	planPayload, err := json.Marshal(map[string]any{
		"generated_at": "2026-07-19T00:00:00Z",
		"plan": map[string]any{
			"tracks": []map[string]any{{
				"track_id": "bulk-unassigned-e2e",
				"items":    items,
			}},
		},
	})
	if err != nil {
		t.Fatalf("[E2E-BULK-ASSIGN] marshal unassigned plan: %v", err)
	}
	env, _, _ := installBulkAssignPlanningFixtures(
		t, fixture, fixture.env, string(triagePayload), string(planPayload), "ok", "real",
	)
	process := runBuiltRobotProcessWithin(t, defaultRunTimeout, fixture.ntmPath, fixture.projectDir, env,
		"--robot-bulk-assign="+fixture.session,
		"--from-bv",
		"--strategy=impact",
		"--dry-run",
		"--reserve-files=false",
		"--robot-format=json",
	)
	if process.exitCode != 0 || len(bytes.TrimSpace(process.stderr)) != 0 {
		t.Fatalf("[E2E-BULK-ASSIGN] unassigned-beads process exit=%d stdout=%s stderr=%s", process.exitCode, process.stdout, process.stderr)
	}
	var result BulkAssignOutput
	decodeSingleRobotJSON(t, process.stdout, &result)
	if !result.Success || result.ErrorCode != "" || !result.DryRun || result.AllocationSource != "bv" ||
		result.Summary.TotalPanes != 1 || result.Summary.Assigned != 0 || result.Summary.Skipped != 0 ||
		result.Summary.Failed != 0 || len(result.Assignments) != 1 || len(result.UnassignedBeads) != 4 ||
		len(result.UnassignedPanes) != 0 {
		t.Fatalf("[E2E-BULK-ASSIGN] unassigned-beads envelope=%+v", result)
	}
	assignment := result.Assignments[0]
	if assignment.Pane != strconv.Itoa(pane.Index) || assignment.PaneID != pane.ID ||
		assignment.Status != "planned" || assignment.PromptSent || assignment.Claimed || assignment.Error != "" {
		t.Fatalf("[E2E-BULK-ASSIGN] unassigned-beads assignment=%+v", assignment)
	}
	remaining := make(map[string]struct{}, len(beads))
	for _, beadID := range beads {
		remaining[beadID] = struct{}{}
	}
	if _, ok := remaining[assignment.Bead]; !ok {
		t.Fatalf("[E2E-BULK-ASSIGN] assigned unknown bead %q", assignment.Bead)
	}
	delete(remaining, assignment.Bead)
	for _, beadID := range result.UnassignedBeads {
		if _, ok := remaining[beadID]; !ok {
			t.Fatalf("[E2E-BULK-ASSIGN] unexpected or duplicate unassigned bead %q in %v", beadID, result.UnassignedBeads)
		}
		delete(remaining, beadID)
	}
	if len(remaining) != 0 {
		t.Fatalf("[E2E-BULK-ASSIGN] unassigned-beads partition omitted %v", remaining)
	}
}

func TestE2E_RobotBulkAssign_WithFromBV(t *testing.T) {
	CommonE2EPrerequisites(t)
	brPath, err := exec.LookPath("br")
	if err != nil {
		t.Fatalf("real br is required for from-BV E2E coverage: %v", err)
	}
	bvPath, err := exec.LookPath("bv")
	if err != nil {
		t.Fatalf("real bv is required for from-BV E2E coverage: %v", err)
	}

	fixture := newRobotProcessFixture(t, "bulk-from-bv")
	const realBVRobotTimeout = 90 * time.Second
	makeCodexIdle := func(t *testing.T, targetFixture *robotProcessFixture) {
		t.Helper()
		fakeCodex := filepath.Join(targetFixture.root, "codex")
		fakeCodexScript := "#!/bin/sh\nprintf 'Codex> \\n100%% context left\\n'\nwhile IFS= read -r line; do\n  printf 'received: %s\\nCodex> \\n100%% context left\\n' \"$line\"\ndone\n"
		if err := os.WriteFile(fakeCodex, []byte(fakeCodexScript), 0o700); err != nil {
			t.Fatalf("write fake Codex agent: %v", err)
		}
		targetFixture.mustTMUXOutput(t, "respawn-pane", "-k", "-t", targetFixture.paneID, fakeCodex)
		time.Sleep(5500 * time.Millisecond)
	}
	makeCodexIdle(t, fixture)
	runTool := func(t *testing.T, targetFixture *robotProcessFixture, path string, args ...string) []byte {
		t.Helper()
		return runRobotFixtureTool(t, targetFixture, defaultRunTimeout, path, args...)
	}

	runTool(t, fixture, brPath, "init", "--prefix=rbe2e", "--json")
	beadID := strings.TrimSpace(string(runTool(t, fixture, brPath,
		"create", "Hermetic from-BV assignment", "--type=task", "--priority=1", "--silent",
	)))
	if beadID == "" || strings.ContainsAny(beadID, " \t\r\n") {
		t.Fatalf("unexpected br create output %q", beadID)
	}

	type bvTriageEnvelope struct {
		Triage struct {
			Recommendations []struct {
				ID string `json:"id"`
			} `json:"recommendations"`
		} `json:"triage"`
	}
	var triage bvTriageEnvelope
	decodeSingleRobotJSON(t, runTool(t, fixture, bvPath, "--robot-triage"), &triage)
	if len(triage.Triage.Recommendations) == 0 || triage.Triage.Recommendations[0].ID != beadID {
		t.Fatalf("real bv recommendations=%+v, want first bead %q", triage.Triage.Recommendations, beadID)
	}

	for _, strategy := range []string{"impact", "ready"} {
		process := runBuiltRobotProcessWithin(t, realBVRobotTimeout, fixture.ntmPath, fixture.projectDir, fixture.env,
			"--robot-bulk-assign="+fixture.session,
			"--from-bv",
			"--strategy="+strategy,
			"--dry-run",
			"--reserve-files=false",
			"--robot-format=json",
		)
		if process.exitCode != 0 || len(bytes.TrimSpace(process.stderr)) != 0 {
			t.Fatalf("real from-BV %s process exit=%d stdout=%s stderr=%s", strategy, process.exitCode, process.stdout, process.stderr)
		}
		var result BulkAssignOutput
		decodeSingleRobotJSON(t, process.stdout, &result)
		if !result.Success || !result.DryRun {
			t.Fatalf("real from-BV %s envelope=%+v", strategy, result)
		}
		if result.AllocationSource != "bv" || result.Strategy != strategy {
			t.Fatalf("allocation_source=%q strategy=%q, want bv/%s", result.AllocationSource, result.Strategy, strategy)
		}
		if len(result.Assignments) != 1 {
			t.Fatalf("%s assignments=%+v, want exactly one real bv allocation", strategy, result.Assignments)
		}
		plannedAssignment := result.Assignments[0]
		if plannedAssignment.Bead != beadID || plannedAssignment.PaneID != fixture.paneID || plannedAssignment.Status != "planned" || plannedAssignment.PromptSent {
			t.Fatalf("%s assignment=%+v, want bead=%q pane_id=%q planned without dispatch", strategy, plannedAssignment, beadID, fixture.paneID)
		}
		if result.Summary.TotalPanes != 1 || result.Summary.Failed != 0 {
			t.Fatalf("%s summary=%+v, want one successful dry-run plan", strategy, result.Summary)
		}
	}

	runBulk := func(t *testing.T, targetFixture *robotProcessFixture, strategy string, extraArgs ...string) BulkAssignOutput {
		t.Helper()
		args := []string{
			"--robot-bulk-assign=" + targetFixture.session,
			"--from-bv",
			"--strategy=" + strategy,
			"--robot-format=json",
		}
		args = append(args, extraArgs...)
		process := runBuiltRobotProcessWithin(t, realBVRobotTimeout, targetFixture.ntmPath, targetFixture.projectDir, targetFixture.env, args...)
		if process.exitCode != 0 || len(bytes.TrimSpace(process.stderr)) != 0 {
			t.Fatalf("bulk %s exit=%d stdout=%s stderr=%s", strategy, process.exitCode, process.stdout, process.stderr)
		}
		var output BulkAssignOutput
		decodeSingleRobotJSON(t, process.stdout, &output)
		if !output.Success || output.DryRun || output.AllocationSource != "bv" || output.Strategy != strategy {
			t.Fatalf("bulk %s envelope=%+v", strategy, output)
		}
		return output
	}

	const staleTitle = "Hermetic stale adoption"
	staleID := strings.TrimSpace(string(runTool(t, fixture, brPath,
		"create", staleTitle, "--type=task", "--priority=1", "--silent",
	)))
	runTool(t, fixture, brPath, "update", staleID, "--status=in_progress", "--json")
	var workspaceInfo struct {
		DatabasePath string `json:"database_path"`
	}
	decodeSingleRobotJSON(t, runTool(t, fixture, brPath, "info", "--json"), &workspaceInfo)
	if strings.TrimSpace(workspaceInfo.DatabasePath) == "" {
		t.Fatal("br info returned no SQLite database path")
	}
	staleUpdatedAt := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	database, err := sql.Open(sqliteutil.DriverName, sqliteutil.FileDSN(workspaceInfo.DatabasePath, "busy_timeout(5000)", "foreign_keys(ON)"))
	if err != nil {
		t.Fatalf("open real Beads database: %v", err)
	}
	if _, err := database.Exec("UPDATE issues SET updated_at = ? WHERE id = ?", staleUpdatedAt.Format(time.RFC3339Nano), staleID); err != nil {
		_ = database.Close()
		t.Fatalf("age stale Beads row: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close real Beads database: %v", err)
	}
	runTool(t, fixture, brPath, "sync", "--flush-only", "--json", "--no-auto-import")

	// The hermetic fixture has no Agent Mail and the bead names no files, so
	// fresh adoption opts out of the default file reservation.
	staleOutput := runBulk(t, fixture, "stale", "--reserve-files=false")
	if len(staleOutput.Assignments) != 1 {
		t.Fatalf("stale assignments=%+v, want one", staleOutput.Assignments)
	}
	staleAssignment := staleOutput.Assignments[0]
	if staleAssignment.Bead != staleID || staleAssignment.PaneID != fixture.paneID || staleAssignment.Status != "assigned" ||
		!staleAssignment.Claimed || !staleAssignment.PromptSent || staleAssignment.ClaimActor == "" ||
		staleAssignment.IdempotencyKey == "" || staleAssignment.DispatchReceiptID == "" {
		t.Fatalf("stale assignment=%+v", staleAssignment)
	}
	database, err = sql.Open(sqliteutil.DriverName, sqliteutil.FileDSN(workspaceInfo.DatabasePath, "busy_timeout(5000)", "foreign_keys(ON)"))
	if err != nil {
		t.Fatalf("reopen real Beads database: %v", err)
	}
	var trackerStatus, trackerAssignee string
	if err := database.QueryRow("SELECT status, COALESCE(assignee, '') FROM issues WHERE id = ?", staleID).Scan(&trackerStatus, &trackerAssignee); err != nil {
		_ = database.Close()
		t.Fatalf("read adopted Beads row: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close adopted Beads database: %v", err)
	}
	if trackerStatus != "in_progress" || trackerAssignee != staleAssignment.ClaimActor {
		t.Fatalf("adopted tracker status=%q assignee=%q, want in_progress/%q", trackerStatus, trackerAssignee, staleAssignment.ClaimActor)
	}

	// A second process must replay the durable receipt without claiming or
	// dispatching the same stale work a second time. Wait for the asynchronous
	// tmux delivery to become visible and stable before taking the baseline.
	captureStable := func(t *testing.T, targetFixture *robotProcessFixture, minimumReceipts int) string {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		last := ""
		stableReads := 0
		for time.Now().Before(deadline) {
			captured := targetFixture.mustTMUXOutput(t, "capture-pane", "-p", "-t", targetFixture.paneID, "-S", "-")
			if strings.Count(captured, "received:") >= minimumReceipts && captured == last {
				stableReads++
				if stableReads >= 2 {
					return captured
				}
			} else {
				stableReads = 0
			}
			last = captured
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("pane %s did not stabilize with %d receipts; last output:\n%s", targetFixture.paneID, minimumReceipts, last)
		return ""
	}
	paneBeforeReplay := captureStable(t, fixture, 1)
	replayOutput := runBulk(t, fixture, "stale", "--reserve-files=false")
	if len(replayOutput.Assignments) != 1 {
		t.Fatalf("stale replay assignments=%+v, want one", replayOutput.Assignments)
	}
	replayed := replayOutput.Assignments[0]
	if replayed.IdempotencyKey != staleAssignment.IdempotencyKey || replayed.DispatchReceiptID != staleAssignment.DispatchReceiptID ||
		replayed.ClaimActor != staleAssignment.ClaimActor || !replayed.Claimed || !replayed.PromptSent {
		t.Fatalf("stale replay=%+v, first=%+v", replayed, staleAssignment)
	}
	paneAfterReplay := captureStable(t, fixture, 1)
	if paneAfterReplay != paneBeforeReplay {
		t.Fatalf("durable stale replay actuated tmux twice\nbefore:\n%s\nafter:\n%s", paneBeforeReplay, paneAfterReplay)
	}

	// Seed the crash window after the external tracker CAS commits but before
	// RecordAtomicClaim persists its receipt, then recover with a fresh process.
	recoveryFixtureValue := *fixture
	recoveryFixture := &recoveryFixtureValue
	recoveryFixture.session = fixture.session + "-recovery"
	recoveryFixture.paneID = strings.TrimSpace(fixture.mustTMUXOutput(t,
		"new-session", "-d", "-s", recoveryFixture.session,
		"-x", "160", "-y", "48", "-c", recoveryFixture.projectDir,
		"-P", "-F", "#{pane_id}",
		"/bin/bash --noprofile --norc -i",
	))
	if recoveryFixture.paneID == "" {
		t.Fatal("private tmux server returned an empty recovery pane ID")
	}
	fixture.mustTMUXOutput(t, "select-pane", "-t", recoveryFixture.paneID, "-T", recoveryFixture.session+"__cod_1")
	makeCodexIdle(t, recoveryFixture)
	const recoveryTitle = "Recover stale claim after process exit"
	recoveryBead := strings.TrimSpace(string(runTool(t, recoveryFixture, brPath,
		"create", recoveryTitle, "--type=task", "--priority=1", "--silent",
	)))
	recoveryKey := strings.Repeat("a", 64)
	recoveryAgent := fmt.Sprintf("ntm:%s:%s", recoveryFixture.session, recoveryFixture.paneID)
	recoveryActor := assignment.StableClaimActor(recoveryAgent, recoveryKey)
	runTool(t, recoveryFixture, brPath, "update", recoveryBead, "--status=in_progress", "--assignee="+recoveryActor, "--json")
	recoveryPrompt := fmt.Sprintf(
		"Read AGENTS.md, register with Agent Mail. Work on: %s - %s.\nUse br show %s for details. Mark in_progress when starting. Use ultrathink.",
		recoveryBead, recoveryTitle, recoveryBead,
	)
	recoveryHome := ""
	for _, entry := range recoveryFixture.env {
		if key, value, ok := strings.Cut(entry, "="); ok && key == "HOME" {
			recoveryHome = value
			break
		}
	}
	if recoveryHome == "" {
		t.Fatal("recovery fixture has no HOME")
	}
	t.Setenv("HOME", recoveryHome)
	recoveryStore := assignment.NewStore(recoveryFixture.session)
	recoveryStore.Assignments[recoveryBead] = &assignment.Assignment{
		BeadID: recoveryBead, BeadTitle: recoveryTitle, Pane: 0,
		AgentType: "codex", AgentName: recoveryAgent, Status: assignment.StatusClaiming,
		AssignedAt: time.Now().UTC(), IdempotencyKey: recoveryKey,
		ClaimActor: recoveryActor, ClaimState: assignment.ClaimUnknown,
		PendingPrompt: recoveryPrompt, PromptSHA256: assignment.PromptSHA256(recoveryPrompt),
		IntentSHA256:   assignment.PromptSHA256(recoveryPrompt),
		DispatchTarget: recoveryFixture.paneID, OccupancyKey: recoveryFixture.paneID,
		DispatchState: assignment.DispatchPending,
	}
	if err := recoveryStore.Save(); err != nil {
		t.Fatalf("seed crash-window assignment ledger: %v", err)
	}

	// Recovery replays the durable intent (recorded without a reservation), so
	// neither the reserving default nor --require-reservation reaches Agent Mail.
	recoveredOutput := runBulk(t, recoveryFixture, "stale",
		"--prompt-template="+filepath.Join(recoveryFixture.root, "missing-recovery-template.txt"),
		"--require-reservation",
	)
	if len(recoveredOutput.Assignments) != 1 {
		t.Fatalf("crash recovery assignments=%+v, want one", recoveredOutput.Assignments)
	}
	recovered := recoveredOutput.Assignments[0]
	if recovered.Bead != recoveryBead || recovered.PaneID != recoveryFixture.paneID || recovered.Reason != "stale_recovery" ||
		recovered.IdempotencyKey != recoveryKey || recovered.ClaimActor != recoveryActor || recovered.Status != "assigned" ||
		!recovered.Claimed || !recovered.PromptSent || recovered.DispatchReceiptID == "" {
		t.Fatalf("crash recovery assignment=%+v", recovered)
	}
	durableRecovery, err := assignment.LoadStoreStrict(recoveryFixture.session)
	if err != nil {
		t.Fatalf("reload recovered assignment ledger: %v", err)
	}
	recoveryRecord := durableRecovery.Get(recoveryBead)
	if recoveryRecord == nil || recoveryRecord.ClaimState != assignment.ClaimClaimed ||
		recoveryRecord.DispatchState != assignment.DispatchSent || recoveryRecord.DispatchReceiptID != recovered.DispatchReceiptID {
		t.Fatalf("durable crash recovery record=%+v", recoveryRecord)
	}
	recoveryPane := captureStable(t, recoveryFixture, 1)
	recoveryPaneUnwrapped := strings.ReplaceAll(recoveryPane, "\n", "")
	if strings.Count(recoveryPane, "received: Read AGENTS.md") != 1 ||
		!strings.Contains(recoveryPaneUnwrapped, recoveryBead) ||
		!strings.Contains(recoveryPaneUnwrapped, recoveryTitle) {
		t.Fatalf("crash recovery did not deliver its durable prompt exactly once:\n%s", recoveryPane)
	}

	// Exercise the two-phase mixed path in one built process: the durable sent
	// row must replay without actuation while a fresh ready bead claims and
	// dispatches to a second physical pane.
	mixedPaneID := strings.TrimSpace(fixture.mustTMUXOutput(t,
		"split-window", "-d", "-t", recoveryFixture.session,
		"-c", recoveryFixture.projectDir, "-P", "-F", "#{pane_id}",
		"/bin/bash --noprofile --norc -i",
	))
	if mixedPaneID == "" {
		t.Fatal("private tmux server returned an empty mixed-plan pane ID")
	}
	fixture.mustTMUXOutput(t, "select-pane", "-t", mixedPaneID, "-T", recoveryFixture.session+"__cod_2")
	mixedPaneFixtureValue := *recoveryFixture
	mixedPaneFixture := &mixedPaneFixtureValue
	mixedPaneFixture.paneID = mixedPaneID
	makeCodexIdle(t, mixedPaneFixture)
	const mixedFreshTitle = "Fresh work beside durable replay"
	mixedFreshBead := strings.TrimSpace(string(runTool(t, recoveryFixture, brPath,
		"create", mixedFreshTitle, "--type=task", "--priority=0", "--silent",
	)))
	mixedFreshBefore := mixedPaneFixture.mustTMUXOutput(t, "capture-pane", "-p", "-t", mixedPaneID, "-S", "-")
	mixedOutput := runBulk(t, recoveryFixture, "balanced", "--reserve-files=false")
	if len(mixedOutput.Assignments) != 2 || mixedOutput.Summary.Assigned != 2 || mixedOutput.Summary.Failed != 0 {
		t.Fatalf("mixed replay/fresh output=%+v", mixedOutput)
	}
	var mixedReplay, mixedFresh *BulkAssignAssignment
	for i := range mixedOutput.Assignments {
		switch mixedOutput.Assignments[i].Bead {
		case recoveryBead:
			mixedReplay = &mixedOutput.Assignments[i]
		case mixedFreshBead:
			mixedFresh = &mixedOutput.Assignments[i]
		}
	}
	if mixedReplay == nil || mixedFresh == nil || mixedReplay.DispatchReceiptID != recovered.DispatchReceiptID ||
		!mixedReplay.Claimed || !mixedReplay.PromptSent || mixedFresh.Status != "assigned" ||
		!mixedFresh.Claimed || !mixedFresh.PromptSent || mixedFresh.DispatchReceiptID == "" || mixedFresh.PaneID != mixedPaneID {
		t.Fatalf("mixed replay=%+v fresh=%+v output=%+v", mixedReplay, mixedFresh, mixedOutput)
	}
	recoveryPaneAfterMixed := captureStable(t, recoveryFixture, 1)
	if strings.Count(recoveryPaneAfterMixed, "received: Read AGENTS.md") != 1 ||
		strings.Count(recoveryPaneAfterMixed, "received: Use br show") != 1 {
		t.Fatalf("mixed durable replay actuated its pane again\nbefore:\n%s\nafter:\n%s", recoveryPane, recoveryPaneAfterMixed)
	}
	mixedFreshPane := captureStable(t, mixedPaneFixture, 1)
	mixedFreshPaneUnwrapped := strings.ReplaceAll(mixedFreshPane, "\n", "")
	if strings.Count(mixedFreshPane, "received: Read AGENTS.md") != 1 ||
		strings.Contains(mixedFreshBefore, "received: Read AGENTS.md") ||
		!strings.Contains(mixedFreshPaneUnwrapped, mixedFreshBead) ||
		!strings.Contains(mixedFreshPaneUnwrapped, mixedFreshTitle) {
		t.Fatalf("mixed fresh prompt was not delivered exactly once\nbefore:\n%s\nafter:\n%s", mixedFreshBefore, mixedFreshPane)
	}

	newPeerFixture := func(t *testing.T, suffix string) *robotProcessFixture {
		t.Helper()
		peerValue := *fixture
		peer := &peerValue
		peer.session = fixture.session + "-" + suffix
		peer.paneID = strings.TrimSpace(fixture.mustTMUXOutput(t,
			"new-session", "-d", "-s", peer.session,
			"-x", "160", "-y", "48", "-c", peer.projectDir,
			"-P", "-F", "#{pane_id}",
			"/bin/bash --noprofile --norc -i",
		))
		if peer.paneID == "" {
			t.Fatalf("private tmux server returned an empty %s peer pane ID", suffix)
		}
		fixture.mustTMUXOutput(t, "select-pane", "-t", peer.paneID, "-T", peer.session+"__cod_1")
		makeCodexIdle(t, peer)
		return peer
	}
	peerA := newPeerFixture(t, "race-a")
	peerB := newPeerFixture(t, "race-b")
	const raceTitle = "Concurrent stale adoption CAS"
	raceBead := strings.TrimSpace(string(runTool(t, fixture, brPath,
		"create", raceTitle, "--type=task", "--priority=1", "--silent",
	)))
	runTool(t, fixture, brPath, "update", raceBead, "--status=in_progress", "--json")
	raceUpdatedAt := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Microsecond)
	database, err = sql.Open(sqliteutil.DriverName, sqliteutil.FileDSN(workspaceInfo.DatabasePath, "busy_timeout(5000)", "foreign_keys(ON)"))
	if err != nil {
		t.Fatalf("open Beads database for concurrent stale race: %v", err)
	}
	if _, err := database.Exec("UPDATE issues SET updated_at = ? WHERE id = ?", raceUpdatedAt.Format(time.RFC3339Nano), raceBead); err != nil {
		_ = database.Close()
		t.Fatalf("age concurrent stale Beads row: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close concurrent stale Beads database: %v", err)
	}
	runTool(t, fixture, brPath, "sync", "--flush-only", "--json", "--no-auto-import")

	barrierDir := t.TempDir()
	wrapperDir := t.TempDir()
	brWrapper := filepath.Join(wrapperDir, "br")
	barrierScript := fmt.Sprintf(`#!/bin/sh
barrier=%q
real_br=%q
is_list=0
is_in_progress=0
is_info=0
is_no_auto_import=0
is_no_auto_flush=0
is_locked_ntm_list=0
is_direct_ntm_child=0
expect_status=0
if [ "${1:-}" = "--lock-timeout" ] && [ "${2:-}" = "5000" ]; then
  is_locked_ntm_list=1
fi
if [ -n "${NTM_E2E_DIRECT_PARENT_PID:-}" ] && [ "$PPID" = "$NTM_E2E_DIRECT_PARENT_PID" ]; then
  is_direct_ntm_child=1
fi
for arg in "$@"; do
  if [ "$expect_status" -eq 1 ]; then
    [ "$arg" = "in_progress" ] && is_in_progress=1
    expect_status=0
    continue
  fi
  case "$arg" in
    list) is_list=1 ;;
    info) is_info=1 ;;
    --status=in_progress) is_in_progress=1 ;;
    --status) expect_status=1 ;;
    --no-auto-import) is_no_auto_import=1 ;;
    --no-auto-flush) is_no_auto_flush=1 ;;
  esac
done
if [ "$is_direct_ntm_child" -eq 1 ] && [ "$is_locked_ntm_list" -eq 1 ] && [ "$is_list" -eq 1 ] && [ "$is_in_progress" -eq 1 ]; then
	contender=${NTM_E2E_RACE_CONTENDER:-}
	case "$contender" in
	  A|B) ;;
	  *) printf 'missing concurrent br list contender identity\n' >&2; exit 97 ;;
	esac
	output="$barrier/list.$contender.$$"
	"$real_br" "$@" > "$output"
	status=$?
	cat "$output"
	exit "$status"
fi
if [ "$is_direct_ntm_child" -eq 1 ] && [ "$is_locked_ntm_list" -eq 1 ] && [ "$is_info" -eq 1 ] && [ "$is_no_auto_import" -eq 1 ] && [ "$is_no_auto_flush" -eq 1 ]; then
	contender=${NTM_E2E_RACE_CONTENDER:-}
	case "$contender" in
	  A|B) ;;
	  *) printf 'missing concurrent br claim contender identity\n' >&2; exit 97 ;;
	esac
	output="$barrier/info.$contender.$$"
	"$real_br" "$@" > "$output"
	status=$?
	: > "$barrier/arrived.$contender"
	attempts=0
	while [ "$attempts" -lt 30 ]; do
	  [ -f "$barrier/arrived.A" ] && [ -f "$barrier/arrived.B" ] && break
	  attempts=$((attempts + 1))
	  sleep 1
	done
	if [ ! -f "$barrier/arrived.A" ] || [ ! -f "$barrier/arrived.B" ]; then
	  printf 'timed out waiting for concurrent guarded-claim barrier\n' >&2
	  exit 98
	fi
	cat "$output"
	exit "$status"
fi
exec "$real_br" "$@"
`, barrierDir, brPath)
	if err := os.WriteFile(brWrapper, []byte(barrierScript), 0o700); err != nil {
		t.Fatalf("write concurrent br barrier: %v", err)
	}
	raceEnv := mergeRobotProcessEnv(fixture.env, map[string]string{
		"PATH": wrapperDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	type raceProcess struct {
		cmd      *exec.Cmd
		group    *testutil.ProcessGroupForTest
		stdout   *bytes.Buffer
		stderr   *bytes.Buffer
		joined   bool
		waitErr  error
		closeErr error
	}
	joinRace := func(process *raceProcess) error {
		if process.joined {
			return errors.Join(process.waitErr, process.closeErr)
		}
		process.waitErr = process.cmd.Wait()
		process.closeErr = process.group.Close()
		process.joined = true
		return errors.Join(process.waitErr, process.closeErr)
	}
	startRace := func(t *testing.T, peer *robotProcessFixture, contender string) *raceProcess {
		t.Helper()
		ntmArgs := []string{
			"--robot-bulk-assign=" + peer.session,
			"--from-bv",
			"--strategy=stale",
			// The race is over the Beads claim; the fixture has no Agent Mail.
			"--reserve-files=false",
			"--robot-format=json",
		}
		launcherArgs := []string{
			"-c",
			`NTM_E2E_DIRECT_PARENT_PID=$$; export NTM_E2E_DIRECT_PARENT_PID; exec "$@"`,
			"ntm-race",
			peer.ntmPath,
		}
		launcherArgs = append(launcherArgs, ntmArgs...)
		cmd := exec.CommandContext(ctx, "/bin/sh", launcherArgs...)
		cmd.Dir = peer.projectDir
		cmd.Env = mergeRobotProcessEnv(raceEnv, map[string]string{
			"NTM_E2E_RACE_CONTENDER": contender,
		})
		group, groupErr := testutil.NewProcessGroupForTest(ctx, cmd)
		if groupErr != nil {
			t.Fatalf("create owned process group for %s: %v", peer.session, groupErr)
		}
		cmd.Cancel = func() error {
			return group.Signal(os.Kill)
		}
		cmd.WaitDelay = 2 * time.Second
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err := cmd.Start(); err != nil {
			closeErr := group.Close()
			t.Fatalf("start concurrent bulk process for %s: %v; close owned process group: %v", peer.session, err, closeErr)
		}
		process := &raceProcess{cmd: cmd, group: group, stdout: stdout, stderr: stderr}
		t.Cleanup(func() {
			if process.joined {
				return
			}
			cancel()
			_ = process.group.Signal(os.Kill)
			_ = joinRace(process)
			if process.closeErr != nil {
				t.Errorf("close owned process group for %s: %v", peer.session, process.closeErr)
			}
		})
		return process
	}
	processA := startRace(t, peerA, "A")
	processB := startRace(t, peerB, "B")
	stdoutA, stderrA := processA.stdout, processA.stderr
	stdoutB, stderrB := processB.stdout, processB.stderr
	barrierDeadline := time.Now().Add(35 * time.Second)
	for {
		_, errA := os.Stat(filepath.Join(barrierDir, "arrived.A"))
		_, errB := os.Stat(filepath.Join(barrierDir, "arrived.B"))
		if errA == nil && errB == nil {
			break
		}
		if (errA != nil && !os.IsNotExist(errA)) || (errB != nil && !os.IsNotExist(errB)) || time.Now().After(barrierDeadline) {
			cancel()
			_ = processA.group.Signal(os.Kill)
			_ = processB.group.Signal(os.Kill)
			waitErrA := joinRace(processA)
			waitErrB := joinRace(processB)
			t.Fatalf("concurrent contenders did not reach the shared guarded-claim barrier: marker A=%v marker B=%v wait A=%v wait B=%v stdout A=%q stderr A=%q stdout B=%q stderr B=%q",
				errA, errB, waitErrA, waitErrB, stdoutA.Bytes(), stderrA.Bytes(), stdoutB.Bytes(), stderrB.Bytes())
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, contender := range []string{"A", "B"} {
		snapshots, err := filepath.Glob(filepath.Join(barrierDir, "list."+contender+".*"))
		if err != nil {
			t.Fatalf("glob concurrent stale snapshot for %s: %v", contender, err)
		}
		if len(snapshots) == 0 {
			t.Fatalf("concurrent contender %s captured no direct stale snapshots", contender)
		}
		for _, snapshotPath := range snapshots {
			snapshot, err := os.ReadFile(snapshotPath)
			if err != nil {
				t.Fatalf("read concurrent stale snapshot for %s: %v", contender, err)
			}
			if !bytes.Contains(snapshot, []byte(raceBead)) {
				t.Fatalf("concurrent contender %s did not observe stale bead %q in authoritative snapshot %s: %s", contender, raceBead, snapshotPath, snapshot)
			}
			var captured struct {
				Issues []struct {
					ID        string    `json:"id"`
					Assignee  string    `json:"assignee"`
					UpdatedAt time.Time `json:"updated_at"`
				} `json:"issues"`
			}
			if err := json.Unmarshal(snapshot, &captured); err != nil {
				t.Fatalf("decode concurrent stale snapshot for %s: %v\npayload=%s", contender, err, snapshot)
			}
			matched := false
			for _, item := range captured.Issues {
				if item.ID != raceBead {
					continue
				}
				matched = true
				if strings.TrimSpace(item.Assignee) != "" || !item.UpdatedAt.Equal(raceUpdatedAt) {
					t.Fatalf("concurrent contender %s observed bead %q with assignee=%q updated_at=%s, want unowned at %s",
						contender, raceBead, item.Assignee, item.UpdatedAt.Format(time.RFC3339Nano), raceUpdatedAt.Format(time.RFC3339Nano))
				}
			}
			if !matched {
				t.Fatalf("concurrent contender %s snapshot omitted stale bead %q: %s", contender, raceBead, snapshot)
			}
		}
		claimSnapshots, err := filepath.Glob(filepath.Join(barrierDir, "info."+contender+".*"))
		if err != nil {
			t.Fatalf("glob concurrent guarded-claim snapshot for %s: %v", contender, err)
		}
		if len(claimSnapshots) != 1 {
			t.Fatalf("concurrent contender %s reached %d guarded-claim boundaries, want exactly one: %v", contender, len(claimSnapshots), claimSnapshots)
		}
	}
	waitRace := func(t *testing.T, process *raceProcess) int {
		t.Helper()
		_ = joinRace(process)
		if ctx.Err() != nil {
			t.Fatalf("concurrent bulk process timed out: %v stdout=%s stderr=%s", ctx.Err(), process.stdout.Bytes(), process.stderr.Bytes())
		}
		if process.closeErr != nil {
			t.Fatalf("close owned process group for concurrent bulk process: %v", process.closeErr)
		}
		if process.waitErr != nil {
			var exitErr *exec.ExitError
			if !errors.As(process.waitErr, &exitErr) {
				t.Fatalf("wait for concurrent bulk process: %v", process.waitErr)
			}
		}
		return process.cmd.ProcessState.ExitCode()
	}
	exitA := waitRace(t, processA)
	exitB := waitRace(t, processB)
	if len(bytes.TrimSpace(stderrA.Bytes())) != 0 || len(bytes.TrimSpace(stderrB.Bytes())) != 0 {
		t.Fatalf("concurrent bulk stderr A=%q B=%q", stderrA.Bytes(), stderrB.Bytes())
	}
	var raceA, raceB BulkAssignOutput
	decodeSingleRobotJSON(t, stdoutA.Bytes(), &raceA)
	decodeSingleRobotJSON(t, stdoutB.Bytes(), &raceB)
	type raceResult struct {
		fixture *robotProcessFixture
		exit    int
		output  BulkAssignOutput
	}
	results := []raceResult{{fixture: peerA, exit: exitA, output: raceA}, {fixture: peerB, exit: exitB, output: raceB}}
	var winner, loser *raceResult
	for i := range results {
		result := &results[i]
		if result.output.Success {
			winner = result
		} else {
			loser = result
		}
	}
	if winner == nil || loser == nil || winner.exit != 0 || loser.exit != 1 ||
		len(winner.output.Assignments) != 1 || len(loser.output.Assignments) != 1 {
		t.Fatalf("concurrent stale results A(exit=%d)=%+v B(exit=%d)=%+v", exitA, raceA, exitB, raceB)
	}
	winnerAssignment := winner.output.Assignments[0]
	loserAssignment := loser.output.Assignments[0]
	if winnerAssignment.Bead != raceBead || winnerAssignment.Status != "assigned" || !winnerAssignment.Claimed || !winnerAssignment.PromptSent ||
		winnerAssignment.ClaimActor == "" || winnerAssignment.DispatchReceiptID == "" ||
		loserAssignment.Bead != raceBead || loserAssignment.Status != "failed" || loserAssignment.Claimed || loserAssignment.PromptSent ||
		!strings.Contains(loserAssignment.Error, assignment.ErrClaimConflict.Error()) ||
		loser.output.ErrorCode != "ASSIGNMENT_FAILED" || loser.output.Summary.Failed != 1 {
		t.Fatalf("concurrent winner=%+v loser=%+v", winnerAssignment, loserAssignment)
	}
	winnerPane := captureStable(t, winner.fixture, 1)
	loserPane := loser.fixture.mustTMUXOutput(t, "capture-pane", "-p", "-t", loser.fixture.paneID, "-S", "-")
	winnerPromptStarts := strings.Count(winnerPane, "received: Read AGENTS.md")
	loserPromptStarts := strings.Count(loserPane, "received: Read AGENTS.md")
	if winnerPromptStarts != 1 || loserPromptStarts != 0 {
		t.Fatalf("concurrent dispatch starts winner=%d loser=%d\nwinner:\n%s\nloser:\n%s",
			winnerPromptStarts, loserPromptStarts, winnerPane, loserPane)
	}
	database, err = sql.Open(sqliteutil.DriverName, sqliteutil.FileDSN(workspaceInfo.DatabasePath, "busy_timeout(5000)", "foreign_keys(ON)"))
	if err != nil {
		t.Fatalf("reopen Beads database after concurrent stale race: %v", err)
	}
	if err := database.QueryRow("SELECT status, COALESCE(assignee, '') FROM issues WHERE id = ?", raceBead).Scan(&trackerStatus, &trackerAssignee); err != nil {
		_ = database.Close()
		t.Fatalf("read concurrent stale tracker row: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close concurrent stale tracker database: %v", err)
	}
	if trackerStatus != "in_progress" || trackerAssignee != winnerAssignment.ClaimActor {
		t.Fatalf("concurrent tracker status=%q assignee=%q, want in_progress/%q", trackerStatus, trackerAssignee, winnerAssignment.ClaimActor)
	}
}

func TestE2E_RobotBulkAssign_MultiPaneSession(t *testing.T) {
	CommonE2EPrerequisites(t)

	fixture := newRobotProcessFixture(t, "bulk-multi-pane")
	panes := prepareBulkAssignAgentPanes(t, fixture, 5)
	beads := prepareBulkAssignBeads(t, fixture, 5)

	// Test allocation to multiple panes
	allocation := make(map[string]string, len(panes))
	type expectedAssignment struct {
		paneID   string
		selector string
	}
	expectedByBead := make(map[string]expectedAssignment, len(panes))
	for index := range panes {
		allocation[panes[index].ID] = beads[index]
		expectedByBead[beads[index]] = expectedAssignment{paneID: panes[index].ID, selector: strconv.Itoa(panes[index].Index)}
	}
	allocationBytes, err := json.Marshal(allocation)
	if err != nil {
		t.Fatalf("[E2E-BULK-ASSIGN] marshal multi-pane allocation: %v", err)
	}
	result, process := runBulkAssignFixture(t, fixture,
		"--allocation="+string(allocationBytes),
		"--dry-run")

	if process.exitCode != 0 || !result.Success || !result.DryRun || result.AllocationSource != "explicit" {
		t.Fatalf("[E2E-BULK-ASSIGN] positive multi-pane envelope: exit=%d result=%+v", process.exitCode, result)
	}
	if result.Summary.TotalPanes != 5 || result.Summary.Assigned != 0 || result.Summary.Skipped != 0 ||
		result.Summary.Failed != 0 || len(result.Assignments) != 5 || len(result.UnassignedPanes) != 0 {
		t.Fatalf("[E2E-BULK-ASSIGN] result=%+v, want five resolved allocations and no unassigned panes", result)
	}
	for _, assignment := range result.Assignments {
		expected, ok := expectedByBead[assignment.Bead]
		if !ok || assignment.PaneID != expected.paneID || assignment.Pane != expected.selector ||
			assignment.Status != "planned" || assignment.PromptSent || assignment.Claimed || assignment.Error != "" {
			t.Fatalf("[E2E-BULK-ASSIGN] assignment=%+v did not preserve the one-to-one bead/pane plan", assignment)
		}
		delete(expectedByBead, assignment.Bead)
	}
	if len(expectedByBead) != 0 {
		t.Fatalf("[E2E-BULK-ASSIGN] missing planned bead/pane mappings: %v", expectedByBead)
	}
}

func TestE2E_RobotBulkAssign_AssignmentDetails(t *testing.T) {
	CommonE2EPrerequisites(t)

	fixture := newRobotProcessFixture(t, "bulk-details")
	pane := prepareBulkAssignAgentPanes(t, fixture, 1)[0]
	beadID := prepareBulkAssignBeads(t, fixture, 1)[0]
	allocation, err := json.Marshal(map[string]string{pane.ID: beadID})
	if err != nil {
		t.Fatalf("[E2E-BULK-ASSIGN] marshal assignment-details allocation: %v", err)
	}
	result, process := runBulkAssignFixture(t, fixture, "--allocation="+string(allocation), "--dry-run")
	if process.exitCode != 0 || !result.Success || result.ErrorCode != "" || !result.DryRun ||
		result.AllocationSource != "explicit" || len(result.Assignments) != 1 || result.Summary.Failed != 0 {
		t.Fatalf("[E2E-BULK-ASSIGN] assignment-details envelope: exit=%d result=%+v", process.exitCode, result)
	}
	assignment := result.Assignments[0]
	if _, err := tmux.ParsePaneSelector(assignment.Pane); err != nil {
		t.Fatalf("[E2E-BULK-ASSIGN] invalid canonical pane selector %q: %v", assignment.Pane, err)
	}
	if assignment.Pane != strconv.Itoa(pane.Index) || assignment.PaneID != pane.ID || assignment.Bead != beadID ||
		assignment.Status != "planned" || assignment.PromptSent || assignment.Claimed || assignment.Error != "" {
		t.Fatalf("[E2E-BULK-ASSIGN] assignment details=%+v", assignment)
	}
}

func TestE2E_RobotBulkAssign_SkipMultiplePanes(t *testing.T) {
	CommonE2EPrerequisites(t)

	fixture := newRobotProcessFixture(t, "bulk-skip-multiple")
	panes := prepareBulkAssignAgentPanes(t, fixture, 5)
	beads := prepareBulkAssignBeads(t, fixture, 5)

	// Skip multiple panes
	allocation := make(map[string]string, len(panes))
	expectedPaneByBead := make(map[string]tmux.Pane, len(panes))
	for index := range panes {
		allocation[panes[index].ID] = beads[index]
		expectedPaneByBead[beads[index]] = panes[index]
	}
	allocationBytes, err := json.Marshal(allocation)
	if err != nil {
		t.Fatalf("[E2E-BULK-ASSIGN] marshal skipped-pane allocation: %v", err)
	}
	skippedPaneIDs := map[string]struct{}{panes[0].ID: {}, panes[2].ID: {}, panes[4].ID: {}}
	result, process := runBulkAssignFixture(t, fixture,
		"--allocation="+string(allocationBytes),
		"--skip="+strings.Join([]string{panes[0].ID, panes[2].ID, panes[4].ID}, ","),
		"--dry-run")

	if process.exitCode != 1 || result.Success || !result.DryRun || result.ErrorCode != "PANE_NOT_FOUND" ||
		result.AllocationSource != "explicit" || result.Summary.TotalPanes != 5 || result.Summary.Assigned != 0 ||
		result.Summary.Skipped != 0 || result.Summary.Failed != 3 || len(result.Assignments) != 5 || len(result.UnassignedPanes) != 0 {
		t.Fatalf("[E2E-BULK-ASSIGN] skipped-pane envelope: exit=%d result=%+v", process.exitCode, result)
	}
	skipped, resolved := 0, 0
	for _, assignment := range result.Assignments {
		expectedPane, ok := expectedPaneByBead[assignment.Bead]
		if !ok {
			t.Fatalf("[E2E-BULK-ASSIGN] unexpected or duplicate bead assignment: %+v", assignment)
		}
		if _, shouldSkip := skippedPaneIDs[expectedPane.ID]; shouldSkip {
			if assignment.Pane != expectedPane.ID || assignment.PaneID != "" || assignment.Status != "failed" ||
				assignment.PromptSent || assignment.Claimed || !strings.Contains(strings.ToLower(assignment.Error), "not found") {
				t.Fatalf("[E2E-BULK-ASSIGN] skipped assignment=%+v, want a non-actuated pane failure", assignment)
			}
			skipped++
		} else {
			if assignment.Pane != strconv.Itoa(expectedPane.Index) || assignment.PaneID != expectedPane.ID ||
				assignment.Status != "planned" || assignment.PromptSent || assignment.Claimed || assignment.Error != "" {
				t.Fatalf("[E2E-BULK-ASSIGN] unskipped assignment did not preserve its physical pane plan: %+v", assignment)
			}
			resolved++
		}
		delete(expectedPaneByBead, assignment.Bead)
	}
	if skipped != 3 || resolved != 2 || len(expectedPaneByBead) != 0 {
		t.Fatalf("[E2E-BULK-ASSIGN] skipped=%d resolved=%d, want 3/2", skipped, resolved)
	}
}

func TestE2E_RobotBulkAssign_EmptyAllocation(t *testing.T) {
	CommonE2EPrerequisites(t)

	fixture := newRobotProcessFixture(t, "bulk-empty-allocation")
	result, process := runBulkAssignFixture(t, fixture, "--allocation={}")
	if process.exitCode != 1 || result.Success || result.ErrorCode != "INVALID_FLAG" ||
		result.Assignments == nil || result.UnassignedBeads == nil || result.UnassignedPanes == nil ||
		result.Summary != (BulkAssignSummary{}) ||
		!strings.Contains(strings.ToLower(result.Error), "at least one") {
		t.Fatalf("[E2E-BULK-ASSIGN] empty-allocation envelope: exit=%d result=%+v", process.exitCode, result)
	}
}

func TestE2E_RobotBulkAssign_NonExistentSession(t *testing.T) {
	CommonE2EPrerequisites(t)

	fixture := newRobotProcessFixture(t, "bulk-missing-session-control")
	missingSession := fixture.session + "-missing"
	process := runBuiltRobotProcessWithin(t, defaultRunTimeout, fixture.ntmPath, fixture.projectDir, fixture.env,
		"--robot-bulk-assign="+missingSession,
		"--robot-format=json",
		`--allocation={"0":"missing-session-bead"}`,
		"--dry-run",
	)
	if process.exitCode != 1 || len(bytes.TrimSpace(process.stderr)) != 0 {
		t.Fatalf("[E2E-BULK-ASSIGN] nonexistent-session process exit=%d stdout=%s stderr=%s", process.exitCode, process.stdout, process.stderr)
	}
	var result BulkAssignOutput
	decodeSingleRobotJSON(t, process.stdout, &result)
	if result.Success || result.ErrorCode != "SESSION_NOT_FOUND" || result.Session != missingSession || !result.DryRun ||
		result.Assignments == nil || result.UnassignedBeads == nil || result.UnassignedPanes == nil ||
		result.Summary != (BulkAssignSummary{}) || !strings.Contains(strings.ToLower(result.Error), "panes") {
		t.Fatalf("[E2E-BULK-ASSIGN] nonexistent-session envelope=%+v", result)
	}
}

func TestE2E_RobotBulkAssign_PaneIndexValidation(t *testing.T) {
	CommonE2EPrerequisites(t)

	suite := NewTestSuite(t, "bulk_assign_pane_validation")
	defer suite.Teardown()

	if err := suite.Setup(); err != nil {
		t.Fatalf("[E2E-BULK-ASSIGN] Setup failed: %v", err)
	}

	// A well-formed selector for a pane that does not exist must be a typed failure.
	allocation := `{"99":"bd-nonexistent-pane"}`
	result, output, runErr := runBulkAssignCmd(t, suite, suite.Session(),
		"--allocation="+allocation,
		"--dry-run")

	if result == nil {
		t.Fatalf("[E2E-BULK-ASSIGN] expected JSON response: err=%v output=%s", runErr, output)
	}
	if runErr == nil || result.Success || !result.DryRun || result.ErrorCode != "PANE_NOT_FOUND" ||
		result.AllocationSource != "explicit" || result.Summary.TotalPanes != 1 ||
		result.Summary.Assigned != 0 || result.Summary.Skipped != 0 || result.Summary.Failed != 1 {
		t.Fatalf("[E2E-BULK-ASSIGN] missing pane did not produce the typed failure contract: err=%v result=%+v output=%s",
			runErr, result, output)
	}
	if len(result.Assignments) != 1 {
		t.Fatalf("[E2E-BULK-ASSIGN] assignments=%+v, want exactly one failed pane-99 assignment", result.Assignments)
	}
	assignment := result.Assignments[0]
	if assignment.Pane != "99" || assignment.PaneID != "" || assignment.Bead != "bd-nonexistent-pane" ||
		assignment.Status != "failed" || assignment.PromptSent || assignment.Claimed ||
		!strings.Contains(strings.ToLower(assignment.Error), "not found") {
		t.Fatalf("[E2E-BULK-ASSIGN] assignment=%+v, want a non-actuated PANE_NOT_FOUND result for pane 99", assignment)
	}
}

func TestE2E_RobotBulkAssign_TimestampFormat(t *testing.T) {
	CommonE2EPrerequisites(t)

	fixture := newRobotProcessFixture(t, "bulk-timestamp")
	pane := prepareBulkAssignAgentPanes(t, fixture, 1)[0]
	beadID := prepareBulkAssignBeads(t, fixture, 1)[0]
	allocation, err := json.Marshal(map[string]string{pane.ID: beadID})
	if err != nil {
		t.Fatalf("[E2E-BULK-ASSIGN] marshal timestamp allocation: %v", err)
	}
	result, process := runBulkAssignFixture(t, fixture, "--allocation="+string(allocation), "--dry-run")
	if process.exitCode != 0 || !result.Success || result.ErrorCode != "" || len(result.Assignments) != 1 {
		t.Fatalf("[E2E-BULK-ASSIGN] timestamp envelope: exit=%d result=%+v", process.exitCode, result)
	}
	if _, err := time.Parse(time.RFC3339, result.Timestamp); err != nil {
		t.Fatalf("[E2E-BULK-ASSIGN] timestamp %q is not RFC3339: %v", result.Timestamp, err)
	}
}

func TestE2E_RobotBulkAssign_AllStrategiesValidInput(t *testing.T) {
	CommonE2EPrerequisites(t)

	strategies := []string{"impact", "ready", "stale", "balanced"}

	for _, strategy := range strategies {
		t.Run("valid_"+strategy, func(t *testing.T) {
			fixture := newRobotProcessFixture(t, "bulk-valid-"+strategy)
			pane := prepareBulkAssignAgentPanes(t, fixture, 1)[0]
			beadID := prepareBulkAssignBeads(t, fixture, 1)[0]
			allocation, err := json.Marshal(map[string]string{pane.ID: beadID})
			if err != nil {
				t.Fatalf("[E2E-BULK-ASSIGN] marshal %s strategy allocation: %v", strategy, err)
			}
			result, process := runBulkAssignFixture(t, fixture,
				"--allocation="+string(allocation),
				"--strategy="+strategy,
				"--dry-run",
			)
			if process.exitCode != 0 || !result.Success || result.ErrorCode != "" || !result.DryRun ||
				result.Strategy != strategy || result.AllocationSource != "explicit" ||
				result.Summary.TotalPanes != 1 || result.Summary.Failed != 0 || len(result.Assignments) != 1 {
				t.Fatalf("[E2E-BULK-ASSIGN] valid %s strategy envelope: exit=%d result=%+v", strategy, process.exitCode, result)
			}
			assignment := result.Assignments[0]
			if assignment.PaneID != pane.ID || assignment.Bead != beadID || assignment.Status != "planned" ||
				assignment.PromptSent || assignment.Claimed || assignment.Error != "" {
				t.Fatalf("[E2E-BULK-ASSIGN] valid %s strategy assignment=%+v", strategy, assignment)
			}
		})
	}
}

func TestE2E_RobotBulkAssign_CombinedFlags(t *testing.T) {
	CommonE2EPrerequisites(t)

	fixture := newRobotProcessFixture(t, "bulk-combined-flags")
	panes := prepareBulkAssignAgentPanes(t, fixture, 3)
	beads := prepareBulkAssignBeads(t, fixture, 3)

	// Test multiple flags combined
	allocation := make(map[string]string, len(panes))
	expectedPaneByBead := make(map[string]tmux.Pane, len(panes))
	for index := range panes {
		allocation[panes[index].ID] = beads[index]
		expectedPaneByBead[beads[index]] = panes[index]
	}
	allocationBytes, err := json.Marshal(allocation)
	if err != nil {
		t.Fatalf("[E2E-BULK-ASSIGN] marshal combined allocation: %v", err)
	}
	result, process := runBulkAssignFixture(t, fixture,
		"--allocation="+string(allocationBytes),
		"--skip="+panes[1].ID,
		"--strategy=impact",
		"--dry-run")

	if process.exitCode != 1 || result.Success || !result.DryRun || result.ErrorCode != "PANE_NOT_FOUND" ||
		result.AllocationSource != "explicit" || result.Strategy != "impact" || result.Summary.TotalPanes != 3 ||
		result.Summary.Assigned != 0 || result.Summary.Skipped != 0 || result.Summary.Failed != 1 ||
		len(result.Assignments) != 3 || len(result.UnassignedPanes) != 0 {
		t.Fatalf("[E2E-BULK-ASSIGN] combined envelope: exit=%d result=%+v", process.exitCode, result)
	}
	skipped, resolved := 0, 0
	for _, assignment := range result.Assignments {
		expectedPane, ok := expectedPaneByBead[assignment.Bead]
		if !ok {
			t.Fatalf("[E2E-BULK-ASSIGN] unexpected or duplicate combined bead assignment: %+v", assignment)
		}
		if expectedPane.ID == panes[1].ID {
			if assignment.Pane != expectedPane.ID || assignment.PaneID != "" || assignment.Status != "failed" ||
				assignment.PromptSent || assignment.Claimed || !strings.Contains(strings.ToLower(assignment.Error), "not found") {
				t.Fatalf("[E2E-BULK-ASSIGN] combined skipped assignment=%+v", assignment)
			}
			skipped++
		} else {
			if assignment.Pane != strconv.Itoa(expectedPane.Index) || assignment.PaneID != expectedPane.ID ||
				assignment.Status != "planned" || assignment.PromptSent || assignment.Claimed || assignment.Error != "" {
				t.Fatalf("[E2E-BULK-ASSIGN] combined unskipped assignment did not preserve its plan: %+v", assignment)
			}
			resolved++
		}
		delete(expectedPaneByBead, assignment.Bead)
	}
	if skipped != 1 || resolved != 2 || len(expectedPaneByBead) != 0 {
		t.Fatalf("[E2E-BULK-ASSIGN] combined skipped=%d resolved=%d, want 1/2", skipped, resolved)
	}
}

func TestE2E_RobotBulkAssign_InvalidOrMissingSelectedConfigHasZeroSideEffects(t *testing.T) {
	CommonE2EPrerequisites(t)

	for _, test := range []struct {
		name          string
		missingConfig bool
	}{
		{name: "invalid selected config"},
		{name: "missing selected config", missingConfig: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRobotProcessFixture(t, "bulk-policy-invalid")
			beads := prepareBulkAssignBeads(t, fixture, 1)
			beadID := beads[0]
			beforeBead := readBulkAssignFixtureBead(t, fixture, beadID)
			mail := newAtomicAssignmentMailStub(fixture.projectDir)
			t.Cleanup(mail.close)

			triagePayload := fmt.Sprintf(
				`{"triage":{"recommendations":[{"id":%q,"title":"Policy must load first","type":"task","status":"open","priority":1,"score":100,"action":"assign","reasons":[]}]}}`,
				beadID,
			)
			planPayload := fmt.Sprintf(
				`{"plan":{"tracks":[{"track_id":"policy-first","items":[{"id":%q,"title":"Policy must load first","status":"open","priority":1}]}]}}`,
				beadID,
			)
			baseEnv := atomicAssignmentMergeEnv(fixture.env, mail.env())
			env, bvTrace, brTrace := installBulkAssignPlanningFixtures(
				t, fixture, baseEnv, triagePayload, planPayload, "ok", "real",
			)
			configPath := filepath.Join(fixture.root, "selected-policy.toml")
			invalidConfig := []byte("[assign\noperator_gated_labels = [\"release-approval\"]\n")
			if !test.missingConfig {
				if err := os.WriteFile(configPath, invalidConfig, 0o600); err != nil {
					t.Fatalf("write invalid selected bulk config: %v", err)
				}
			}
			outsideDir := filepath.Join(fixture.root, "outside")
			if err := os.MkdirAll(outsideDir, 0o700); err != nil {
				t.Fatalf("create bulk caller directory: %v", err)
			}
			marker := fmt.Sprintf("NTM_BULK_INVALID_POLICY_MUST_NOT_DISPATCH_%d", time.Now().UnixNano())
			templatePath := filepath.Join(fixture.projectDir, "invalid-policy-template.txt")
			if err := os.WriteFile(templatePath, []byte(marker+"::{bead_id}"), 0o600); err != nil {
				t.Fatalf("write invalid-policy bulk template: %v", err)
			}

			process := runBuiltRobotProcessWithin(
				t, defaultRunTimeout, fixture.ntmPath, outsideDir, env,
				"--config="+configPath,
				"--robot-bulk-assign="+fixture.session,
				"--from-bv",
				"--bulk-strategy=balanced",
				"--prompt-template="+templatePath,
				"--robot-format=json",
			)
			if process.exitCode != 1 || len(bytes.TrimSpace(process.stderr)) != 0 {
				t.Fatalf("bulk invalid policy exit=%d stdout=%s stderr=%s", process.exitCode, process.stdout, process.stderr)
			}
			var result BulkAssignOutput
			decodeSingleRobotJSON(t, process.stdout, &result)
			if result.Success || result.ErrorCode != "INVALID_FLAG" ||
				!strings.Contains(strings.ToLower(result.Error), "config") {
				t.Fatalf("bulk invalid policy envelope = %+v, want one INVALID_FLAG document", result)
			}
			if test.missingConfig {
				if result.Assignments == nil {
					t.Fatalf("command-specific missing-config envelope assignments=nil, want checked-empty []")
				}
			} else {
				var rootFailure struct {
					Meta struct {
						ExitCode int    `json:"exit_code"`
						Command  string `json:"command"`
					} `json:"_meta"`
				}
				decodeSingleRobotJSON(t, process.stdout, &rootFailure)
				if rootFailure.Meta.ExitCode != 1 || rootFailure.Meta.Command != "robot-bulk-assign" || result.Assignments != nil {
					t.Fatalf("invalid TOML root envelope meta=%+v assignments=%v, want command-owned generic failure", rootFailure.Meta, result.Assignments)
				}
			}
			assertBulkAssignTraceEmpty(t, bvTrace, "bv planning")
			assertBulkAssignTraceEmpty(t, brTrace, "Beads")
			if test.missingConfig {
				if _, err := os.Stat(configPath); !os.IsNotExist(err) {
					t.Fatalf("missing selected bulk config was created: %v", err)
				}
			} else {
				afterConfig, err := os.ReadFile(configPath)
				if err != nil {
					t.Fatalf("read invalid bulk config after rejection: %v", err)
				}
				if !bytes.Equal(afterConfig, invalidConfig) {
					t.Fatalf("bulk rejection mutated invalid config:\nbefore=%s\nafter=%s", invalidConfig, afterConfig)
				}
			}
			afterBead := readBulkAssignFixtureBead(t, fixture, beadID)
			if !bytes.Equal(bytes.TrimSpace(afterBead), bytes.TrimSpace(beforeBead)) {
				t.Fatalf("bulk invalid policy mutated Beads state:\nbefore=%s\nafter=%s", beforeBead, afterBead)
			}
			assertBulkAssignFixtureHasNoLedger(t, fixture, beadID)
			assertAtomicAssignmentMailUntouched(t, mail.snapshot())
			if paneOutput := fixture.mustTMUXOutput(t, "capture-pane", "-p", "-t", fixture.paneID, "-S", "-"); strings.Contains(paneOutput, marker) {
				t.Fatalf("invalid bulk policy dispatched marker %q to pane: %s", marker, paneOutput)
			}
		})
	}
}

func TestE2E_RobotBulkAssign_InvalidTargetProjectConfigStopsBeforePlanningAndProjection(t *testing.T) {
	CommonE2EPrerequisites(t)

	for _, test := range []struct {
		name      string
		directory bool
	}{
		{name: "invalid TOML"},
		{name: "non-regular config path", directory: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRobotProcessFixture(t, "bulk-target-policy-invalid")
			beadID := prepareBulkAssignBeads(t, fixture, 1)[0]
			beforeBead := readBulkAssignFixtureBead(t, fixture, beadID)
			mail := newAtomicAssignmentMailStub(fixture.projectDir)
			t.Cleanup(mail.close)

			triagePayload := fmt.Sprintf(
				`{"triage":{"recommendations":[{"id":%q,"title":"Target policy must load first","type":"task","status":"open","priority":1,"score":100,"action":"assign","reasons":[]}]}}`,
				beadID,
			)
			planPayload := fmt.Sprintf(
				`{"plan":{"tracks":[{"track_id":"target-policy-first","items":[{"id":%q,"title":"Target policy must load first","status":"open","priority":1}]}]}}`,
				beadID,
			)
			env, bvTrace, brTrace := installBulkAssignPlanningFixtures(
				t, fixture, atomicAssignmentMergeEnv(fixture.env, mail.env()), triagePayload, planPayload, "ok", "real",
			)

			projectConfigDir := filepath.Join(fixture.projectDir, ".ntm")
			if err := os.MkdirAll(projectConfigDir, 0o700); err != nil {
				t.Fatalf("create invalid target bulk config directory: %v", err)
			}
			projectConfigPath := filepath.Join(projectConfigDir, "config.toml")
			invalidConfig := []byte("[assign\noperator_gated_labels = [\"release-approval\"]\n")
			if test.directory {
				if err := os.MkdirAll(projectConfigPath, 0o700); err != nil {
					t.Fatalf("create non-regular target bulk config: %v", err)
				}
			} else if err := os.WriteFile(projectConfigPath, invalidConfig, 0o600); err != nil {
				t.Fatalf("write invalid target bulk config: %v", err)
			}

			outsideDir := filepath.Join(fixture.root, "outside-target")
			if err := os.MkdirAll(outsideDir, 0o700); err != nil {
				t.Fatalf("create outside target bulk directory: %v", err)
			}
			marker := fmt.Sprintf("NTM_BULK_TARGET_POLICY_MUST_NOT_DISPATCH_%d", time.Now().UnixNano())
			templatePath := filepath.Join(fixture.projectDir, "invalid-target-policy-template.txt")
			if err := os.WriteFile(templatePath, []byte(marker+"::{bead_id}"), 0o600); err != nil {
				t.Fatalf("write invalid target policy bulk template: %v", err)
			}

			process := runBuiltRobotProcessWithin(
				t, defaultRunTimeout, fixture.ntmPath, outsideDir, env,
				"--robot-bulk-assign="+fixture.session,
				"--from-bv",
				"--bulk-strategy=balanced",
				"--prompt-template="+templatePath,
				"--robot-format=json",
			)
			if process.exitCode != 1 || len(bytes.TrimSpace(process.stderr)) != 0 {
				t.Fatalf("bulk invalid target policy exit=%d stdout=%s stderr=%s", process.exitCode, process.stdout, process.stderr)
			}
			var result BulkAssignOutput
			decodeSingleRobotJSON(t, process.stdout, &result)
			if result.Success || result.ErrorCode != "INVALID_FLAG" || result.Assignments == nil ||
				result.UnassignedBeads == nil || result.UnassignedPanes == nil ||
				!strings.Contains(strings.ToLower(result.Error), "config") {
				t.Fatalf("bulk invalid target policy envelope = %+v, want checked-empty INVALID_FLAG response", result)
			}

			assertBulkAssignTraceEmpty(t, bvTrace, "bv planning or startup projection")
			assertBulkAssignTraceEmpty(t, brTrace, "Beads planning or mutation")
			assertBulkAssignFixtureHasNoProjectionRows(t, fixture)
			afterBead := readBulkAssignFixtureBead(t, fixture, beadID)
			if !bytes.Equal(bytes.TrimSpace(afterBead), bytes.TrimSpace(beforeBead)) {
				t.Fatalf("bulk invalid target policy mutated Beads state:\nbefore=%s\nafter=%s", beforeBead, afterBead)
			}
			assertBulkAssignFixtureHasNoLedger(t, fixture, beadID)
			assertAtomicAssignmentMailUntouched(t, mail.snapshot())
			if paneOutput := fixture.mustTMUXOutput(t, "capture-pane", "-p", "-t", fixture.paneID, "-S", "-"); strings.Contains(paneOutput, marker) {
				t.Fatalf("invalid target bulk policy dispatched marker %q to pane: %s", marker, paneOutput)
			}
			if test.directory {
				info, err := os.Stat(projectConfigPath)
				if err != nil || !info.IsDir() {
					t.Fatalf("non-regular target bulk config changed: info=%v err=%v", info, err)
				}
			} else {
				afterConfig, err := os.ReadFile(projectConfigPath)
				if err != nil {
					t.Fatalf("read invalid target bulk config after rejection: %v", err)
				}
				if !bytes.Equal(afterConfig, invalidConfig) {
					t.Fatalf("bulk rejection mutated invalid target config:\nbefore=%s\nafter=%s", invalidConfig, afterConfig)
				}
			}
		})
	}
}

func TestE2E_RobotBulkAssign_CrossCWDTargetPolicyRejectsOverlappingLiveGate(t *testing.T) {
	CommonE2EPrerequisites(t)

	fixture := newRobotProcessFixture(t, "bulk-cross-cwd-gate")
	beads := prepareBulkAssignBeads(t, fixture, 1)
	beadID := beads[0]
	brPath, err := exec.LookPath("br")
	if err != nil {
		t.Fatalf("real br is required for bulk live-gate fixture: %v", err)
	}
	const gateLabel = "architecture-approval"
	runRobotFixtureTool(t, fixture, defaultRunTimeout, brPath,
		"update", beadID, "--add-label="+gateLabel, "--json",
	)
	beforeBead := readBulkAssignFixtureBead(t, fixture, beadID)
	projectConfigDir := filepath.Join(fixture.projectDir, ".ntm")
	if err := os.MkdirAll(projectConfigDir, 0o700); err != nil {
		t.Fatalf("create bulk project config directory: %v", err)
	}
	projectConfig := []byte("[assign]\noperator_gated_labels = [\"" + gateLabel + "\"]\n")
	projectConfigPath := filepath.Join(projectConfigDir, "config.toml")
	if err := os.WriteFile(projectConfigPath, projectConfig, 0o600); err != nil {
		t.Fatalf("write target bulk policy: %v", err)
	}

	triagePayload := fmt.Sprintf(
		`{"triage":{"recommendations":[{"id":%q,"title":"Stale unlabeled triage row","type":"task","status":"open","priority":0,"score":999,"action":"assign","reasons":[],"labels":[]}],"blockers_to_clear":[{"id":%q,"title":"Overlapping high-impact row","unblocks_count":99,"actionable":true}]}}`,
		beadID, beadID,
	)
	planPayload := fmt.Sprintf(
		`{"plan":{"tracks":[{"track_id":"overlapping-live-gate","items":[{"id":%q,"title":"Plan omits labels","status":"open","priority":0,"unblocks":["bd-child"]}]}]}}`,
		beadID,
	)
	mail := newAtomicAssignmentMailStub(fixture.projectDir)
	t.Cleanup(mail.close)
	baseEnv := atomicAssignmentMergeEnv(fixture.env, mail.env())
	env, bvTrace, brTrace := installBulkAssignPlanningFixtures(
		t, fixture, baseEnv, triagePayload, planPayload, "ok", "real",
	)
	outsideDir := filepath.Join(fixture.root, "unrelated-caller")
	if err := os.MkdirAll(outsideDir, 0o700); err != nil {
		t.Fatalf("create unrelated bulk caller directory: %v", err)
	}
	marker := fmt.Sprintf("NTM_BULK_CROSS_CWD_GATE_MUST_NOT_DISPATCH_%d", time.Now().UnixNano())
	templatePath := filepath.Join(fixture.projectDir, "cross-cwd-gate-template.txt")
	if err := os.WriteFile(templatePath, []byte(marker+"::{bead_id}"), 0o600); err != nil {
		t.Fatalf("write cross-CWD bulk template: %v", err)
	}

	process := runBuiltRobotProcessWithin(
		t, defaultRunTimeout, fixture.ntmPath, outsideDir, env,
		"--robot-bulk-assign="+fixture.session,
		"--from-bv",
		"--bulk-strategy=impact",
		"--prompt-template="+templatePath,
		"--robot-format=json",
	)
	if process.exitCode != 0 || len(bytes.TrimSpace(process.stderr)) != 0 {
		t.Fatalf("cross-CWD live gate exit=%d stdout=%s stderr=%s", process.exitCode, process.stdout, process.stderr)
	}
	var result BulkAssignOutput
	decodeSingleRobotJSON(t, process.stdout, &result)
	if !result.Success || result.ErrorCode != "" || result.AllocationSource != "bv" ||
		len(result.Assignments) != 0 || len(result.UnassignedBeads) != 0 ||
		result.Summary.TotalPanes != 1 || result.Summary.Assigned != 0 || result.Summary.Failed != 0 {
		t.Fatalf("cross-CWD live gate envelope = %+v, want no authorized assignments", result)
	}
	bvCalls, err := os.ReadFile(bvTrace)
	if err != nil {
		t.Fatalf("read cross-CWD bv trace: %v", err)
	}
	for _, want := range []string{"--robot-triage", "--robot-plan"} {
		if !bytes.Contains(bvCalls, []byte(want)) {
			t.Fatalf("cross-CWD bulk planning omitted %q:\n%s", want, bvCalls)
		}
	}
	brCalls, err := os.ReadFile(brTrace)
	if err != nil {
		t.Fatalf("read cross-CWD br trace: %v", err)
	}
	for _, want := range []string{
		"ready --json --limit 100000",
		"list --json --status open --limit 100000",
	} {
		if !bytes.Contains(brCalls, []byte(want)) {
			t.Fatalf("cross-CWD bulk planning omitted live-label lookup %q:\n%s", want, brCalls)
		}
	}
	afterBead := readBulkAssignFixtureBead(t, fixture, beadID)
	if !bytes.Equal(bytes.TrimSpace(afterBead), bytes.TrimSpace(beforeBead)) {
		t.Fatalf("cross-CWD live gate mutated Beads state:\nbefore=%s\nafter=%s", beforeBead, afterBead)
	}
	afterConfig, err := os.ReadFile(projectConfigPath)
	if err != nil {
		t.Fatalf("read target bulk policy after command: %v", err)
	}
	if !bytes.Equal(afterConfig, projectConfig) {
		t.Fatalf("cross-CWD bulk command mutated target policy:\nbefore=%s\nafter=%s", projectConfig, afterConfig)
	}
	assertBulkAssignFixtureHasNoLedger(t, fixture, beadID)
	assertAtomicAssignmentMailUntouched(t, mail.snapshot())
	if paneOutput := fixture.mustTMUXOutput(t, "capture-pane", "-p", "-t", fixture.paneID, "-S", "-"); strings.Contains(paneOutput, marker) {
		t.Fatalf("cross-CWD live gate dispatched marker %q to pane: %s", marker, paneOutput)
	}
}

func TestE2E_RobotBulkAssign_PlanAndLabelVerificationFailuresHaveZeroMutation(t *testing.T) {
	CommonE2EPrerequisites(t)

	for _, test := range []struct {
		name      string
		planMode  string
		labelMode string
		wantTrace string
	}{
		{name: "plan command failure", planMode: "failure", labelMode: "real", wantTrace: "--robot-plan"},
		{name: "plan item absent from live labels", planMode: "ok", labelMode: "empty", wantTrace: "list --json --status open --limit 100000"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRobotProcessFixture(t, "bulk-verification-failure")
			beads := prepareBulkAssignBeads(t, fixture, 1)
			beadID := beads[0]
			beforeBead := readBulkAssignFixtureBead(t, fixture, beadID)
			triagePayload := fmt.Sprintf(
				`{"triage":{"recommendations":[{"id":%q,"title":"Unverified bulk work","type":"task","status":"open","priority":1,"score":100,"action":"assign","reasons":[]}]}}`,
				beadID,
			)
			planPayload := fmt.Sprintf(
				`{"plan":{"tracks":[{"track_id":"verification-failure","items":[{"id":%q,"title":"Unverified bulk work","status":"open","priority":1}]}]}}`,
				beadID,
			)
			mail := newAtomicAssignmentMailStub(fixture.projectDir)
			t.Cleanup(mail.close)
			baseEnv := atomicAssignmentMergeEnv(fixture.env, mail.env())
			env, bvTrace, brTrace := installBulkAssignPlanningFixtures(
				t, fixture, baseEnv, triagePayload, planPayload, test.planMode, test.labelMode,
			)
			marker := fmt.Sprintf("NTM_BULK_VERIFICATION_MUST_NOT_DISPATCH_%d", time.Now().UnixNano())
			templatePath := filepath.Join(fixture.projectDir, "verification-failure-template.txt")
			if err := os.WriteFile(templatePath, []byte(marker+"::{bead_id}"), 0o600); err != nil {
				t.Fatalf("write verification-failure bulk template: %v", err)
			}

			process := runBuiltRobotProcessWithin(
				t, defaultRunTimeout, fixture.ntmPath, fixture.projectDir, env,
				"--robot-bulk-assign="+fixture.session,
				"--from-bv",
				"--bulk-strategy=balanced",
				"--prompt-template="+templatePath,
				"--robot-format=json",
			)
			if process.exitCode != 1 || len(bytes.TrimSpace(process.stderr)) != 0 {
				t.Fatalf("bulk verification failure exit=%d stdout=%s stderr=%s", process.exitCode, process.stdout, process.stderr)
			}
			var result BulkAssignOutput
			decodeSingleRobotJSON(t, process.stdout, &result)
			if result.Success || result.ErrorCode != "INTERNAL_ERROR" || result.Assignments == nil ||
				!strings.Contains(result.Error, "verify actionable bulk assignment work") {
				t.Fatalf("bulk verification failure envelope = %+v", result)
			}
			traces := make([]byte, 0)
			if data, err := os.ReadFile(bvTrace); err == nil {
				traces = append(traces, data...)
			} else if !os.IsNotExist(err) {
				t.Fatalf("read bulk verification bv trace: %v", err)
			}
			if data, err := os.ReadFile(brTrace); err == nil {
				traces = append(traces, data...)
			} else if !os.IsNotExist(err) {
				t.Fatalf("read bulk verification br trace: %v", err)
			}
			if !bytes.Contains(traces, []byte(test.wantTrace)) {
				t.Fatalf("bulk verification failure did not reach expected read-only boundary %q:\n%s", test.wantTrace, traces)
			}
			afterBead := readBulkAssignFixtureBead(t, fixture, beadID)
			if !bytes.Equal(bytes.TrimSpace(afterBead), bytes.TrimSpace(beforeBead)) {
				t.Fatalf("bulk verification failure mutated Beads state:\nbefore=%s\nafter=%s", beforeBead, afterBead)
			}
			assertBulkAssignFixtureHasNoLedger(t, fixture, beadID)
			assertAtomicAssignmentMailUntouched(t, mail.snapshot())
			if paneOutput := fixture.mustTMUXOutput(t, "capture-pane", "-p", "-t", fixture.paneID, "-S", "-"); strings.Contains(paneOutput, marker) {
				t.Fatalf("bulk verification failure dispatched marker %q to pane: %s", marker, paneOutput)
			}
		})
	}
}

func TestE2E_RobotBulkAssign_LargeAllocation(t *testing.T) {
	CommonE2EPrerequisites(t)

	fixture := newRobotProcessFixture(t, "bulk-large")
	panes := prepareBulkAssignAgentPanes(t, fixture, 20)
	beads := prepareBulkAssignBeads(t, fixture, 20)
	allocation := make(map[string]string, len(panes))
	expected := make(map[string]tmux.Pane, len(panes))
	for index := range panes {
		allocation[panes[index].ID] = beads[index]
		expected[beads[index]] = panes[index]
	}
	encoded, err := json.Marshal(allocation)
	if err != nil {
		t.Fatalf("[E2E-BULK-ASSIGN] marshal large allocation: %v", err)
	}
	result, process := runBulkAssignFixture(t, fixture, "--allocation="+string(encoded), "--dry-run")
	if process.exitCode != 0 || !result.Success || result.ErrorCode != "" || !result.DryRun ||
		result.AllocationSource != "explicit" || result.Summary.TotalPanes != 20 ||
		result.Summary.Assigned != 0 || result.Summary.Skipped != 0 || result.Summary.Failed != 0 ||
		len(result.Assignments) != 20 || len(result.UnassignedBeads) != 0 || len(result.UnassignedPanes) != 0 {
		t.Fatalf("[E2E-BULK-ASSIGN] large-allocation envelope: exit=%d result=%+v", process.exitCode, result)
	}
	for _, assignment := range result.Assignments {
		pane, ok := expected[assignment.Bead]
		if !ok || assignment.Pane != strconv.Itoa(pane.Index) || assignment.PaneID != pane.ID ||
			assignment.Status != "planned" || assignment.PromptSent || assignment.Claimed || assignment.Error != "" {
			t.Fatalf("[E2E-BULK-ASSIGN] large assignment=%+v", assignment)
		}
		delete(expected, assignment.Bead)
	}
	if len(expected) != 0 {
		t.Fatalf("[E2E-BULK-ASSIGN] large allocation omitted mappings: %v", expected)
	}
}

func TestE2E_RobotBulkAssign_SkipPanesFormat(t *testing.T) {
	CommonE2EPrerequisites(t)

	testCases := []struct {
		name            string
		skipPanes       func(string, string) string
		allocatedPanes  []int
		unassignedPanes []int
	}{
		{
			name:            "single",
			skipPanes:       func(_, secondPaneID string) string { return secondPaneID },
			allocatedPanes:  []int{1},
			unassignedPanes: []int{0},
		},
		{
			name:           "comma_separated",
			skipPanes:      func(firstPaneID, secondPaneID string) string { return firstPaneID + "," + secondPaneID },
			allocatedPanes: []int{0, 1},
		},
		{
			name:           "with_spaces",
			skipPanes:      func(firstPaneID, secondPaneID string) string { return firstPaneID + ", " + secondPaneID },
			allocatedPanes: []int{0, 1},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRobotProcessFixture(t, "bulk-skip-format-"+tc.name)
			panes := prepareBulkAssignAgentPanes(t, fixture, 2)
			firstPaneID, secondPaneID := panes[0].ID, panes[1].ID
			allocation := make(map[string]string, len(tc.allocatedPanes))
			expectedByPaneID := make(map[string]string, len(tc.allocatedPanes))
			for _, paneIndex := range tc.allocatedPanes {
				beadID := fmt.Sprintf("bd-skip-format-%d", paneIndex)
				allocation[panes[paneIndex].ID] = beadID
				expectedByPaneID[panes[paneIndex].ID] = beadID
			}
			allocationBytes, err := json.Marshal(allocation)
			if err != nil {
				t.Fatalf("[E2E-BULK-ASSIGN] marshal format-test allocation: %v", err)
			}
			result, process := runBulkAssignFixture(t, fixture,
				"--allocation="+string(allocationBytes),
				"--skip="+tc.skipPanes(firstPaneID, secondPaneID),
				"--dry-run")

			if process.exitCode != 1 || result.Success || !result.DryRun || result.ErrorCode != "PANE_NOT_FOUND" ||
				result.AllocationSource != "explicit" || result.Summary.TotalPanes != 2 || result.Summary.Assigned != 0 ||
				result.Summary.Skipped != 0 || result.Summary.Failed != len(tc.allocatedPanes) ||
				len(result.Assignments) != len(tc.allocatedPanes) || len(result.UnassignedPanes) != len(tc.unassignedPanes) {
				t.Fatalf("[E2E-BULK-ASSIGN] skip format %s envelope: exit=%d result=%+v", tc.name, process.exitCode, result)
			}
			for _, assignment := range result.Assignments {
				beadID, ok := expectedByPaneID[assignment.Pane]
				if !ok || assignment.Bead != beadID || assignment.PaneID != "" || assignment.Status != "failed" ||
					assignment.PromptSent || assignment.Claimed || !strings.Contains(strings.ToLower(assignment.Error), "not found") {
					t.Fatalf("[E2E-BULK-ASSIGN] skip format %s assignment=%+v did not prove its selector", tc.name, assignment)
				}
				delete(expectedByPaneID, assignment.Pane)
			}
			if len(expectedByPaneID) != 0 {
				t.Fatalf("[E2E-BULK-ASSIGN] skip format %s omitted selectors %v", tc.name, expectedByPaneID)
			}
			expectedUnassigned := make(map[string]struct{}, len(tc.unassignedPanes))
			for _, paneIndex := range tc.unassignedPanes {
				expectedUnassigned[strconv.Itoa(panes[paneIndex].Index)] = struct{}{}
			}
			for _, selector := range result.UnassignedPanes {
				if _, ok := expectedUnassigned[selector]; !ok {
					t.Fatalf("[E2E-BULK-ASSIGN] skip format %s unexpected unassigned selector %q in %v", tc.name, selector, result.UnassignedPanes)
				}
				delete(expectedUnassigned, selector)
			}
			if len(expectedUnassigned) != 0 {
				t.Fatalf("[E2E-BULK-ASSIGN] skip format %s omitted unassigned selectors %v", tc.name, expectedUnassigned)
			}
		})
	}
}

func TestE2E_RobotBulkAssign_AllocationPaneTypes(t *testing.T) {
	CommonE2EPrerequisites(t)

	fixture := newRobotProcessFixture(t, "bulk-pane-types")
	panes := prepareBulkAssignAgentPanes(t, fixture, 2)
	beads := prepareBulkAssignBeads(t, fixture, 2)
	allocation := make(map[string]string, len(panes))
	expected := make(map[string]tmux.Pane, len(panes))
	for index := range panes {
		selector := strconv.Itoa(panes[index].Index)
		allocation[selector] = beads[index]
		expected[beads[index]] = panes[index]
	}
	encoded, err := json.Marshal(allocation)
	if err != nil {
		t.Fatalf("[E2E-BULK-ASSIGN] marshal pane-type allocation: %v", err)
	}
	result, process := runBulkAssignFixture(t, fixture, "--allocation="+string(encoded), "--dry-run")
	if process.exitCode != 0 || !result.Success || result.ErrorCode != "" || !result.DryRun ||
		result.AllocationSource != "explicit" || result.Summary.TotalPanes != 2 ||
		result.Summary.Failed != 0 || len(result.Assignments) != 2 {
		t.Fatalf("[E2E-BULK-ASSIGN] pane-type envelope: exit=%d result=%+v", process.exitCode, result)
	}
	for _, assignment := range result.Assignments {
		if _, err := tmux.ParsePaneSelector(assignment.Pane); err != nil {
			t.Fatalf("[E2E-BULK-ASSIGN] invalid canonical pane selector %q: %v", assignment.Pane, err)
		}
		pane, ok := expected[assignment.Bead]
		if !ok || assignment.Pane != strconv.Itoa(pane.Index) || assignment.PaneID != pane.ID ||
			assignment.Status != "planned" || assignment.PromptSent || assignment.Claimed || assignment.Error != "" {
			t.Fatalf("[E2E-BULK-ASSIGN] pane-type assignment=%+v", assignment)
		}
		delete(expected, assignment.Bead)
	}
	if len(expected) != 0 {
		t.Fatalf("[E2E-BULK-ASSIGN] pane-type allocation omitted mappings: %v", expected)
	}
}
