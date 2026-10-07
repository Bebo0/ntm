package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecuteCommandRetryPolicyRetriesExitCodeFailure(t *testing.T) {
	executor := newCommandTestExecutor(t)
	attemptsPath := filepath.Join(executor.config.ProjectDir, "retry-attempts.txt")
	workflow := &Workflow{
		Name:     "command-retry",
		Settings: DefaultWorkflowSettings(),
		Steps: []Step{{
			ID:         "retry_exit",
			Command:    `printf 'attempt\n' >> retry-attempts.txt; exit 7`,
			OnError:    ErrorActionRetry,
			RetryCount: 3,
			RetryDelay: Duration{Duration: time.Millisecond},
		}},
	}
	executor.graph = NewDependencyGraph(workflow)

	result := executor.executeStep(context.Background(), &workflow.Steps[0], workflow)
	if result.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", result.Status, StatusFailed)
	}
	if result.Attempts != 4 {
		t.Fatalf("attempts = %d, want initial attempt plus 3 retries", result.Attempts)
	}
	if result.Error == nil || result.Error.Type != "exit" {
		t.Fatalf("error = %#v, want exit error", result.Error)
	}
	if !strings.Contains(result.Error.Details, "exit_code=7") {
		t.Fatalf("error details = %q, want exit_code=7", result.Error.Details)
	}

	content, err := os.ReadFile(attemptsPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) returned error: %v", attemptsPath, err)
	}
	if got := strings.Count(string(content), "attempt\n"); got != 4 {
		t.Fatalf("recorded attempts = %d, want 4; content = %q", got, content)
	}
}

