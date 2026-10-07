package resilience

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestSessionCoordinatorClaimPreemptsHostAndHostResumes covers the ownership
// protocol between the session monitor and foreground coordinators: a claim
// waits for the running host to stop, the host cannot restart while any claim
// is held, claims do not exclude each other, and the host resumes after the
// last claim ends.
func TestSessionCoordinatorClaimPreemptsHostAndHostResumes(t *testing.T) {
	prepareMonitorTest(t)
	const session = "coordinated"
	host, err := OpenSessionCoordinatorHost(session)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	if status, err := ReadSessionCoordinatorStatus(session); err != nil || status.State != SessionCoordinatorStarting || !status.Alive ||
		!status.Healthy || status.PID != os.Getpid() || status.Host != SessionCoordinatorHostMonitor || status.Features == nil {
		t.Fatalf("opened host status = %+v, err = %v", status, err)
	}
	if owned, err := host.Activate(); err != nil || !owned {
		t.Fatalf("idle session: owned=%t err=%v", owned, err)
	}
	if owned, err := host.Activate(); err != nil || !owned {
		t.Fatalf("activation is not idempotent: owned=%t err=%v", owned, err)
	}
	if err := host.Publish(func(status *SessionCoordinatorStatus) {
		status.State, status.Mode, status.Features = SessionCoordinatorRunning, "maintenance", nil
	}); err != nil {
		t.Fatal(err)
	}
	running, err := ReadSessionCoordinatorStatus(session)
	if err != nil || running.State != SessionCoordinatorRunning || running.Mode != "maintenance" || running.Features == nil {
		t.Fatalf("published status = %+v, err = %v", running, err)
	}

	type claimResult struct {
		claim *SessionCoordinatorClaim
		err   error
	}
	first := make(chan claimResult, 1)
	go func() {
		claim, err := ClaimSessionCoordinator(t.Context(), session)
		first <- claimResult{claim, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		pending, err := host.ClaimPending()
		if err != nil {
			t.Fatal(err)
		}
		if pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("claim never registered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case result := <-first:
		t.Fatalf("claim was granted while the host coordinated: %+v", result)
	case <-time.After(4 * sessionCoordinatorClaimPoll):
	}
	host.Deactivate()
	var claim *SessionCoordinatorClaim
	select {
	case result := <-first:
		if result.err != nil || !result.claim.Preempted {
			t.Fatalf("claim after yield = %+v, err = %v", result.claim, result.err)
		}
		claim = result.claim
	case <-time.After(5 * time.Second):
		t.Fatal("claim was not granted after the host yielded")
	}
	if owned, err := host.Activate(); err != nil || owned {
		t.Fatalf("host restarted during a claim: owned=%t err=%v", owned, err)
	}
	second, err := ClaimSessionCoordinator(t.Context(), session)
	if err != nil || second.Preempted {
		t.Fatalf("concurrent claim = %+v, err = %v", second, err)
	}
	claim.Release()
	claim.Release()
	if owned, err := host.Activate(); err != nil || owned {
		t.Fatalf("host restarted while another claim remained: owned=%t err=%v", owned, err)
	}
	second.Release()
	if owned, err := host.Activate(); err != nil || !owned {
		t.Fatalf("host did not resume after the last claim: owned=%t err=%v", owned, err)
	}

	host.Close()
	stopped, err := ReadSessionCoordinatorStatus(session)
	if err != nil || stopped.State != SessionCoordinatorStopped || stopped.Alive || stopped.Healthy {
		t.Fatalf("closed host status = %+v, err = %v", stopped, err)
	}
	if owned, err := host.Activate(); err == nil || owned {
		t.Fatalf("closed host activated: owned=%t err=%v", owned, err)
	}
	// Closing released ownership: a claim needs no preemption.
	after, err := ClaimSessionCoordinator(t.Context(), session)
	if err != nil || after.Preempted {
		t.Fatalf("claim after host close = %+v, err = %v", after, err)
	}
	after.Release()
}

// TestSessionCoordinatorClaimAbandonedWithoutYieldLeavesNoClaim: a claim that
// gives up waiting must not keep the host paused.
func TestSessionCoordinatorClaimAbandonedWithoutYieldLeavesNoClaim(t *testing.T) {
	prepareMonitorTest(t)
	const session = "stubborn"
	host, err := OpenSessionCoordinatorHost(session)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	if owned, err := host.Activate(); err != nil || !owned {
		t.Fatalf("owned=%t err=%v", owned, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*sessionCoordinatorClaimPoll)
	defer cancel()
	claim, err := ClaimSessionCoordinator(ctx, session)
	if err == nil || claim != nil || !errors.Is(err, context.DeadlineExceeded) ||
		!strings.Contains(err.Error(), "wait for the session monitor to stop its coordinator") {
		t.Fatalf("abandoned claim = %+v, err = %v", claim, err)
	}
	if pending, err := host.ClaimPending(); err != nil || pending {
		t.Fatalf("abandoned claim left the host paused: pending=%t err=%v", pending, err)
	}
}

func TestReadSessionCoordinatorStatusWithoutHost(t *testing.T) {
	prepareMonitorTest(t)
	if status, err := ReadSessionCoordinatorStatus("never-hosted"); !errors.Is(err, os.ErrNotExist) || status != nil {
		t.Fatalf("status = %+v, err = %v", status, err)
	}
	if _, err := ReadSessionCoordinatorStatus("../escape"); err == nil {
		t.Fatal("path-traversing session name was accepted")
	}
}
