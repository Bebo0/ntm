package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/agentmail"
	"github.com/Dicklesworthstone/ntm/internal/assignment"
	"github.com/Dicklesworthstone/ntm/internal/config"
	"github.com/Dicklesworthstone/ntm/internal/coordinator"
	"github.com/Dicklesworthstone/ntm/internal/resilience"
)

// The resident session monitor hosts the session coordinator, so persisted
// [coordinator] toggles act on every spawned session without a separate
// `ntm coordinator run`.
//
// Assignment maintenance is not a policy choice and always runs: it renews
// the exact leases of delivered, still-owned work before they lapse and
// releases leases and claims once the work is closed. With no optional
// feature enabled the host runs only that maintenance, at the `assign --watch`
// cadence, and does nothing at all while the session's assignment ledger has
// no active assignments. Any enabled feature switches to the full coordinator
// loop that `ntm coordinator run` runs. Config is re-read on a fixed cadence,
// so a toggle takes effect without restarting the monitor.
//
// A foreground `ntm coordinator run` or a reservation-maintaining
// `ntm assign --watch` claims the session (resilience.ClaimSessionCoordinator);
// the host then stops its coordinator and resumes after the last claim ends,
// so the two never coordinate one session at the same time.

const (
	monitorCoordinatorModeMaintenance = "maintenance"
	monitorCoordinatorModeFull        = "full"

	monitorCoordinatorPollEvery          = time.Second
	monitorCoordinatorReloadEvery        = 15 * time.Second
	monitorCoordinatorMaintenanceEvery   = 30 * time.Second
	monitorCoordinatorMaintenanceTimeout = 30 * time.Second
)

// monitorCoordinatorHost runs one session's coordinator inside its monitor.
type monitorCoordinatorHost struct {
	session    string
	projectDir string

	pollEvery          time.Duration // claim probe and hosted-coordinator liveness
	reloadEvery        time.Duration // config reload, heartbeat, restart backoff
	maintenanceEvery   time.Duration // maintenance-mode pass cadence
	maintenanceTimeout time.Duration // bound on one maintenance pass

	liveProjectKey string // cached once live pane resolution succeeds
}

func newMonitorCoordinatorHost(session, projectDir string) *monitorCoordinatorHost {
	return &monitorCoordinatorHost{
		session: session, projectDir: projectDir,
		pollEvery: monitorCoordinatorPollEvery, reloadEvery: monitorCoordinatorReloadEvery,
		maintenanceEvery: monitorCoordinatorMaintenanceEvery, maintenanceTimeout: monitorCoordinatorMaintenanceTimeout,
	}
}

// monitorCoordinatorSettings is one effective configuration of the hosted
// coordinator. Any difference between two loads restarts it.
type monitorCoordinatorSettings struct {
	runtime    coordinator.CoordinatorConfig
	ntm        *config.Config
	assign     *config.AssignConfig // effective assignment policy, when auto-assign is enabled
	projectKey string
	configPath string
	warning    string // a requested feature that could not be enabled
}

func (s monitorCoordinatorSettings) equal(other monitorCoordinatorSettings) bool {
	return s.runtime == other.runtime && s.projectKey == other.projectKey && s.configPath == other.configPath &&
		s.warning == other.warning && reflect.DeepEqual(s.assign, other.assign) && reflect.DeepEqual(s.ntm, other.ntm)
}

func (s monitorCoordinatorSettings) features() []string {
	return coordinatorEnabledFeatures(s.runtime, s.ntm)
}

func (s monitorCoordinatorSettings) mode() string {
	if len(s.features()) == 0 {
		return monitorCoordinatorModeMaintenance
	}
	return monitorCoordinatorModeFull
}

// coordinatorEnabledFeatures names every optional coordinator action the
// configuration enables. Feature names match `ntm coordinator enable` where
// one exists.
func coordinatorEnabledFeatures(runtime coordinator.CoordinatorConfig, ntm *config.Config) []string {
	features := []string{}
	if runtime.AutoAssign {
		features = append(features, "auto-assign")
	}
	if runtime.SendDigests {
		features = append(features, "digest")
	}
	if runtime.ConflictNotify {
		features = append(features, "conflict-notify")
	}
	if runtime.ConflictNegotiate {
		features = append(features, "conflict-negotiate")
	}
	if runtime.MailNudge {
		features = append(features, "mail-nudge")
	}
	if runtime.RotationUsageThreshold > 0 {
		features = append(features, "context-rotation")
	}
	if ntm != nil && ntm.Integrations.CAAM.AutoFailover {
		features = append(features, "caam-failover")
	}
	return features
}

