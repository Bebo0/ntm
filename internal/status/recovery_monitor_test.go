package status

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/agent"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

const recoveryBaseline = "Stable project transcript anchor\nWorking on the requested changes\n"

func recoveryObservation(output string, state AgentState) SessionObservation {
	now := time.Now()
	return SessionObservation{Session: "project", ObservedAt: now, Complete: true, Panes: []PaneObservation{{
		Pane: tmux.PaneRef{ID: "%17", WindowIndex: 3, PaneIndex: 2}, AgentType: "claude",
		Metadata:  tmux.Pane{ID: "%17", WindowIndex: 3, Index: 2, PID: 123, Type: tmux.AgentClaude, Command: "claude"},
		RawOutput: output, Current: StateObservation{ObservedAt: now, Freshness: FreshnessFresh, Confidence: 1,
			Status: AgentStatus{State: state}},
	}}}
}

func monitoredRecovery(t *testing.T) (*CompactionRecoveryIntegration, *[]string) {
	t.Helper()
	cri := NewCompactionRecoveryIntegration(RecoveryConfig{Cooldown: time.Nanosecond, Prompt: "Read AGENTS.md", MaxRecoveries: 5})
	cri.recovery.verifyProcess = func(context.Context, string, string) error { return nil }
	var targets []string
	cri.recovery.sendPromptContext = func(ctx context.Context, target, prompt string, enter bool, kind agent.AgentType) error {
		if ctx.Err() != nil || target != "%17" || prompt != "Read AGENTS.md" || !enter || kind.Canonical() != agent.AgentTypeClaudeCode {
			t.Fatalf("invalid delivery: target=%q prompt=%q type=%s err=%v", target, prompt, kind, ctx.Err())
		}
		targets = append(targets, target)
		return nil
	}
	return cri, &targets
}

func idleRecoveryObservation(_ context.Context, session string) (SessionObservation, error) {
	result := recoveryObservation(recoveryBaseline+"Conversation compacted\n", StateIdle)
	result.Session = session
	return result, nil
}

func TestMonitoredRecoveryWaitsForIdleAndSendsOnceToStablePane(t *testing.T) {
	cri, targets := monitoredRecovery(t)
	ctx := context.Background()
	checks := 0
	observe := func(ctx context.Context, session string) (SessionObservation, error) {
		checks++
		return idleRecoveryObservation(ctx, session)
	}
	if got := cri.RecoverObserved(ctx, recoveryObservation(recoveryBaseline, StateWorking), "/project", observe); len(got) != 0 {
		t.Fatalf("baseline produced attempts: %+v", got)
	}
	banner := recoveryBaseline + "Conversation compacted\n"
	if got := cri.RecoverObserved(ctx, recoveryObservation(banner, StateWorking), "/project", observe); len(got) != 0 || checks != 0 {
		t.Fatalf("working pane got recovery: attempts=%v checks=%d", got, checks)
	}
	got := cri.RecoverObserved(ctx, recoveryObservation(banner, StateIdle), "/project", observe)
	if len(got) != 1 || !got[0].Sent || got[0].Error != "" || checks != 1 || !reflect.DeepEqual(*targets, []string{"%17"}) {
		t.Fatalf("recovery=%+v checks=%d targets=%v", got, checks, *targets)
	}
	for i := 0; i < 10; i++ {
		cri.RecoverObserved(ctx, recoveryObservation(banner+"❯ ", StateIdle), "/project", observe)
	}
	if len(*targets) != 1 || cri.recovery.GetRecoveryCount("%17") != 1 {
		t.Fatalf("stale banner repeated delivery: %v", *targets)
	}
	events := cri.recovery.GetRecoveryEvents()
	if len(events) != 1 || events[0].PaneID != "%17" || events[0].PaneIndex != 2 || events[0].Session != "project" {
		t.Fatalf("wrong physical recovery receipt: %+v", events)
	}
}