func TestExecuteCommandContextCancellationReturnsCancelled(t *testing.T) {
	executor := newCommandTestExecutor(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	step := &Step{
		ID:      "cancel_command",
		Command: "sleep 60",
		Timeout: Duration{Duration: 30 * time.Second},
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	result := executor.executeCommand(ctx, step, &Workflow{Name: "command-cancel"})
	if result.Status != StatusCancelled {
		t.Fatalf("status = %q, want %q; error = %#v", result.Status, StatusCancelled, result.Error)
	}
	if result.Error == nil || result.Error.Type != "cancelled" {
		t.Fatalf("error = %#v, want cancelled error", result.Error)
	}
}

func TestCommandReceiptRecoversResultBeforeSchedulerCheckpoint(t *testing.T) {
	e := newCommandTestExecutor(t)
	workflow := &Workflow{
		SchemaVersion: SchemaVersion, Name: e.state.WorkflowID, Settings: DefaultWorkflowSettings(),
		Steps: []Step{
			{ID: "produce", Command: `printf 'once\n' >> effects; printf '{"answer":42}'`, OutputVar: "produced", OutputParse: OutputParse{Type: "json"}},
			{ID: "consume", DependsOn: []string{"produce"}, Command: `printf '%s' '${steps.produce.data.answer}' > dependent`},
		},
	}
	e.state.Status, e.state.Session = StatusRunning, e.config.Session
	e.state.InFlightSteps = map[string]InFlightStepState{"produce": {StepID: "produce", Kind: StepKindCommand, StartedAt: time.Now()}}
	result := e.executeCommand(context.Background(), &workflow.Steps[0], workflow)
	if result.Status != StatusCompleted {
		t.Fatalf("command did not complete: %+v", result)
	}
	// The command has returned but its caller has not recorded the StepResult.
	// Load precisely the checkpoint written by the command's settlement path.
	prior, err := LoadState(e.config.ProjectDir, e.state.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := prior.Steps["produce"]; exists {
		t.Fatal("fixture already has the scheduler checkpoint")
	}
	final, err := NewExecutor(e.config).Resume(context.Background(), workflow, prior, nil)
	if err != nil || final.Status != StatusCompleted {
		t.Fatalf("receipt recovery failed: %+v %v", final, err)
	}
	if data, err := os.ReadFile(filepath.Join(e.config.ProjectDir, "effects")); err != nil || string(data) != "once\n" {
		t.Fatalf("receipt recovery repeated the command: %q %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(e.config.ProjectDir, "dependent")); err != nil || string(data) != "42" {
		t.Fatalf("receipt recovery lost parsed output used by a dependent: %q %v", data, err)
	}
	if final.Variables["produced"] != `{"answer":42}` {
		t.Fatalf("receipt recovery lost the named output: %#v", final.Variables)
	}
}

func TestCommandLaunchRequiresDurableIntent(t *testing.T) {
	e := newCommandTestExecutor(t)
	blockCheckpointPath(t, e)
	step := Step{ID: "must-not-start", Command: "printf started > effects"}
	result := e.executeCommand(context.Background(), &step, &Workflow{Name: e.state.WorkflowID})
	if result.Status != StatusFailed || result.Error == nil || result.Error.Type != "checkpoint" || !errors.Is(e.checkpointFailure(), ErrCheckpointFailed) {
		t.Fatalf("command ignored an unwritable launch journal: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(e.config.ProjectDir, "effects")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("command ran without a durable intent: %v", err)
	}
}

func TestCommandResumeRejectsUnknownEvidenceBeforeExplicitReset(t *testing.T) {
	e := newCommandTestExecutor(t)
	step := Step{ID: "command", Command: "printf once >> effects"}
	workflow := &Workflow{Name: e.state.WorkflowID, Steps: []Step{step}}
	if result := e.executeCommand(context.Background(), &step, workflow); result.Status != StatusCompleted {
		t.Fatalf("command failed: %+v", result)
	}
	prior, err := LoadState(e.config.ProjectDir, e.state.RunID)
	if err != nil {
		t.Fatal(err)
	}
	record := prior.CommandExecutions[step.ID]
	record.Status = "future-unknown"
	prior.CommandExecutions[step.ID] = record
	if err := SaveState(e.config.ProjectDir, prior); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(pipelineStatePath(e.config.ProjectDir, prior.RunID))
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewExecutor(e.config).ResumeWithOptions(context.Background(), workflow, prior, ResumeOptions{Reset: true}, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown status") {
		t.Fatalf("explicit reset erased unrecognized evidence: %v", err)
	}
	after, err := os.ReadFile(pipelineStatePath(e.config.ProjectDir, prior.RunID))
	if err != nil || string(before) != string(after) {
		t.Fatal("rejected reset changed the saved command evidence")
	}
	if data, err := os.ReadFile(filepath.Join(e.config.ProjectDir, "effects")); err != nil || string(data) != "once" {
		t.Fatalf("rejected reset repeated work: %q %v", data, err)
	}
}

func TestCommandResumeProtectsLiveWaitNoneAfterSchedulerCompletion(t *testing.T) {
	e := newCommandTestExecutor(t)
	step := Step{ID: "sidecar", Command: "sleep 60", Wait: WaitNone}
	workflow := &Workflow{Name: e.state.WorkflowID, Steps: []Step{step}}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		e.backgroundCommandWG.Wait()
	}()
	result := e.executeCommand(ctx, &step, workflow)
	if result.Status != StatusCompleted {
		t.Fatalf("WaitNone did not dispatch: %+v", result)
	}
	e.state.Steps[step.ID] = result
	if err := e.persistState(); err != nil {
		t.Fatal(err)
	}
	prior, err := LoadState(e.config.ProjectDir, e.state.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(prior.InFlightSteps) != 0 || prior.Steps[step.ID].Status != StatusCompleted {
		t.Fatal("fixture has not reached synchronous WaitNone completion")
	}
	checker := NewExecutor(e.config)
	checker.state = prior
	if err := checker.applyResumeOptions(workflow, ResumeOptions{}); err == nil || !strings.Contains(err.Error(), "unresolved launch") {
		t.Fatalf("completed scheduler bookkeeping hid a live command: %v", err)
	}
	cancel()
	e.backgroundCommandWG.Wait()
	if err := checker.applyResumeOptions(workflow, ResumeOptions{Mode: ResumeModeRestartFailed}); err != nil {
		t.Fatalf("explicit replay was rejected after stopping the old work: %v", err)
	}
	if _, exists := checker.state.CommandExecutions[step.ID]; exists || !shouldRerunStep(checker.state.Steps[step.ID]) {
		t.Fatal("explicit replay retained the live receipt or skipped the completed WaitNone leaf")
	}
}

func TestCommandReplayCannotEraseWorkBehindCompletedContainer(t *testing.T) {
	for _, kind := range []string{"parallel", "foreach_rounds"} {
		t.Run(kind, func(t *testing.T) {
			e := newCommandTestExecutor(t)
			body := Step{ID: "sidecar", Command: "printf 'once\\n' >> effects; sleep 60", Wait: WaitNone}
			// A parallel group's return cancels its child contexts. Keep it
			// alive until the sidecar has actually produced its external effect.
			ready := Step{ID: "ready", Command: `expected=1; if [ -f allow ]; then expected=2; fi; while [ ! -f effects ] || [ "$(wc -l < effects)" -lt "$expected" ]; do sleep 0.01; done`}
			group := Step{ID: "group", Parallel: ParallelSpec{Steps: []Step{body, ready}}}
			perRun := 1
			if kind == "foreach_rounds" {
				perRun = 2
				group = Step{ID: "group", Foreach: &ForeachConfig{Items: "${vars.items}", MaxRounds: IntOrExpr{Value: 2}, Steps: []Step{body}}}
			}
			workflow := &Workflow{SchemaVersion: SchemaVersion, Name: e.state.WorkflowID, Settings: DefaultWorkflowSettings(), Steps: []Step{
				group,
				{ID: "gate", DependsOn: []string{"group"}, Command: fmt.Sprintf("while [ ! -f effects ] || [ \"$(wc -l < effects)\" -lt %d ]; do sleep 0.01; done; if [ ! -f allow ]; then exit 7; fi; while [ \"$(wc -l < effects)\" -lt %d ]; do sleep 0.01; done", perRun, 2*perRun)},
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			initial, err := NewExecutor(e.config).Run(ctx, workflow, map[string]interface{}{"items": []interface{}{"item"}}, nil)
			if err == nil || initial.Status != StatusFailed || initial.Steps["group"].Status != StatusCompleted {
				t.Fatalf("fixture did not complete the container before failing: %+v %v", initial, err)
			}
			prior, err := LoadState(e.config.ProjectDir, initial.RunID)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(pipelineStatePath(e.config.ProjectDir, initial.RunID))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NewExecutor(e.config).ResumeWithOptions(context.Background(), workflow, prior, ResumeOptions{Mode: ResumeModeRestartFailed}, nil); err == nil || !strings.Contains(err.Error(), "cannot replay command") {
				t.Fatalf("leaf-only replay silently skipped its completed container: %v", err)
			}
			after, err := os.ReadFile(pipelineStatePath(e.config.ProjectDir, initial.RunID))
			if err != nil || string(before) != string(after) {
				t.Fatal("rejected leaf replay erased command receipts or container progress")
			}
			if err := os.WriteFile(filepath.Join(e.config.ProjectDir, "allow"), []byte("allow"), 0600); err != nil {
				t.Fatal(err)
			}
			prior, err = LoadState(e.config.ProjectDir, initial.RunID)
			if err != nil {
				t.Fatal(err)
			}
			final, err := NewExecutor(e.config).ResumeWithOptions(ctx, workflow, prior, ResumeOptions{Reset: true}, nil)
			if err != nil || final.Status != StatusCompleted {
				t.Fatalf("explicit whole-run replay failed: %+v %v", final, err)
			}
			if data, err := os.ReadFile(filepath.Join(e.config.ProjectDir, "effects")); err != nil || strings.Count(string(data), "once\n") != 2*perRun {
				t.Fatalf("explicit replay did not repeat the nested work exactly once: %q %v", data, err)
			}
		})
	}
}

func TestCommandCleanupRunsForEveryResumedAttempt(t *testing.T) {
	for _, kind := range []string{"direct", "parallel", "foreach_rounds", "success_foreach_rounds"} {
		t.Run(kind, func(t *testing.T) {
			e := newCommandTestExecutor(t)
			cleanup := Step{ID: "release", Command: `printf 'released\n' >> cleanups`}
			perAttempt := 1
			switch kind {
			case "parallel":
				cleanup = Step{ID: "cleanup_group", Parallel: ParallelSpec{Steps: []Step{cleanup}}}
			case "foreach_rounds", "success_foreach_rounds":
				perAttempt = 2
				cleanup = Step{ID: "cleanup_group", Foreach: &ForeachConfig{Items: "${vars.resources}", MaxRounds: IntOrExpr{Value: 2}, Steps: []Step{cleanup}}}
				if kind == "success_foreach_rounds" {
					cleanup = Step{ID: "cleanup_start", Command: "true", OnSuccess: []Step{cleanup}}
				}
			}
			workflow := &Workflow{SchemaVersion: SchemaVersion, Name: e.state.WorkflowID, Settings: DefaultWorkflowSettings(),
				Steps: []Step{{ID: "work", Command: `printf 'acquired\n' >> attempts; sleep 60`}},
			}
			workflow.Settings.OnCancel = []Step{cleanup}
			workflow.Settings.OnCancelTimeout = Duration{Duration: 2 * time.Second}
			ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
			initial, err := NewExecutor(e.config).Run(ctx, workflow, map[string]interface{}{"resources": []interface{}{"resource"}}, nil)
			cancel()
			if err == nil || initial == nil || initial.Status != StatusCancelled {
				t.Fatalf("initial attempt did not cancel: %+v %v", initial, err)
			}
			prior, err := LoadState(e.config.ProjectDir, initial.RunID)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel = context.WithTimeout(context.Background(), 750*time.Millisecond)
			resumed, err := NewExecutor(e.config).Resume(ctx, workflow, prior, nil)
			cancel()
			if err == nil || resumed == nil || resumed.Status != StatusCancelled {
				t.Fatalf("resumed attempt did not cancel: %+v %v", resumed, err)
			}
			if data, err := os.ReadFile(filepath.Join(e.config.ProjectDir, "attempts")); err != nil || strings.Count(string(data), "acquired\n") != 2 {
				t.Fatalf("work did not reacquire its resource on resume: %q %v", data, err)
			}
			if data, err := os.ReadFile(filepath.Join(e.config.ProjectDir, "cleanups")); err != nil || strings.Count(string(data), "released\n") != 2*perAttempt {
				t.Fatalf("each cancelled attempt must run its cleanup: %q %v", data, err)
			}
		})
	}
}

func TestCommandResumePreservesUnresolvedCleanup(t *testing.T) {
	e := newCommandTestExecutor(t)
	cleanup := Step{ID: "release", Command: "sleep 60", Wait: WaitNone}
	workflow := &Workflow{Name: e.state.WorkflowID, Settings: DefaultWorkflowSettings(), Steps: []Step{{ID: "work", Command: "printf unsafe > effects"}}}
	workflow.Settings.OnCancel = []Step{cleanup}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		e.backgroundCommandWG.Wait()
	}()
	result := e.executeCommand(ctx, &cleanup, workflow)
	if result.Status != StatusCompleted {
		t.Fatalf("cleanup did not launch: %+v", result)
	}
	e.state.Steps[cleanup.ID] = result
	if err := e.persistState(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(pipelineStatePath(e.config.ProjectDir, e.state.RunID))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []ResumeMode{ResumeModeContinue, ResumeModeRestartFailed} {
		prior, err := LoadState(e.config.ProjectDir, e.state.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NewExecutor(e.config).ResumeWithOptions(context.Background(), workflow, prior, ResumeOptions{Mode: mode}, nil); err == nil {
			t.Fatalf("%s forgot an unresolved cleanup process", mode)
		}
		after, err := os.ReadFile(pipelineStatePath(e.config.ProjectDir, e.state.RunID))
		if err != nil || string(before) != string(after) {
			t.Fatalf("%s changed unresolved cleanup evidence", mode)
		}
	}
	if _, err := os.Stat(filepath.Join(e.config.ProjectDir, "effects")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resume dispatched while old cleanup was unresolved: %v", err)
	}
}

func TestCommandCleanupResetPreservesMainNamespaceCollision(t *testing.T) {
	e := newCommandTestExecutor(t)
	workflow := &Workflow{SchemaVersion: SchemaVersion, Name: e.state.WorkflowID, Settings: DefaultWorkflowSettings(), Steps: []Step{
		{ID: "cleanup_group_release", Command: `printf 'once\n' >> main_effects`},
		{ID: "gate", DependsOn: []string{"cleanup_group_release"}, Command: "test -f allow"},
	}}
	workflow.Settings.OnCancel = []Step{{ID: "cleanup_group", Parallel: ParallelSpec{Steps: []Step{
		{ID: "release", Command: "printf released > cleanup_effects"},
	}}}}
	if validation := Validate(workflow); !validation.Valid {
		t.Fatalf("fixture must use an accepted workflow namespace: %+v", validation.Errors)
	}
	initial, err := NewExecutor(e.config).Run(context.Background(), workflow, nil, nil)
	if err == nil || initial.Status != StatusFailed {
		t.Fatalf("fixture did not fail its gate: %+v %v", initial, err)
	}
	prior, err := LoadState(e.config.ProjectDir, initial.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.config.ProjectDir, "allow"), []byte("allow"), 0600); err != nil {
		t.Fatal(err)
	}
	resumed, err := NewExecutor(e.config).Resume(context.Background(), workflow, prior, nil)
	if err != nil || resumed.Status != StatusCompleted {
		t.Fatalf("resume failed: %+v %v", resumed, err)
	}
	if data, err := os.ReadFile(filepath.Join(e.config.ProjectDir, "main_effects")); err != nil || string(data) != "once\n" {
		t.Fatalf("cleanup namespace erased and repeated completed main work: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(e.config.ProjectDir, "cleanup_effects")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup unexpectedly ran without cancellation: %v", err)
	}
}