// resolveProjectKey prefers the same live-pane resolution `ntm coordinator
// run` uses and falls back to the spawn manifest's project directory.
func (h *monitorCoordinatorHost) resolveProjectKey(ctx context.Context) string {
	if h.liveProjectKey != "" {
		return h.liveProjectKey
	}
	if key, err := resolveCoordinatorProjectKey(ctx, h.session, false); err == nil && strings.TrimSpace(key) != "" {
		h.liveProjectKey = key
		return key
	}
	return refineAgentMailProjectKey(h.session, agentmail.CanonicalProjectKey(h.projectDir))
}

// loadSettings reads the selected config. A load failure is returned with
// built-in defaults so the caller decides whether to keep earlier settings.
func (h *monitorCoordinatorHost) loadSettings(ctx context.Context) (monitorCoordinatorSettings, error) {
	path := selectedConfigPath()
	runtime, ntm, err := loadCoordinatorRuntimeConfigFrom(path)
	settings := monitorCoordinatorSettings{runtime: runtime, ntm: ntm, projectKey: h.resolveProjectKey(ctx), configPath: path}
	if err != nil {
		return settings, err
	}
	if runtime.AutoAssign {
		// Admission fails closed on the same safety policy `coordinator run`
		// enforces, and a policy change restarts the coordinator like any
		// other setting; the rest of the coordinator runs without admission.
		effective, policyErr := loadAuthoritativeAssignmentPolicy(settings.projectKey)
		if policyErr != nil {
			settings.runtime.AutoAssign = false
			settings.warning = fmt.Sprintf("auto-assign is disabled: assignment safety policy did not load: %v", policyErr)
		} else {
			policy := effective.Assign
			settings.assign = &policy
		}
	}
	return settings, nil
}

// hostedCoordinator is one running generation of the hosted coordinator.
type hostedCoordinator struct {
	cancel  context.CancelFunc
	done    chan struct{}
	started atomic.Bool // the generation is coordinating, not still starting
	err     error       // valid once done is closed
}

func (c *hostedCoordinator) stop() {
	c.cancel()
	<-c.done
}

func (c *hostedCoordinator) exited() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// monitorCoordinatorLoop is the host loop's state; only run's goroutine
// touches it.
type monitorCoordinatorLoop struct {
	host    *monitorCoordinatorHost
	control *resilience.SessionCoordinatorHost

	hosted     *hostedCoordinator
	settings   monitorCoordinatorSettings
	loaded     bool // settings hold a load result (possibly defaults)
	loadedOK   bool // settings came from a successful load
	nextReload time.Time
	retryAt    time.Time

	ownershipErr string
	startErr     string
	loadErr      string

	publishedView string
	publishedAt   time.Time
}

