package resilience

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/process"
)

// Session coordination ownership.
//
// The resident session monitor hosts the session coordinator by default.
// Foreground commands that run the same coordination (`ntm coordinator run`,
// and `ntm assign --watch` while it maintains reservations) take precedence:
// their claim makes the monitor stop its coordinator within one poll, and the
// monitor resumes after the last claim is released. Two flock(2) files next to
// the monitor's control files carry the protocol:
//
//	<session>-coordinator.lock        the monitor holds it exclusively while
//	                                  its coordinator runs; every claim holds
//	                                  it shared, so neither can run while the
//	                                  other does.
//	<session>-coordinator.claim.lock  every claim holds it shared for its whole
//	                                  life; the monitor probes it exclusively
//	                                  to learn that a claim is waiting.
//
// The kernel releases both on process exit, so a crashed owner never wedges
// the other side. Only the monitor publishes <session>-coordinator.json for
// status surfaces; readers recompute liveness from its process identity.

var errControlLockBusy = errors.New("session control lock is held")

// Session coordinator host states published in SessionCoordinatorStatus.
const (
	SessionCoordinatorStarting = "starting"
	SessionCoordinatorRunning  = "running"
	SessionCoordinatorYielded  = "yielded"
	SessionCoordinatorStopped  = "stopped"
)

// SessionCoordinatorHostMonitor names the only host that publishes a status
// record: the resident session monitor.
const SessionCoordinatorHostMonitor = "session-monitor"

const (
	sessionCoordinatorClaimTimeout   = time.Minute
	sessionCoordinatorClaimPoll      = 50 * time.Millisecond
	sessionCoordinatorHeartbeatStale = time.Minute
)

// SessionCoordinatorStatus is the monitor-hosted coordinator's published
// state. Alive and Healthy are computed when read; a persisted "running" row
// alone is not proof that anything is running.
type SessionCoordinatorStatus struct {
	Session           string     `json:"session"`
	Host              string     `json:"host"`
	PID               int        `json:"pid"`
	ProcessStarted    time.Time  `json:"process_started"`
	State             string     `json:"state"`
	Mode              string     `json:"mode,omitempty"`
	Features          []string   `json:"features"`
	ConfigPath        string     `json:"config_path,omitempty"`
	ProjectKey        string     `json:"project_key,omitempty"`
	StateSince        time.Time  `json:"state_since"`
	HeartbeatAt       time.Time  `json:"heartbeat_at"`
	LastMaintenanceAt *time.Time `json:"last_maintenance_at,omitempty"`
	MaintenanceError  string     `json:"maintenance_error,omitempty"`
	Error             string     `json:"error,omitempty"`
	Alive             bool       `json:"alive"`
	Healthy           bool       `json:"healthy"`
}

func coordinatorPrefix(session string) (string, error) {
	safe, err := sanitizeSessionName(session)
	if err != nil {
		return "", err
	}
	return filepath.Join(ManifestDir(), "monitors", safe+"-coordinator"), nil
}

func prepareCoordinatorPrefix(session string) (string, error) {
	prefix, err := coordinatorPrefix(session)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(prefix), 0700); err != nil {
		return "", err
	}
	return prefix, nil
}

// SessionCoordinatorHost is the session monitor's side of coordination
// ownership. It never waits for a lock: a held lock means a foreground claim
// owns coordination and the monitor stays out of the way.
type SessionCoordinatorHost struct {
	prefix string
	mu     sync.Mutex
	active *os.File
	status SessionCoordinatorStatus
	closed bool
}

// OpenSessionCoordinatorHost prepares the monitor's control files and
// publishes a starting record. It does not take ownership.
func OpenSessionCoordinatorHost(session string) (*SessionCoordinatorHost, error) {
	if err := monitorPlatformSupported(); err != nil {
		return nil, err
	}
	prefix, err := prepareCoordinatorPrefix(session)
	if err != nil {
		return nil, err
	}
	started, err := process.StartTime(os.Getpid())
	if err != nil {
		return nil, fmt.Errorf("identify session coordinator host process: %w", err)
	}
	now := time.Now().UTC()
	host := &SessionCoordinatorHost{
		prefix: prefix,
		status: SessionCoordinatorStatus{
			Session: session, Host: SessionCoordinatorHostMonitor, PID: os.Getpid(), ProcessStarted: started,
			State: SessionCoordinatorStarting, Features: []string{}, StateSince: now,
		},
	}
	if err := host.writeLocked(); err != nil {
		return nil, err
	}
	return host, nil
}

// ClaimPending reports whether a foreground claim is waiting for, or holding,
// session coordination.
func (h *SessionCoordinatorHost) ClaimPending() (bool, error) {
	file, err := tryControlLock(h.prefix+".claim.lock", false)
	if errors.Is(err, errControlLockBusy) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, file.Close()
}

// Activate takes exclusive coordination ownership unless a foreground claim
// is waiting or active. It reports whether the host now owns coordination and
// is idempotent while owned.
func (h *SessionCoordinatorHost) Activate() (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false, errors.New("session coordinator host is closed")
	}
	if h.active != nil {
		return true, nil
	}
	pending, err := h.ClaimPending()
	if err != nil || pending {
		return false, err
	}
	file, err := tryControlLock(h.prefix+".lock", false)
	if errors.Is(err, errControlLockBusy) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	h.active = file
	return true, nil
}

