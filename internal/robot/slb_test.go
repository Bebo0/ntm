package robot

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/ntm/internal/tools"
)

func fakeToolsPath(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}

	for dir := wd; dir != "/"; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			fakePath := filepath.Join(dir, "testdata", "faketools")
			if _, err := os.Stat(fakePath); err == nil {
				return fakePath
			}
			break
		}
	}

	return ""
}

// withFakeTools prepends the fake tools to PATH for the rest of the test.
// t.Setenv keeps the restore ordered with any later t.Setenv in the test.
func withFakeTools(t *testing.T) {
	t.Helper()

	fakePath := fakeToolsPath(t)
	if fakePath == "" {
		t.Skip("testdata/faketools not found")
	}

	t.Setenv("PATH", fakePath+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// slbTestProject initializes an slb project in a temp working directory and
// starts two agent sessions in it with the installed slb.
func slbTestProject(t *testing.T) (requester, reviewer tools.SLBSession) {
	t.Helper()
	if _, err := exec.LookPath("slb"); err != nil {
		t.Skip("slb not installed")
	}
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())

	run := func(args ...string) []byte {
		t.Helper()
		out, err := exec.Command("slb", args...).Output()
		if err != nil {
			var stderr []byte
			if exitErr, ok := err.(*exec.ExitError); ok {
				stderr = exitErr.Stderr
			}
			t.Fatalf("slb %s: %v\n%s", strings.Join(args, " "), err, stderr)
		}
		return out
	}
	run("init", "--json")
	start := func(agent string) tools.SLBSession {
		t.Helper()
		var s struct {
			SessionID  string `json:"session_id"`
			SessionKey string `json:"session_key"`
		}
		if err := json.Unmarshal(run("session", "start", "--agent", agent, "--program", "ntm-test", "--model", "test", "--json"), &s); err != nil || s.SessionID == "" {
			t.Fatalf("slb session start for %s: %v", agent, err)
		}
		return tools.SLBSession{ID: s.SessionID, Key: s.SessionKey}
	}
	return start("requester"), start("reviewer")
}

func TestRobotSLBReviewsThroughInstalledSLB(t *testing.T) {
	requester, reviewer := slbTestProject(t)
	adapter := tools.NewSLBAdapter()
	file := func(command string) string {
		t.Helper()
		raw, err := adapter.Request(context.Background(), requester, command, "clean up test output")
		var created struct {
			RequestID string `json:"request_id"`
		}
		if err != nil || json.Unmarshal(raw, &created) != nil || created.RequestID == "" {
			t.Fatalf("Request(%q) = %s, %v", command, raw, err)
		}
		return created.RequestID
	}
	approveID := file("rm -rf ./build")
	rejectID := file("git push --force")

	pending, err := GetSLBPending()
	if err != nil || !pending.Success || pending.Count != 2 {
		t.Fatalf("GetSLBPending = success:%t count:%d err:%q (%v)", pending.Success, pending.Count, pending.Error, err)
	}

	review := func(s tools.SLBSession) {
		t.Setenv("SLB_SESSION_ID", s.ID)
		t.Setenv("SLB_SESSION_KEY", s.Key)
	}

	review(tools.SLBSession{})
	if out, _ := GetSLBApprove(approveID); out.Success || out.ErrorCode != ErrCodePermissionDenied {
		t.Fatalf("approve without a reviewer session = success:%t code:%q err:%q, want PERMISSION_DENIED", out.Success, out.ErrorCode, out.Error)
	}

	review(requester)
	selfReview, _ := GetSLBApprove(approveID)
	if selfReview.Success || selfReview.ErrorCode != ErrCodePermissionDenied {
		t.Fatalf("self-review = success:%t code:%q err:%q, want PERMISSION_DENIED", selfReview.Success, selfReview.ErrorCode, selfReview.Error)
	}
	if strings.Contains(selfReview.Error, requester.Key) {
		t.Fatalf("robot error leaks the session key: %q", selfReview.Error)
	}

	review(reviewer)
	var decision struct {
		Decision  string `json:"decision"`
		NewStatus string `json:"new_request_status"`
	}
	approved, err := GetSLBApprove(approveID)
	if err != nil || !approved.Success || json.Unmarshal(approved.Result, &decision) != nil ||
		decision.Decision != "approve" || decision.NewStatus != "approved" {
		t.Fatalf("GetSLBApprove = success:%t result:%s err:%q (%v)", approved.Success, approved.Result, approved.Error, err)
	}

	rejected, err := GetSLBDeny(rejectID, "too risky")
	if err != nil || !rejected.Success || json.Unmarshal(rejected.Result, &decision) != nil ||
		decision.Decision != "reject" || decision.NewStatus != "rejected" {
		t.Fatalf("GetSLBDeny = success:%t result:%s err:%q (%v)", rejected.Success, rejected.Result, rejected.Error, err)
	}

	if out, _ := GetSLBApprove("00000000-0000-0000-0000-000000000000"); out.Success || out.ErrorCode != ErrCodeNotFound {
		t.Fatalf("approve unknown request = success:%t code:%q err:%q, want NOT_FOUND", out.Success, out.ErrorCode, out.Error)
	}
}

func TestGetSLBDenyRequiresReason(t *testing.T) {
	output, err := GetSLBDeny("req-123", "  ")
	if err != nil {
		t.Fatalf("GetSLBDeny error: %v", err)
	}
	if output.Success || output.ErrorCode != ErrCodeInvalidFlag {
		t.Fatalf("GetSLBDeny without reason = success:%t code:%q, want INVALID_FLAG", output.Success, output.ErrorCode)
	}
}

func TestGetSLBApproveMissingID(t *testing.T) {
	output, err := GetSLBApprove("")
	if err != nil {
		t.Fatalf("GetSLBApprove error: %v", err)
	}
	if output.Success {
		t.Fatalf("expected failure for missing ID")
	}
	if output.ErrorCode != ErrCodeInvalidFlag {
		t.Fatalf("error_code=%q, want %q", output.ErrorCode, ErrCodeInvalidFlag)
	}
}
