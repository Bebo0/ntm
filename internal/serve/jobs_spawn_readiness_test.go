package serve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/ntm/internal/robot"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

func TestSwarmJobLaunchReadinessHTTP(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout string
		field   string
		dryRun  bool
		fail    bool
	}{
		{name: "gated preview", timeout: "1m", dryRun: true},
		{name: "gated failure", timeout: "45s", fail: true},
		{name: "case folded key", timeout: "45s", field: "LAUNCH_READY_TIMEOUT"},
		{name: "omitted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewHermeticServer("test")
			defer srv.Stop()
			options := make(chan robot.SpawnOptions, 1)
			srv.spawnAgents = func(_ context.Context, opts robot.SpawnOptions) (*robot.SpawnOutput, error) {
				options <- opts
				out := &robot.SpawnOutput{
					Session: opts.Session, DryRun: opts.DryRun,
					Agents: []robot.SpawnedAgent{{Pane: "%42", Type: "claude"}},
				}
				out.Success = !tc.fail
				if tc.fail {
					return out, errors.New("first agent could not initialize")
				}
				return out, nil
			}
			extra := ""
			if tc.timeout != "" {
				field := tc.field
				if field == "" {
					field = "launch_ready_timeout"
				}
				extra = fmt.Sprintf(`,%q:%q`, field, tc.timeout)
			}
			env := postJob(t, srv, fmt.Sprintf(`{"type":"swarm_spawn","params":{"session":"recoverable","cc_count":2,"dry_run":%t%s}}`, tc.dryRun, extra))
			final := pollJobTerminal(t, srv, env.Job.ID)
			wantStatus := JobStatusCompleted
			if tc.fail {
				wantStatus = JobStatusFailed
				if !strings.Contains(final.Job.Error, "could not initialize") {
					t.Fatalf("lost initialization failure: %+v", final.Job)
				}
				assertSpawnJobRecovery(t, final)
			}
			if final.Job.Status != string(wantStatus) {
				t.Fatalf("readiness job status = %s, want %s: %+v", final.Job.Status, wantStatus, final.Job)
			}
			if tc.timeout == "" {
				if _, exists := final.Job.Result["launch_ready_timeout"]; exists {
					t.Fatal("unrequested readiness gate appeared in the result")
				}
			} else {
				want := tc.timeout
				if want == "1m" {
					want = "1m0s"
				}
				if final.Job.Result["launch_ready_timeout"] != want {
					t.Fatalf("lost effective readiness budget: %+v", final.Job.Result)
				}
			}
			select {
			case opts := <-options:
				gated := tc.timeout != ""
				if opts.WaitReady != gated || (opts.LifecycleDeps != nil) != gated || opts.DryRun != tc.dryRun || opts.CCCount != 2 {
					t.Fatalf("readiness control did not reach the spawn service: %+v", opts)
				}
				if gated {
					// The real installed gate must honor cancellation without
					// entering its production tmux launcher.
					ctx, cancel := context.WithCancel(context.Background())
					cancel()
					if _, err := opts.LifecycleDeps.LaunchAgent(ctx, tmux.Pane{}, opts.Session, "claude", 1, "", ""); !errors.Is(err, context.Canceled) {
						t.Fatalf("installed readiness gate ignored cancellation: %v", err)
					}
				}
			default:
				t.Fatal("job finished without entering the spawn service")
			}
		})
	}
}

func TestSwarmJobLaunchReadinessRejectsInvalidInputHTTP(t *testing.T) {
	for _, value := range []string{`"0s"`, `"-1s"`, `"invalid"`, `"999999999999999999h"`, `""`, `null`, `10`, `true`, `{}`} {
		t.Run(value, func(t *testing.T) {
			srv := NewHermeticServer("test")
			defer srv.Stop()
			called := make(chan struct{}, 1)
			srv.spawnAgents = func(context.Context, robot.SpawnOptions) (*robot.SpawnOutput, error) {
				called <- struct{}{}
				return nil, errors.New("unexpected mutation")
			}
			env := postJob(t, srv, fmt.Sprintf(`{"type":"swarm_spawn","params":{"session":"guarded","cc_count":2,"launch_ready_timeout":%s}}`, value))
			final := pollJobTerminal(t, srv, env.Job.ID)
			if final.Job.Status != string(JobStatusFailed) || !strings.Contains(final.Job.Error, "launch_ready_timeout") {
				t.Fatalf("invalid readiness budget was not diagnosed: %+v", final.Job)
			}
			select {
			case <-called:
				t.Fatal("invalid readiness gate reached the mutating spawn service")
			default:
			}
		})
	}
}

func TestSpawnJobReadinessProgressRetainsEarlierAgents(t *testing.T) {
	var snapshots []map[string]interface{}
	ctx := context.WithValue(context.Background(), jobProgressContextKey{}, jobProgressReporter(func(result map[string]interface{}) error {
		snapshots = append(snapshots, result)
		return nil
	}))
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	observe := spawnJobProgressObserver(ctx, cancel)
	for i := 1; i <= 3; i++ {
		agent := robot.SpawnedAgent{Pane: fmt.Sprintf("0.%d", i), Type: "claude"}
		if err := observe(robot.SpawnProgress{Stage: "launch_agent", Phase: "finished", Session: "gated", PaneID: fmt.Sprintf("%%%d", i), Agent: &agent}); err != nil {
			t.Fatal(err)
		}
		agent.Ready = true
		if err := observe(robot.SpawnProgress{Stage: "wait_ready", Phase: "finished", Session: "gated", Agents: []robot.SpawnedAgent{agent}}); err != nil {
			t.Fatal(err)
		}
		rows := snapshots[len(snapshots)-1]["agents"].([]interface{})
		if len(rows) != i {
			t.Fatalf("readiness of agent %d lost earlier launched processes: %+v", i, rows)
		}
		for index, row := range rows {
			got := row.(map[string]interface{})
			if got["pane"] != fmt.Sprintf("0.%d", index+1) || got["ready"] != true {
				t.Fatalf("readiness progress lost order/state: %+v", rows)
			}
		}
		// Each saved callback value is an immutable recovery observation.
		prior := snapshots[len(snapshots)-2]["agents"].([]interface{})
		if prior[i-1].(map[string]interface{})["ready"] != false {
			t.Fatal("later readiness changed the previously recorded launch")
		}
	}
	progress := snapshots[len(snapshots)-1]["spawn_progress"].(map[string]interface{})
	if len(progress["agent_pane_ids"].(map[string]interface{})) != 3 || len(progress["observed_agents"].([]interface{})) != 3 {
		t.Fatalf("readiness lost durable recovery identities: %+v", progress)
	}
}