// Deactivate releases coordination ownership. Callers stop their coordinator
// first: releasing the lock is what admits a waiting claim.
func (h *SessionCoordinatorHost) Deactivate() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.active != nil {
		_ = h.active.Close()
		h.active = nil
	}
}

// Publish applies update to the status record, refreshes its heartbeat and
// persists it. A state change restarts StateSince.
func (h *SessionCoordinatorHost) Publish(update func(*SessionCoordinatorStatus)) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	previous := h.status.State
	if update != nil {
		update(&h.status)
	}
	if h.status.Features == nil {
		h.status.Features = []string{}
	}
	if h.status.State != previous {
		h.status.StateSince = time.Now().UTC()
	}
	return h.writeLocked()
}

// Close releases ownership and records that the host stopped.
func (h *SessionCoordinatorHost) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	if h.active != nil {
		_ = h.active.Close()
		h.active = nil
	}
	h.status.State = SessionCoordinatorStopped
	h.status.StateSince = time.Now().UTC()
	_ = h.writeLocked()
}

func (h *SessionCoordinatorHost) writeLocked() error {
	h.status.HeartbeatAt = time.Now().UTC()
	h.status.Alive, h.status.Healthy = false, false
	return writeMonitorJSON(h.prefix+".json", h.status)
}

// SessionCoordinatorClaim is a foreground command's precedence over the
// session monitor's coordinator. Release it when the command stops
// coordinating; the monitor then resumes.
type SessionCoordinatorClaim struct {
	claim  *os.File
	active *os.File
	// Preempted reports that the session monitor was running the
	// coordinator and stopped it for this claim.
	Preempted bool
}

// ClaimSessionCoordinator waits, bounded, until no session monitor runs the
// coordinator for session and keeps it from restarting until Release. Other
// claims are not excluded. Without Unix locking no monitor can host a
// coordinator, so the claim is empty.
func ClaimSessionCoordinator(ctx context.Context, session string) (*SessionCoordinatorClaim, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := monitorPlatformSupported(); err != nil {
		return &SessionCoordinatorClaim{}, nil
	}
	prefix, err := prepareCoordinatorPrefix(session)
	if err != nil {
		return nil, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, sessionCoordinatorClaimTimeout)
	defer cancel()
	claim, _, err := waitControlLock(waitCtx, prefix+".claim.lock", true)
	if err != nil {
		return nil, fmt.Errorf("register session coordination claim: %w", err)
	}
	active, waited, err := waitControlLock(waitCtx, prefix+".lock", true)
	if err != nil {
		_ = claim.Close()
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("the session monitor did not stop its coordinator for %s within %s", session, sessionCoordinatorClaimTimeout)
		}
		return nil, fmt.Errorf("wait for the session monitor to stop its coordinator: %w", err)
	}
	return &SessionCoordinatorClaim{claim: claim, active: active, Preempted: waited}, nil
}

// Release ends the claim. It is safe to call more than once.
func (c *SessionCoordinatorClaim) Release() {
	if c == nil {
		return
	}
	if c.active != nil {
		_ = c.active.Close()
		c.active = nil
	}
	if c.claim != nil {
		_ = c.claim.Close()
		c.claim = nil
	}
}

func waitControlLock(ctx context.Context, path string, shared bool) (*os.File, bool, error) {
	ticker := time.NewTicker(sessionCoordinatorClaimPoll)
	defer ticker.Stop()
	waited := false
	for {
		file, err := tryControlLock(path, shared)
		if err == nil {
			return file, waited, nil
		}
		if !errors.Is(err, errControlLockBusy) {
			return nil, waited, err
		}
		waited = true
		select {
		case <-ctx.Done():
			return nil, waited, ctx.Err()
		case <-ticker.C:
		}
	}
}

// ReadSessionCoordinatorStatus returns the monitor-hosted coordinator's last
// published record with liveness recomputed. A session whose monitor never
// hosted a coordinator returns an error satisfying errors.Is(err,
// os.ErrNotExist).
func ReadSessionCoordinatorStatus(session string) (*SessionCoordinatorStatus, error) {
	prefix, err := coordinatorPrefix(session)
	if err != nil {
		return nil, err
	}
	var status SessionCoordinatorStatus
	if err := readMonitorJSON(prefix+".json", &status); err != nil {
		return nil, err
	}
	if status.Session != session || status.PID <= 0 {
		return nil, errors.New("session coordinator status identity mismatch")
	}
	status.Alive, status.Healthy = false, false
	if status.State != SessionCoordinatorStopped && process.IsAlive(status.PID) {
		started, err := process.StartTime(status.PID)
		status.Alive = err == nil && started.Equal(status.ProcessStarted)
	}
	status.Healthy = status.Alive && time.Since(status.HeartbeatAt) < sessionCoordinatorHeartbeatStale
	return &status, nil
}
