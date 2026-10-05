package cli

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/config"
	statuspkg "github.com/Dicklesworthstone/ntm/internal/status"
)

type compactionRecovererFunc func(context.Context, statuspkg.SessionObservation, string,
	func(context.Context, string) (statuspkg.SessionObservation, error)) []statuspkg.RecoveryAttempt

func (f compactionRecovererFunc) RecoverObserved(ctx context.Context, observation statuspkg.SessionObservation, project string,
	observe func(context.Context, string) (statuspkg.SessionObservation, error)) []statuspkg.RecoveryAttempt {
	return f(ctx, observation, project, observe)
}

func TestMonitorCompactionRecoveryHonorsEnabled(t *testing.T) {
	if monitorCompactionRecovery(nil) != nil {
		t.Fatal("missing policy enabled recovery")
	}
	cfg := config.Default()
	cfg.ContextRotation.Recovery.Enabled = false
	if monitorCompactionRecovery(cfg) != nil {
		t.Fatal("disabled policy enabled recovery")
	}
	cfg.ContextRotation.Recovery.Enabled = true
	cfg.ContextRotation.Recovery.CooldownSeconds = int(^uint(0) >> 1)
	cfg.ContextRotation.Recovery.MaxRecoveriesPerPane = 7
	cfg.ContextRotation.Recovery.Prompt = "Custom recovery reminder"
	cri := monitorCompactionRecovery(cfg)
	if cri == nil || cri.Detector() == nil || cri.Recovery() == nil {
		t.Fatal("enabled policy did not construct the shared integration")
	}
}

func TestMonitorRecoverySharesObservationAndOriginalReader(t *testing.T) {
	type executionKey struct{}
	ctx := context.WithValue(context.Background(), executionKey{}, "owner")
	original := statuspkg.SessionObservation{Session: "project", ObservedAt: time.Now(), Complete: true}
	reads, recoveries := 0, 0
	read := func(got context.Context, session string) (statuspkg.SessionObservation, error) {
		reads++
		if got.Value(executionKey{}) != "owner" || session != "project" {
			t.Fatal("observation lost the resident context or session")
		}
		if reads == 1 {
			deadline, ok := got.Deadline()
			if !ok || time.Until(deadline) > 10*time.Second {
				t.Fatal("monitor capture has no finite budget")
			}
		}
		return original, nil
	}
	recovery := compactionRecovererFunc(func(got context.Context, observation statuspkg.SessionObservation, project string,
		recheck func(context.Context, string) (statuspkg.SessionObservation, error)) []statuspkg.RecoveryAttempt {
		recoveries++
		if got != ctx || project != "/manifest/project" || !reflect.DeepEqual(observation, original) {
			t.Fatal("recovery lost the admitted observation or manifest project")
		}
		if _, err := recheck(got, observation.Session); err != nil {
			t.Fatal(err)
		}
		return nil
	})
	wrapped := observeMonitorRecovery(recovery, "/manifest/project", read)
	got, err := wrapped(ctx, "project")
	if err != nil || !reflect.DeepEqual(got, original) || reads != 2 || recoveries != 1 {
		t.Fatalf("reader recursed or timeline result changed: reads=%d recovery=%d output=%+v err=%v", reads, recoveries, got, err)
	}
}

func TestMonitorRecoveryInvalidatesFailedCaptureWithoutHidingTimelineError(t *testing.T) {
	for _, scenario := range []string{"error", "wrong session"} {
		t.Run(scenario, func(t *testing.T) {
			cause := errors.New("read failed")
			if scenario == "wrong session" {
				cause = nil
			}
			partial := statuspkg.SessionObservation{Session: "other", ObservedAt: time.Now()}
			calls := 0
			recovery := compactionRecovererFunc(func(_ context.Context, candidate statuspkg.SessionObservation, _ string,
				_ func(context.Context, string) (statuspkg.SessionObservation, error)) []statuspkg.RecoveryAttempt {
				calls++
				if candidate.Session != "project" || !candidate.ObservedAt.IsZero() || len(candidate.Panes) != 0 {
					t.Fatalf("invalid capture not invalidated: %+v", candidate)
				}
				return nil
			})
			wrapped := observeMonitorRecovery(recovery, "/project", func(context.Context, string) (statuspkg.SessionObservation, error) {
				return partial, cause
			})
			got, err := wrapped(context.Background(), "project")
			if !reflect.DeepEqual(got, partial) || err != cause || calls != 1 {
				t.Fatalf("timeline observation/error was overwritten: %+v %v calls=%d", got, err, calls)
			}
		})
	}
}

func TestMonitorRecoveryCancellationPreventsNewRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	recovery := compactionRecovererFunc(func(context.Context, statuspkg.SessionObservation, string,
		func(context.Context, string) (statuspkg.SessionObservation, error)) []statuspkg.RecoveryAttempt {
		calls++
		return nil
	})
	read := func(context.Context, string) (statuspkg.SessionObservation, error) {
		cancel()
		return statuspkg.SessionObservation{Session: "project", ObservedAt: time.Now()}, nil
	}
	wrapped := observeMonitorRecovery(recovery, "/project", read)
	if _, err := wrapped(ctx, "project"); !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("cancelled capture started recovery: err=%v calls=%d", err, calls)
	}
	if _, err := wrapped(ctx, "project"); !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatal("cancelled owner was resumed")
	}
	// Disabling the feature preserves the original reader, not a second loop.
	wrapped = monitorRecoveryObserver(nil, "/project", read)
	if reflect.ValueOf(wrapped).Pointer() != reflect.ValueOf(read).Pointer() {
		t.Fatal("disabled recovery still wraps the reader")
	}
}

func TestMonitorRecoveryLoopJoinsInFlightRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, finished := make(chan struct{}), make(chan struct{})
	recovery := compactionRecovererFunc(func(ctx context.Context, _ statuspkg.SessionObservation, _ string,
		_ func(context.Context, string) (statuspkg.SessionObservation, error)) []statuspkg.RecoveryAttempt {
		close(started)
		<-ctx.Done()
		return nil
	})
	read := func(context.Context, string) (statuspkg.SessionObservation, error) {
		return statuspkg.SessionObservation{Session: "project", ObservedAt: time.Now()}, nil
	}
	go func() {
		defer close(finished)
		recordSessionTimeline(ctx, "project", time.Hour, observeMonitorRecovery(recovery, "/project", read))
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("resident observation loop never started recovery")
	}
	select {
	case <-finished:
		t.Fatal("resident loop detached an unfinished recovery")
	default:
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("resident observation loop ignored owner cancellation")
	}
}
