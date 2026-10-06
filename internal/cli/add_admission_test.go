package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/config"
	"github.com/Dicklesworthstone/ntm/internal/pressure"
	"github.com/Dicklesworthstone/ntm/internal/robot"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
	"github.com/Dicklesworthstone/ntm/tests/testutil"
)

func TestAddAdmissionCountsBeforeFlatten(t *testing.T) {
	specs := AgentSpecs{
		{Type: AgentTypeClaude, Count: 2, Model: "reviewer"},
		{Type: AgentTypeClaude, Count: 1, Model: "other-model"},
		{Type: AgentTypeCursor, Count: 4},
		{Type: AgentType("custom-agent"), Count: 3},
		{Type: AgentTypeCodex, Count: 0},
	}
	original := append(AgentSpecs(nil), specs...)
	counts, total, err := addAdmissionCounts(specs)
	if err != nil || total != 10 || !reflect.DeepEqual(counts, map[string]int{"cc": 3, "cursor": 4, "custom-agent": 3}) {
		t.Fatalf("counts = %v total=%d err=%v", counts, total, err)
	}
	if !reflect.DeepEqual(specs, original) {
		t.Fatal("counting mutated launch specifications")
	}
	maxInt := int(^uint(0) >> 1)
	for _, specs := range []AgentSpecs{
		{{Type: AgentTypeClaude, Count: -1}},
		{{Type: AgentTypeClaude, Count: maxInt}, {Type: AgentTypeCodex, Count: 1}},
	} {
		if counts, total, err := addAdmissionCounts(specs); !errors.Is(err, errCLIInvalidInput) || counts != nil || total != 0 {
			t.Fatalf("unsafe count accepted: %v %d %v", counts, total, err)
		}
	}
}

func TestAddAdmissionErrorRetainsDecisionAndCancellation(t *testing.T) {
	decision := &pressure.SpawnAdmission{
		Decision: pressure.SpawnAdmissionRefuse, Reason: "agent_type_limit_exceeded", Serialized: true,
	}
	refusal := &robot.SpawnAdmissionError{
		ErrorCode: robot.ErrCodeResourceBusy, Admission: decision, Err: errors.New("configured Claude limit exceeded"),
	}
	for _, cancelled := range []bool{false, true} {
		var cause error = fmt.Errorf("add failed: %w", refusal)
		want := robot.ErrCodeResourceBusy
		if cancelled {
			cause = errors.Join(cause, context.Canceled)
			want = robot.ErrCodeTimeout
		}
		result := newAgentLifecycleFailureResponse(cause, "project", false, true, nil, nil)
		if result.Success || result.PartialMutation || result.Admission != decision || result.ErrorCode != want || result.Code != want {
			t.Fatalf("refusal lost code or decision: %+v", result)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		var decoded agentLifecycleFailureResponse
		if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.Admission == nil || !decoded.Admission.Serialized || decoded.AffectedPaneIDs == nil {
			t.Fatalf("JSON lost admission or checked-empty array: %s (%v)", encoded, err)
		}
	}
}

// Native surface regression: the actual add command must refuse a full fleet
// before splitting a pane. The existing pane is deliberately adopted as Claude;
// no provider CLI or external account is needed to test count admission.
func TestAddCommandRejectsCappedFleetBeforeSplitting(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("cross-process admission locking requires Linux or macOS")
	}
	testutil.RequireTmuxThrottled(t)
	t.Setenv("NTM_CONFIG", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	project := t.TempDir()
	session := fmt.Sprintf("ntm-test-add-cap-%d", time.Now().UnixNano())
	if err := tmux.CreateSession(session, project); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tmux.KillSession(session) })
	panes, err := tmux.GetPanes(session)
	if err != nil || len(panes) != 1 {
		t.Fatalf("initial panes: %+v %v", panes, err)
	}
	if err := tmux.SetPaneAgentIdentityContext(t.Context(), panes[0].ID, session+"__cc_1", tmux.AgentClaude); err != nil {
		t.Fatal(err)
	}
	previousCfg, previousJSON := cfg, jsonOutput
	t.Cleanup(func() { cfg, jsonOutput = previousCfg, previousJSON })
	cfg = newTmuxIntegrationTestConfig(project)
	cfg.SpawnPacing.Enabled = true
	cfg.SpawnPacing.MaxConcurrentSpawns = 1000
	cfg.SpawnPacing.AgentCaps = config.AgentPacingConfig{}
	cfg.SpawnPacing.AgentTypeLimits = map[string]int{"claude": 1}
	cfg.Checkpoints.Enabled = false
	cfg.CASS.Context.Enabled = false
	jsonOutput = true

	stdout, callErr := captureAddAdmissionOutput(t, func() error {
		cmd := newAddCmd()
		cmd.SetArgs([]string{session, "--cc=1", "--no-cass-context"})
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		return cmd.ExecuteContext(t.Context())
	})
	var refusal *robot.SpawnAdmissionError
	if !errors.As(callErr, &refusal) {
		t.Fatalf("command did not return an admission refusal: %v output=%s", callErr, stdout)
	}
	var result agentLifecycleFailureResponse
	if err := json.Unmarshal([]byte(stdout), &result); err != nil || result.ErrorCode != robot.ErrCodeResourceBusy || result.PartialMutation || result.Admission == nil || !result.Admission.Serialized {
		t.Fatalf("command admission response: %s (%v)", stdout, err)
	}
	live, err := tmux.GetPanes(session)
	if err != nil || len(live) != 1 || live[0].ID != panes[0].ID {
		t.Fatalf("refused add changed topology: %+v (%v)", live, err)
	}

	// Scale composes this same engine without nested terminal JSON. Its
	// outcome must not retain an earlier successful add count on refusal.
	outcome := AddOutcome{Added: 99}
	stdout, callErr = captureAddAdmissionOutput(t, func() error {
		return executeAdd(t.Context(), AddOptions{
			Session: session, Agents: AgentSpecs{{Type: AgentTypeClaude, Count: 1}},
			NoCassContext: true, Outcome: &outcome,
		}, false)
	})
	if callErr == nil || stdout != "" || outcome.Added != 0 || outcome.Admission == nil || outcome.Admission.Reason != "agent_type_limit_exceeded" {
		t.Fatalf("composed refusal lost outcome or printed nested JSON: %+v %v %q", outcome, callErr, stdout)
	}
}

func captureAddAdmissionOutput(t *testing.T, call func() error) (string, error) {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	defer func() { os.Stdout = original }()
	os.Stdout = writer
	read := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(reader)
		read <- string(data)
	}()
	callErr := call()
	_ = writer.Close()
	os.Stdout = original
	return <-read, callErr
}