func TestMonitoredRecoveryRechecksIdleAfterEnrichment(t *testing.T) {
	cri, targets := monitoredRecovery(t)
	ctx := context.Background()
	cri.RecoverObserved(ctx, recoveryObservation(recoveryBaseline, StateIdle), "/project", idleRecoveryObservation)
	busy := func(context.Context, string) (SessionObservation, error) {
		return recoveryObservation(recoveryBaseline+"Conversation compacted\n", StateWorking), nil
	}
	banner := recoveryObservation(recoveryBaseline+"Conversation compacted\n", StateIdle)
	if got := cri.RecoverObserved(ctx, banner, "/project", busy); len(got) != 0 || len(*targets) != 0 {
		t.Fatalf("fresh working state ignored: %+v", got)
	}
	if cri.recovery.GetRecoveryCount("%17") != 0 {
		t.Fatal("preflight consumed recovery budget")
	}
	if got := cri.RecoverObserved(ctx, banner, "/project", idleRecoveryObservation); len(got) != 1 || !got[0].Sent {
		t.Fatalf("pending recovery was lost instead of deferred: %+v", got)
	}
}

func TestMonitoredRecoveryRejectsInvalidCurrentEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*SessionObservation)
	}{
		{"old session observation", func(o *SessionObservation) { o.ObservedAt = time.Now().Add(-time.Minute) }},
		{"future capture", func(o *SessionObservation) { o.Panes[0].Current.ObservedAt = time.Now().Add(time.Hour) }},
		{"stale", func(o *SessionObservation) { o.Panes[0].Current.Freshness = FreshnessStale }},
		{"capture error", func(o *SessionObservation) { o.Panes[0].Current.Error = "capture failed" }},
		{"rate limited", func(o *SessionObservation) { o.Panes[0].Current.Status.ErrorType = ErrorRateLimit }},
		{"low confidence", func(o *SessionObservation) { o.Panes[0].Current.Confidence = 0.5 }},
		{"dead", func(o *SessionObservation) { o.Panes[0].Metadata.Dead = true }},
		{"service", func(o *SessionObservation) { o.Panes[0].Metadata.Service = "cass" }},
		{"no pid", func(o *SessionObservation) { o.Panes[0].Metadata.PID = 0 }},
		{"wrong metadata id", func(o *SessionObservation) { o.Panes[0].Metadata.ID = "%18" }},
		{"wrong metadata type", func(o *SessionObservation) { o.Panes[0].Metadata.Type = tmux.AgentCodex }},
		{"duplicate id", func(o *SessionObservation) { o.Panes = append(o.Panes, o.Panes[0]) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cri, targets := monitoredRecovery(t)
			ctx := context.Background()
			cri.RecoverObserved(ctx, recoveryObservation(recoveryBaseline, StateIdle), "/project", idleRecoveryObservation)
			bad := recoveryObservation(recoveryBaseline+"Conversation compacted\n", StateIdle)
			tc.mutate(&bad)
			cri.RecoverObserved(ctx, bad, "/project", idleRecoveryObservation)
			// Successful capture after a gap only establishes a new baseline.
			cri.RecoverObserved(ctx, recoveryObservation(recoveryBaseline+"Conversation compacted\n", StateIdle), "/project", idleRecoveryObservation)
			if len(*targets) != 0 {
				t.Fatalf("untrusted observation authorized a send: %v", *targets)
			}
		})
	}
}

