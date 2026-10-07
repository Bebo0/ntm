package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/robot"
	"github.com/Dicklesworthstone/ntm/tests/testutil"
)

// TestRobotInterruptOpIDRetryAcrossProcessesReplays drives the user surface
// end to end: every invocation is a separate `ntm` process (the
// TestRobotProcessContractHelper re-exec) sharing one isolated state DB, the
// way an orchestrator retries after a timeout.
//
//   - `--robot-interrupt=S --op-id=ID --msg=TASK` interrupts once and delivers
//     the task once;
//   - the identical retry from a NEW process replays the recorded outcome
//     with no second Ctrl+C and no second delivery;
//   - --robot-send-receipt=ID reports the interrupt outcome;
//   - reusing ID with a different task fails IDEMPOTENCY_CONFLICT untouched;
//   - --robot-send-receipt combined with --robot-interrupt is refused rather
//     than silently skipping the interrupt.
//
// Ground truth is testutil.InterruptFixture's log of received SIGINTs and
// submitted lines.
func TestRobotInterruptOpIDRetryAcrossProcessesReplays(t *testing.T) {
	testutil.RequireTmuxThrottled(t)

	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	configHome := filepath.Join(tmpDir, "xdg")
	for _, dir := range []string{homeDir, configHome} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create isolated directory: %v", err)
		}
	}
	fx := testutil.StartInterruptFixture(t, "cliopid")
	marker := fmt.Sprintf("ntm-cli-opid-%d", time.Now().UnixNano())
	opID := "op-cli-" + marker

	run := func(args ...string) (map[string]any, int) {
		t.Helper()
		rawArgs, err := json.Marshal(args)
		if err != nil {
			t.Fatalf("encode helper args: %v", err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestRobotProcessContractHelper$")
		cmd.Dir = tmpDir
		cmd.Env = envWithOverrides(os.Environ(),
			"HOME="+homeDir,
			"XDG_CONFIG_HOME="+configHome,
			"NTM_NO_COLOR=1",
			"NTM_CONFIG=",
			"NTM_ROBOT_FORMAT=",
			"NTM_OUTPUT_FORMAT=",
			"TOON_DEFAULT_FORMAT=",
			"NTM_ROBOT_VERBOSITY=",
			// Keep the child on this process's isolated tmux server, where
			// the fixture session lives.
			"NTM_TEST_TMUX_ENV_OWNED=1",
			"NTM_ROBOT_CONTRACT_ARGS="+string(rawArgs),
		)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		exitCode := 0
		if err := cmd.Run(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("ntm %v failed without an exit status: %v", args, err)
			}
			exitCode = exitErr.ExitCode()
		}
		var payload map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
			t.Fatalf("ntm %v stdout is not one JSON document: %v\nstdout=%q\nstderr=%q", args, err, stdout.String(), stderr.String())
		}
		return payload, exitCode
	}
	interrupt := func(task string) []string {
		return []string{
			"--robot-interrupt=" + fx.Session, "--panes=" + fx.PaneID,
			"--force", "--no-wait", "--msg=" + task, "--op-id=" + opID,
		}
	}
	operationOf := func(payload map[string]any) map[string]any {
		op, _ := payload["operation"].(map[string]any)
		return op
	}

	first, code := run(interrupt("new task " + marker)...)
	if code != 0 || first["success"] != true {
		t.Fatalf("first interrupt exit=%d payload=%v, want success", code, first)
	}
	if op := operationOf(first); op == nil || op["kind"] != "interrupt" || op["status"] != "completed" || op["replayed"] == true {
		t.Fatalf("first operation = %v, want a fresh completed interrupt operation", op)
	}
	fx.WaitForEvents(t, marker, 1, 1)

	retry, code := run(interrupt("new task " + marker)...)
	fx.AssertQuiet(t, marker, 1, 1)
	if code != 0 || retry["success"] != true {
		t.Fatalf("retry exit=%d payload=%v, want replayed success", code, retry)
	}
	if op := operationOf(retry); op == nil || op["replayed"] != true || op["operation_id"] != opID {
		t.Fatalf("retry operation = %v, want the replayed %s", op, opID)
	}
	if retry["interrupted_at"] != first["interrupted_at"] {
		t.Fatalf("retry interrupted_at = %v, want the recorded %v", retry["interrupted_at"], first["interrupted_at"])
	}

	receipt, code := run("--robot-send-receipt=" + opID)
	if code != 0 || receipt["success"] != true || receipt["session"] != fx.Session {
		t.Fatalf("receipt exit=%d payload=%v, want the %s receipt", code, receipt, fx.Session)
	}
	if op := operationOf(receipt); op == nil || op["kind"] != "interrupt" || op["status"] != "completed" {
		t.Fatalf("receipt operation = %v, want completed interrupt", op)
	}
	outcome, _ := receipt["interrupt_outcome"].(map[string]any)
	if outcome == nil || outcome["success"] != true || outcome["message_sent"] != true {
		t.Fatalf("receipt interrupt_outcome = %v, want the recorded successful interrupt with the task delivered", receipt["interrupt_outcome"])
	}

	conflict, code := run(interrupt("different task " + marker)...)
	if code != 1 || conflict["error_code"] != robot.ErrCodeIdempotencyConflict {
		t.Fatalf("conflicting reuse exit=%d payload=%v, want IDEMPOTENCY_CONFLICT", code, conflict)
	}

	combined, code := run("--robot-send-receipt="+opID, "--robot-interrupt="+fx.Session, "--force")
	if code != 1 || combined["error_code"] != robot.ErrCodeInvalidFlag ||
		!strings.Contains(fmt.Sprint(combined["error"]), "--robot-interrupt") {
		t.Fatalf("receipt+interrupt exit=%d payload=%v, want INVALID_FLAG naming --robot-interrupt", code, combined)
	}
	fx.AssertQuiet(t, marker, 1, 1)
}