// run hosts the coordinator until ctx ends, then stops it and records that
// the host stopped.
func (h *monitorCoordinatorHost) run(ctx context.Context) {
	control, err := resilience.OpenSessionCoordinatorHost(h.session)
	if err != nil {
		slog.Warn("session coordinator is not hosted by this monitor", "session", h.session, "error", err)
		return
	}
	loop := &monitorCoordinatorLoop{host: h, control: control}
	defer control.Close()
	defer loop.stopHosted()
	ticker := time.NewTicker(h.pollEvery)
	defer ticker.Stop()
	for {
		loop.step(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (l *monitorCoordinatorLoop) step(ctx context.Context, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	pending, err := l.control.ClaimPending()
	if err != nil {
		// Exclusivity cannot be verified; running anyway could coordinate the
		// session twice.
		l.ownershipErr = fmt.Sprintf("cannot verify coordination ownership: %v", err)
		l.yield(now)
		return
	}
	if pending {
		l.ownershipErr = ""
		l.yield(now)
		return
	}
	owned, err := l.control.Activate()
	if err != nil {
		l.ownershipErr = fmt.Sprintf("cannot take coordination ownership: %v", err)
		l.yield(now)
		return
	}
	l.ownershipErr = ""
	if !owned {
		// A claim holds coordination between its two locks.
		l.yield(now)
		return
	}
	l.reload(ctx, now)
	if l.hosted != nil && l.hosted.exited() {
		if l.hosted.err != nil {
			l.startErr = l.hosted.err.Error()
			slog.Warn("session coordinator stopped early; retrying", "session", l.host.session,
				"error", l.hosted.err, "retry_in", l.host.reloadEvery)
		}
		l.hosted = nil
		l.retryAt = now.Add(l.host.reloadEvery)
	}
	if l.hosted == nil && !now.Before(l.retryAt) {
		l.hosted = l.host.start(ctx, l.settings, l.control)
	}
	if l.hosted != nil && l.hosted.started.Load() {
		l.startErr = ""
	}
	l.publish(now, resilience.SessionCoordinatorRunning)
}

// yield stops the hosted coordinator and releases ownership: a foreground
// claim takes precedence, and unverifiable ownership must not coordinate.
func (l *monitorCoordinatorLoop) yield(now time.Time) {
	if l.hosted != nil {
		slog.Info("session coordinator pausing", "session", l.host.session, "ownership_error", l.ownershipErr)
		l.stopHosted()
	}
	l.control.Deactivate()
	// Resume with a fresh config read rather than settings that aged while
	// paused.
	l.retryAt, l.nextReload, l.startErr = time.Time{}, time.Time{}, ""
	l.publish(now, resilience.SessionCoordinatorYielded)
}

func (l *monitorCoordinatorLoop) stopHosted() {
	if l.hosted != nil {
		l.hosted.stop()
		l.hosted = nil
	}
}

// reload re-reads config on the reload cadence and restarts the hosted
// coordinator when its effective settings changed.
func (l *monitorCoordinatorLoop) reload(ctx context.Context, now time.Time) {
	if l.loaded && now.Before(l.nextReload) {
		return
	}
	l.nextReload = now.Add(l.host.reloadEvery)
	next, err := l.host.loadSettings(ctx)
	if err != nil {
		if l.loadedOK {
			l.loadErr = fmt.Sprintf("config reload failed; keeping the previous settings: %v", err)
			return
		}
		l.loadErr = fmt.Sprintf("config did not load; running assignment maintenance only: %v", err)
		next.runtime = coordinator.DefaultCoordinatorConfig()
		next.ntm, next.assign = nil, nil
	} else {
		l.loadErr = ""
		l.loadedOK = true
	}
	if l.hosted != nil && !l.settings.equal(next) {
		slog.Info("session coordinator settings changed; restarting", "session", l.host.session,
			"mode", next.mode(), "features", strings.Join(next.features(), ","))
		l.stopHosted()
		l.retryAt, l.startErr = time.Time{}, ""
	}
	l.settings, l.loaded = next, true
}

// publish persists the host's view on change and on the heartbeat cadence.
func (l *monitorCoordinatorLoop) publish(now time.Time, state string) {
	errorText := strings.Join(slices.DeleteFunc([]string{l.ownershipErr, l.startErr, l.loadErr, l.settings.warning},
		func(s string) bool { return s == "" }), "; ")
	mode, features := "", []string{}
	if l.loaded {
		mode, features = l.settings.mode(), l.settings.features()
	}
	view := strings.Join([]string{state, mode, strings.Join(features, ","), errorText, l.settings.configPath, l.settings.projectKey}, "\x00")
	if view == l.publishedView && now.Sub(l.publishedAt) < l.host.reloadEvery {
		return
	}
	if view != l.publishedView {
		slog.Info("session coordinator host", "session", l.host.session, "state", state, "mode", mode,
			"features", strings.Join(features, ","), "error", errorText)
	}
	if err := l.control.Publish(func(status *resilience.SessionCoordinatorStatus) {
		status.State, status.Error = state, errorText
		status.Mode, status.Features = mode, features
		status.ConfigPath, status.ProjectKey = l.settings.configPath, l.settings.projectKey
	}); err != nil {
		slog.Warn("session coordinator status could not be published", "session", l.host.session, "error", err)
		return
	}
	l.publishedView, l.publishedAt = view, now
}

func (h *monitorCoordinatorHost) start(parent context.Context, settings monitorCoordinatorSettings, control *resilience.SessionCoordinatorHost) *hostedCoordinator {
	ctx, cancel := context.WithCancel(parent)
	hosted := &hostedCoordinator{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(hosted.done)
		hosted.err = h.runHosted(ctx, settings, control, func() { hosted.started.Store(true) })
	}()
	return hosted
}

// runHosted runs one coordinator generation until ctx ends. A returned error
// means it stopped early and should be retried.
func (h *monitorCoordinatorHost) runHosted(ctx context.Context, settings monitorCoordinatorSettings, control *resilience.SessionCoordinatorHost, started func()) error {
	mailClient := newAgentMailClient(settings.projectKey)
	if settings.mode() == monitorCoordinatorModeMaintenance {
		// Existing assignments carry their registered owners. Reading the saved
		// coordinator identity suffices: maintenance never registers an
		// identity, sends mail, or admits work.
		name := resolveCoordinatorIdentity(ctx, nil, h.session, settings.projectKey)
		coord := coordinator.New(h.session, settings.projectKey, mailClient, name).WithConfig(settings.runtime)
		started()
		h.maintain(ctx, coord, control)
		return nil
	}

	// The full loop maintains assignments every cycle; a previous
	// maintenance-mode result no longer describes it.
	_ = control.Publish(func(status *resilience.SessionCoordinatorStatus) {
		status.LastMaintenanceAt, status.MaintenanceError = nil, ""
	})
	if settings.runtime.AutoAssign {
		if err := configureAuthoritativeAssignmentPolicy(settings.projectKey); err != nil {
			return fmt.Errorf("auto-assign safety policy: %w", err)
		}
	}
	name := resolveCoordinatorIdentity(ctx, mailClient, h.session, settings.projectKey)
	coord := coordinator.New(h.session, settings.projectKey, mailClient, name).
		WithConfig(settings.runtime).
		WithNTMConfig(settings.ntm)
	if err := coord.Start(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		// The full loop refuses to start without a clean session observation.
		// Protecting existing work does not need one, so keep leases alive
		// until the retry.
		h.maintainOnce(ctx, coord, control)
		return fmt.Errorf("start coordinator: %w", err)
	}
	started()
	<-ctx.Done()
	coord.Stop()
	return nil
}

// maintain runs assignment maintenance passes until ctx ends. A pass runs only
// while the ledger has active assignments; an idle session costs one ledger
// read per pass.
func (h *monitorCoordinatorHost) maintain(ctx context.Context, coord *coordinator.SessionCoordinator, control *resilience.SessionCoordinatorHost) {
	ticker := time.NewTicker(h.maintenanceEvery)
	defer ticker.Stop()
	for {
		h.maintainOnce(ctx, coord, control)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (h *monitorCoordinatorHost) maintainOnce(ctx context.Context, coord *coordinator.SessionCoordinator, control *resilience.SessionCoordinatorHost) {
	if ctx.Err() != nil {
		return
	}
	ledger, err := assignment.LoadStoreStrictReadOnly(h.session)
	if err != nil {
		h.reportMaintenance(control, false, fmt.Errorf("read assignment ledger: %w", err))
		return
	}
	if len(ledger.ListActive()) == 0 {
		h.reportMaintenance(control, false, nil)
		return
	}
	passCtx, cancel := context.WithTimeout(ctx, h.maintenanceTimeout)
	err = coord.MaintainAssignments(passCtx)
	err = errors.Join(err, passCtx.Err())
	cancel()
	if ctx.Err() != nil {
		return
	}
	h.reportMaintenance(control, true, err)
}

func (h *monitorCoordinatorHost) reportMaintenance(control *resilience.SessionCoordinatorHost, ran bool, err error) {
	message := ""
	if err != nil {
		message = err.Error()
	}
	_ = control.Publish(func(status *resilience.SessionCoordinatorStatus) {
		if message != status.MaintenanceError {
			if message != "" {
				// Loss of protection is the one thing an operator must see:
				// the maintainer retains affected assignments and never
				// reacquires leases.
				slog.Warn("assignment reservation maintenance degraded", "session", h.session, "error", message)
			} else if status.MaintenanceError != "" {
				slog.Info("assignment reservation maintenance recovered", "session", h.session)
			}
		}
		status.MaintenanceError = message
		if ran {
			now := time.Now().UTC()
			status.LastMaintenanceAt = &now
		}
	})
}