func TestMonitoredRecoveryRejectsChangedIdentityAtSend(t *testing.T) {
	for _, scenario := range []string{"pid", "type", "session", "missing", "capture failure", "bare shell"} {
		t.Run(scenario, func(t *testing.T) {
			cri, targets := monitoredRecovery(t)
			ctx := context.Background()
			cri.RecoverObserved(ctx, recoveryObservation(recoveryBaseline, StateIdle), "/project", idleRecoveryObservation)
			if scenario == "bare shell" {
				cri.recovery.verifyProcess = func(context.Context, string, string) error { return errors.New("no stable non-shell process") }
			}
			observe := func(context.Context, string) (SessionObservation, error) {
				o := recoveryObservation(recoveryBaseline+"Conversation compacted\n", StateIdle)
				switch scenario {
				case "pid":
					o.Panes[0].Metadata.PID++
				case "type":
					o.Panes[0].AgentType = "codex"
					o.Panes[0].Metadata.Type = tmux.AgentCodex
				case "session":
					o.Session = "other"
				case "missing":
					o.Panes = nil
				case "capture failure":
					return o, errors.New("read failed")
				}
				return o, nil
			}
			got := cri.RecoverObserved(ctx, recoveryObservation(recoveryBaseline+"Conversation compacted\n", StateIdle), "/project", observe)
			if len(got) != 1 || got[0].Sent || got[0].Error == "" || len(*targets) != 0 || cri.recovery.GetRecoveryCount("%17") != 0 {
				t.Fatalf("invalid final evidence authorized send: %+v targets=%v", got, *targets)
			}
		})
	}
}

func TestMonitoredRecoveryUncertainDeliveryIsNotRetried(t *testing.T) {
	cri, _ := monitoredRecovery(t)
	var sends int
	cri.recovery.sendPromptContext = func(context.Context, string, string, bool, agent.AgentType) error {
		sends++
		return errors.New("possibly delivered before failure")
	}
	ctx := context.Background()
	cri.RecoverObserved(ctx, recoveryObservation(recoveryBaseline, StateIdle), "/project", idleRecoveryObservation)
	banner := recoveryObservation(recoveryBaseline+"Conversation compacted\n", StateIdle)
	got := cri.RecoverObserved(ctx, banner, "/project", idleRecoveryObservation)
	if len(got) != 1 || got[0].Sent || got[0].Error == "" || cri.recovery.GetRecoveryCount("%17") != 1 {
		t.Fatalf("uncertain outcome not retained: %+v", got)
	}
	for i := 0; i < 10; i++ {
		cri.RecoverObserved(ctx, banner, "/project", idleRecoveryObservation)
	}
	if sends != 1 || len(cri.recovery.GetRecoveryEvents()) != 0 {
		t.Fatal("uncertain delivery retried or reported sent")
	}
}

func TestMonitoredRecoveryCancellationJoinsDelivery(t *testing.T) {
	cri, _ := monitoredRecovery(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cri.RecoverObserved(ctx, recoveryObservation(recoveryBaseline, StateIdle), "/project", idleRecoveryObservation)
	started, finished := make(chan struct{}), make(chan []RecoveryAttempt, 1)
	var active atomic.Int32
	cri.recovery.sendPromptContext = func(ctx context.Context, _ string, _ string, _ bool, _ agent.AgentType) error {
		active.Add(1)
		defer active.Add(-1)
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	go func() {
		finished <- cri.RecoverObserved(ctx, recoveryObservation(recoveryBaseline+"Conversation compacted\n", StateIdle), "/project", idleRecoveryObservation)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("delivery did not start")
	}
	select {
	case <-finished:
		t.Fatal("reported completion while delivery is blocked")
	default:
	}
	cancel()
	select {
	case got := <-finished:
		if active.Load() != 0 || len(got) != 1 || got[0].Sent || !strings.Contains(got[0].Error, "canceled") {
			t.Fatalf("unjoined cancellation: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("delivery ignored cancellation")
	}
	if got := cri.RecoverObserved(ctx, recoveryObservation(recoveryBaseline, StateIdle), "/project", idleRecoveryObservation); len(got) != 0 {
		t.Fatal("cancelled owner resumed")
	}
}

func TestRecoveryDeliveryReentryAndPreflightBudget(t *testing.T) {
	rm := NewRecoveryManager(RecoveryConfig{Cooldown: time.Nanosecond, MaxRecoveries: 2})
	started, release := make(chan struct{}), make(chan struct{})
	ctx := context.Background()
	var sent atomic.Int32
	rm.sendPromptContext = func(context.Context, string, string, bool, agent.AgentType) error { sent.Add(1); return nil }
	build := func(context.Context, string, bool) string { close(started); <-release; return "prompt" }
	done := make(chan struct{})
	go func() {
		defer close(done)
		rm.deliverRecovery(ctx, "project", 2, "%17", "banner", agent.AgentTypeClaudeCode, build, nil)
	}()
	<-started
	if ok, _ := rm.CanSendRecovery("%17"); ok {
		t.Fatal("in-flight construction not protected")
	}
	if ok, err := rm.deliverRecovery(ctx, "project", 2, "%17", "banner", agent.AgentTypeClaudeCode, build, nil); ok || err != nil {
		t.Fatal("reentry admitted")
	}
	close(release)
	<-done
	if sent.Load() != 1 {
		t.Fatal("duplicate send")
	}
	buildNow := func(context.Context, string, bool) string { return "prompt" }
	if ok, err := rm.deliverRecovery(ctx, "project", 2, "%17", "banner", agent.AgentTypeClaudeCode, buildNow,
		func(context.Context) error { return errors.New("not authorized") }); ok || err == nil {
		t.Fatal("failed preflight admitted")
	}
	if rm.GetRecoveryCount("%17") != 1 {
		t.Fatal("preflight failure consumed budget")
	}
}

func TestMonitoredRecoveryCooldownKeepsPendingAndGapDiscardsIt(t *testing.T) {
	cri, targets := monitoredRecovery(t)
	ctx := context.Background()
	cri.recovery.SetCooldown(time.Hour)
	cri.recovery.lastRecovery["%17"] = time.Now()
	cri.RecoverObserved(ctx, recoveryObservation(recoveryBaseline, StateWorking), "/project", idleRecoveryObservation)
	banner := recoveryObservation(recoveryBaseline+"Conversation compacted\n", StateIdle)
	cri.RecoverObserved(ctx, banner, "/project", idleRecoveryObservation)
	if len(cri.pending) != 1 || len(*targets) != 0 {
		t.Fatal("cooldown lost pending event")
	}
	cri.recovery.SetCooldown(time.Nanosecond)
	cri.RecoverObserved(ctx, banner, "/project", idleRecoveryObservation)
	if len(*targets) != 1 {
		t.Fatal("pending event not recovered after cooldown")
	}
	cri.RecoverObserved(ctx, recoveryObservation(banner.Panes[0].RawOutput+"Another context interval\nConversation compacted\n", StateWorking), "/project", idleRecoveryObservation)
	cri.RecoverObserved(ctx, SessionObservation{Session: "project"}, "/project", idleRecoveryObservation)
	if len(cri.pending) != 0 {
		t.Fatal("observation gap kept stale authorization")
	}
}

func TestMonitoredRecoveryTopologyAndProcessLifetimes(t *testing.T) {
	cri, targets := monitoredRecovery(t)
	ctx := context.Background()
	cri.RecoverObserved(ctx, recoveryObservation(recoveryBaseline, StateWorking), "/project", idleRecoveryObservation)
	cri.RecoverObserved(ctx, recoveryObservation(recoveryBaseline+"Conversation compacted\n", StateWorking), "/project", idleRecoveryObservation)
	partial := SessionObservation{Session: "project", ObservedAt: time.Now(), Complete: false}
	cri.RecoverObserved(ctx, partial, "/project", idleRecoveryObservation)
	if len(cri.panes) != 1 {
		t.Fatal("partial topology forgot a live pane")
	}
	changed := recoveryObservation(recoveryBaseline+"Conversation compacted\n", StateIdle)
	changed.Panes[0].Metadata.PID++
	cri.RecoverObserved(ctx, changed, "/project", idleRecoveryObservation)
	if len(*targets) != 0 || len(cri.pending) != 0 {
		t.Fatal("old event replayed to replacement process")
	}
	partial.Complete = true
	cri.RecoverObserved(ctx, partial, "/project", idleRecoveryObservation)
	if len(cri.panes) != 0 || len(cri.detector.Events()) != 0 {
		t.Fatal("complete absence did not forget pane")
	}
}
