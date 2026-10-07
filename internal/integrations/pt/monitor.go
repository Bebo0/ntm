// Package pt provides integration with process_triage (pt) for Bayesian agent health monitoring.
// It monitors agent processes continuously and triggers alerts when agents become stuck or zombie.
package pt

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/config"
	"github.com/Dicklesworthstone/ntm/internal/integrations/rano"
	"github.com/Dicklesworthstone/ntm/internal/tools"
)

func monitorLogger() *slog.Logger {
	return slog.Default().With("component", "integrations.pt.monitor")
}

// Classification represents an agent's health classification.
type Classification string

const (
	ClassUseful  Classification = "useful"  // Agent is actively doing useful work
	ClassWaiting Classification = "waiting" // Agent is waiting for input/API response
	ClassIdle    Classification = "idle"    // Agent is idle but responsive
	ClassStuck   Classification = "stuck"   // Agent appears to be stuck/unresponsive
	ClassZombie  Classification = "zombie"  // Agent process is defunct
	ClassUnknown Classification = "unknown" // Unable to classify
)

// ClassificationEvent records a single classification result.
type ClassificationEvent struct {
	Classification         Classification `json:"classification"`
	Confidence             float64        `json:"confidence"` // 0.0 to 1.0
	Timestamp              time.Time      `json:"timestamp"`
	Reason                 string         `json:"reason,omitempty"`
	NetworkActive          bool           `json:"network_active,omitempty"` // From rano if available
	Source                 string         `json:"source,omitempty"`
	Recommendation         string         `json:"recommendation,omitempty"`
	AbandonmentProbability float64        `json:"abandonment_probability,omitempty"`
}

// AgentState tracks the current state and history for an agent pane.
type AgentState struct {
	Pane             string                `json:"pane"`
	Session          string                `json:"session,omitempty"`
	WindowIndex      int                   `json:"window_index"`
	PaneIndex        int                   `json:"pane_index"`
	PID              int                   `json:"pid"`
	Classification   Classification        `json:"classification"`
	Confidence       float64               `json:"confidence"`
	Since            time.Time             `json:"since"` // When this classification started
	LastCheck        time.Time             `json:"last_check"`
	History          []ClassificationEvent `json:"history"`
	ConsecutiveCount int                   `json:"consecutive_count"` // How many times in a row this classification
}

// AlertType indicates the kind of alert being triggered.
type AlertType string

const (
	AlertStuck  AlertType = "stuck"
	AlertZombie AlertType = "zombie"
	AlertIdle   AlertType = "idle"
)

// Alert represents a health alert for an agent.
type Alert struct {
	Session   string         `json:"session,omitempty"`
	Type      AlertType      `json:"type"`
	Pane      string         `json:"pane"`
	PID       int            `json:"pid"`
	State     Classification `json:"state"`
	Duration  time.Duration  `json:"duration"` // How long in this state
	Timestamp time.Time      `json:"timestamp"`
	Message   string         `json:"message"`
}

// ClassificationStateChange describes a pane classification transition.
// It is emitted only for initial observations and real classification changes.
type ClassificationStateChange struct {
	Session          string              `json:"session,omitempty"`
	Pane             string              `json:"pane"`
	PID              int                 `json:"pid"`
	Previous         Classification      `json:"previous"`
	Current          Classification      `json:"current"`
	Event            ClassificationEvent `json:"event"`
	Initial          bool                `json:"initial,omitempty"`
	Since            time.Time           `json:"since"`
	ConsecutiveCount int                 `json:"consecutive_count"`
}

// StateChangeCallback receives cycle-safe PT classification transitions.
type StateChangeCallback func(ClassificationStateChange)

// AlertCallback receives cycle-safe PT alerts when they are emitted.
type AlertCallback func(Alert)

type processClassifier interface {
	IsAvailable(context.Context) bool
	ClassifyProcesses(context.Context, []int) ([]tools.PTProcessResult, error)
	InvalidateStatusCache()
}

type processPaneMap interface {
	RefreshContext(context.Context) error
	GetPIDLabels() map[int]string
	GetPaneForPID(int) *rano.PaneIdentity
}

type processNetworkSource interface {
	IsAvailable(context.Context) bool
	GetAllProcessStats(context.Context) ([]tools.RanoProcessStats, error)
}

// HealthMonitor monitors agent health via process_triage.
type HealthMonitor struct {
	mu sync.RWMutex

	lifecycleMu sync.Mutex

	config      *config.ProcessTriageConfig
	pidMap      processPaneMap
	ptAdapter   processClassifier
	ranoAdapter processNetworkSource
	pollContext context.Context
	cancelPoll  context.CancelFunc

	states map[string]*AgentState // pane -> state
	stopCh chan struct{}
	doneCh chan struct{}

	running bool
	session string
	useRano bool

	// Alert thresholds
	idleThreshold  time.Duration
	stuckThreshold time.Duration
	maxHistory     int // Maximum history entries to keep per agent

	stateChangeCallbacks []StateChangeCallback
	alertCallbacks       []AlertCallback
}

// HealthMonitorOption configures a HealthMonitor.
type HealthMonitorOption func(*HealthMonitor)

// WithStateChangeCallback registers a callback for initial classifications and
// classification transitions. Callbacks run outside the monitor lock.
func WithStateChangeCallback(cb StateChangeCallback) HealthMonitorOption {
	return func(m *HealthMonitor) {
		if cb != nil {
			m.stateChangeCallbacks = append(m.stateChangeCallbacks, cb)
		}
	}
}

// WithAlertCallback registers a callback for emitted alerts. Callbacks run
// outside the monitor lock.
func WithAlertCallback(cb AlertCallback) HealthMonitorOption {
	return func(m *HealthMonitor) {
		if cb != nil {
			m.alertCallbacks = append(m.alertCallbacks, cb)
		}
	}
}

// WithRanoDatabase selects the rano observer database whose recent connection
// events can downgrade a suspected-stuck agent to waiting ([integrations.rano]
// sqlite_path). Empty keeps rano's default, observer.sqlite in the working
// directory.
func WithRanoDatabase(path string) HealthMonitorOption {
	return func(m *HealthMonitor) {
		adapter := tools.NewRanoAdapter()
		adapter.SetDatabase(path)
		m.ranoAdapter = adapter
	}
}

// NewHealthMonitor creates a new health monitor with the given configuration.
func NewHealthMonitor(cfg *config.ProcessTriageConfig, opts ...HealthMonitorOption) *HealthMonitor {
	if cfg == nil {
		defaults := config.DefaultProcessTriageConfig()
		cfg = &defaults
	}
	configCopy := *cfg
	cfg = &configCopy
	m := &HealthMonitor{
		config:         cfg,
		states:         make(map[string]*AgentState),
		stopCh:         make(chan struct{}),
		doneCh:         make(chan struct{}),
		ptAdapter:      tools.NewPTAdapter(),
		ranoAdapter:    tools.NewRanoAdapter(),
		idleThreshold:  time.Duration(cfg.IdleThreshold) * time.Second,
		stuckThreshold: time.Duration(cfg.StuckThreshold) * time.Second,
		maxHistory:     100, // Keep last 100 classification events per agent
		useRano:        cfg.UseRanoData,
	}

	for _, opt := range opts {
		opt(m)
	}

	// Create PID map for the session
	m.pidMap = rano.NewPIDMap(m.session)

	return m
}

// Start begins the monitoring loop.
// It's safe to call Start multiple times; subsequent calls are no-ops.
func (m *HealthMonitor) Start() error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return nil
	}
	if m.config.CheckInterval <= 0 {
		m.mu.Unlock()
		return fmt.Errorf("process_triage check interval must be positive")
	}

	// Verify pt is available
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !m.ptAdapter.IsAvailable(ctx) {
		m.mu.Unlock()
		return fmt.Errorf("process_triage (pt) is not available")
	}

	m.running = true
	m.pollContext, m.cancelPoll = context.WithCancel(context.Background())
	m.stopCh = make(chan struct{})
	m.doneCh = make(chan struct{})
	m.mu.Unlock()

	go m.monitorLoop()

	monitorLogger().Info("health monitor started",
		"session", m.session,
		"check_interval", m.config.CheckInterval,
		"use_rano", m.useRano,
	)

	return nil
}

// Stop halts the monitoring loop.
// It blocks until the loop has fully stopped.
func (m *HealthMonitor) Stop() {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return
	}
	m.running = false
	if m.cancelPoll != nil {
		m.cancelPoll()
	}
	close(m.stopCh)
	m.mu.Unlock()

	// Wait for loop to finish
	<-m.doneCh
	m.mu.Lock()
	clear(m.states)
	m.mu.Unlock()

	monitorLogger().Info("health monitor stopped")
}

// Running returns true if the monitor is currently running.
func (m *HealthMonitor) Running() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.running
}

// GetAllStates returns the current state for all monitored panes.
func (m *HealthMonitor) GetAllStates() map[string]*AgentState {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]*AgentState, len(m.states))
	for pane, state := range m.states {
		stateCopy := *state
		stateCopy.History = append([]ClassificationEvent(nil), state.History...)
		result[pane] = &stateCopy
	}
	return result
}

// monitorLoop is the main monitoring goroutine.
func (m *HealthMonitor) monitorLoop() {
	defer close(m.doneCh)

	interval := time.Duration(m.config.CheckInterval) * time.Second
	// Do an initial check immediately
	m.checkAll()
	// A slow whole-host sample must not create a catch-up burst of scans.
	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			m.checkAll()
			timer.Reset(interval)
		case <-m.stopCh:
			return
		}
	}
}

// checkAll checks the health of all agent processes.
func (m *HealthMonitor) checkAll() {
	m.mu.RLock()
	parent := m.pollContext
	m.mu.RUnlock()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	completed := false
	defer func() {
		if !completed {
			// Unavailable evidence must not leave a stale stuck/useful verdict
			// visible indefinitely or accrue duration through a failed sample.
			m.mu.Lock()
			clear(m.states)
			m.mu.Unlock()
		}
	}()

	// Refresh PID map to get current pane->PID mappings
	if err := m.pidMap.RefreshContext(ctx); err != nil {
		monitorLogger().Warn("failed to refresh PID map", "error", err)
		return
	}

	// Get all PIDs with their labels
	pidLabels := m.pidMap.GetPIDLabels()
	identities := make(map[int]*rano.PaneIdentity, len(pidLabels))
	for pid := range pidLabels {
		identity := m.pidMap.GetPaneForPID(pid)
		if identity == nil || identity.PaneID == "" {
			delete(pidLabels, pid)
			continue
		}
		kind := string(identity.AgentType.Canonical())
		if kind == "" || kind == "user" || kind == "unknown" {
			delete(pidLabels, pid)
			continue
		}
		identities[pid] = identity
		pidLabels[pid] = identity.PaneID
	}
	if len(pidLabels) == 0 {
		monitorLogger().Debug("no panes to monitor")
		return
	}

	// Collect all PIDs to classify
	pids := make([]int, 0, len(pidLabels))
	for pid := range pidLabels {
		pids = append(pids, pid)
	}
	sort.Ints(pids)

	// Classify all processes at once
	results, err := m.ptAdapter.ClassifyProcesses(ctx, pids)
	if err != nil {
		monitorLogger().Warn("failed to classify processes", "error", err)
		return
	}

	// Get rano stats if enabled
	var ranoStats map[int]*tools.RanoProcessStats
	if m.useRano && m.ranoAdapter.IsAvailable(ctx) {
		allStats, err := m.ranoAdapter.GetAllProcessStats(ctx)
		if err == nil {
			ranoStats = make(map[int]*tools.RanoProcessStats, len(allStats))
			for i := range allStats {
				ranoStats[allStats[i].PID] = &allStats[i]
			}
		}
	}

	if ctx.Err() != nil {
		return
	}
	// Collapse all shell/child results to one deterministic pane observation.
	// A quiet child cannot overwrite a sibling's abandonment signal, and one
	// poll cannot advance ConsecutiveCount once per child or emit duplicate alerts.
	now := time.Now()
	byPID := make(map[int]tools.PTProcessResult, len(results))
	for _, result := range results {
		byPID[result.PID] = result
	}
	type paneSample struct {
		pid   int
		event ClassificationEvent
	}
	samples := make(map[string]paneSample)
	var stateChanges []ClassificationStateChange
	var alerts []Alert
	for _, pid := range pids {
		result, ok := byPID[pid]
		if !ok {
			result = tools.PTProcessResult{PID: pid, Classification: tools.PTClassUnknown,
				Reason: "PT returned no process observation"}
		}
		pane := pidLabels[pid]

		// Convert pt classification to our classification
		classification := mapPTClassification(result.Classification)

		// Check rano for network activity
		networkActive := false
		if ranoStats != nil {
			if stats, ok := ranoStats[result.PID]; ok {
				// Rano exports connection events, not HTTP request/byte counts.
				if stats.LastConnection != "" {
					if lastReq, err := time.Parse(time.RFC3339Nano, stats.LastConnection); err == nil {
						age := now.Sub(lastReq)
						networkActive = age >= 0 && age < time.Duration(m.config.CheckInterval)*time.Second
					}
				}
			}
		}

		// If network active and classified as stuck, downgrade to waiting
		if networkActive && classification == ClassStuck {
			classification = ClassWaiting
		}

		event := ClassificationEvent{
			Classification: classification,
			Confidence:     result.Confidence,
			Timestamp:      now,
			Reason:         result.Reason,
			NetworkActive:  networkActive,
			Source:         result.Source,
			Recommendation: result.Recommendation,
		}
		if result.AbandonmentProbability != nil {
			event.AbandonmentProbability = *result.AbandonmentProbability
		}
		previous, exists := samples[pane]
		if !exists || ptStatePriority(event.Classification) > ptStatePriority(previous.event.Classification) ||
			(ptStatePriority(event.Classification) == ptStatePriority(previous.event.Classification) &&
				(event.Confidence > previous.event.Confidence ||
					(event.Confidence == previous.event.Confidence && event.AbandonmentProbability > previous.event.AbandonmentProbability))) {
			samples[pane] = paneSample{pid: pid, event: event}
		}
	}
	panes := make([]string, 0, len(samples))
	for pane := range samples {
		panes = append(panes, pane)
	}
	sort.Strings(panes)
	m.mu.Lock()
	for _, pane := range panes {
		sample := samples[pane]
		identity := identities[sample.pid]
		if change := m.updateState(pane, sample.pid, sample.event); change != nil {
			change.Session = identity.Session
			stateChanges = append(stateChanges, *change)
		}
		state := m.states[pane]
		state.Session, state.WindowIndex, state.PaneIndex = identity.Session, identity.WindowIndex, identity.PaneIndex
		for _, alert := range m.checkAlerts(pane) {
			alert.Session = identity.Session
			if sample.event.Source == "pt_agent_watch" {
				alert.Message = fmt.Sprintf("PT suspects an abandoned process in pane %s; inspect before acting", pane)
			}
			alerts = append(alerts, alert)
		}
	}

	// Clean up states for panes that no longer exist
	for pane := range m.states {
		if _, seen := samples[pane]; !seen {
			delete(m.states, pane)
			monitorLogger().Debug("removed stale pane state", "pane", pane)
		}
	}

	m.mu.Unlock()
	completed = true

	for _, change := range stateChanges {
		m.emitStateChange(change)
	}
	for _, alert := range alerts {
		m.sendAlert(alert)
	}
}

func ptStatePriority(class Classification) int {
	switch class {
	case ClassZombie:
		return 5
	case ClassStuck:
		return 4
	case ClassWaiting:
		return 3
	case ClassUseful:
		return 2
	case ClassIdle:
		return 1
	default:
		return 0
	}
}

// mapPTClassification converts pt classification to our classification.
func mapPTClassification(ptClass tools.PTClassification) Classification {
	switch ptClass {
	case tools.PTClassUseful:
		return ClassUseful
	case tools.PTClassAbandoned:
		return ClassStuck
	case tools.PTClassZombie:
		return ClassZombie
	default:
		return ClassUnknown
	}
}

// updateState updates the state for a pane with a new classification event.
// Must be called with m.mu held.
func (m *HealthMonitor) updateState(pane string, pid int, event ClassificationEvent) *ClassificationStateChange {
	state, exists := m.states[pane]
	if !exists || state.PID != pid {
		state = &AgentState{
			Pane:             pane,
			PID:              pid,
			Classification:   event.Classification,
			Confidence:       event.Confidence,
			Since:            event.Timestamp,
			LastCheck:        event.Timestamp,
			History:          make([]ClassificationEvent, 0, m.maxHistory),
			ConsecutiveCount: 1,
		}
		m.states[pane] = state

		// Add to history
		state.History = append(state.History, event)
		return &ClassificationStateChange{
			Session:          m.session,
			Pane:             pane,
			PID:              pid,
			Previous:         ClassUnknown,
			Current:          event.Classification,
			Event:            event,
			Initial:          true,
			Since:            state.Since,
			ConsecutiveCount: state.ConsecutiveCount,
		}
	} else {
		state.PID = pid
		state.LastCheck = event.Timestamp

		if state.Classification == event.Classification {
			state.ConsecutiveCount++
			state.Confidence = event.Confidence // Update confidence
		} else {
			// Classification changed
			previous := state.Classification
			state.Classification = event.Classification
			state.Confidence = event.Confidence
			state.Since = event.Timestamp
			state.ConsecutiveCount = 1

			// Add to history
			state.History = append(state.History, event)
			if len(state.History) > m.maxHistory {
				state.History = state.History[len(state.History)-m.maxHistory:]
			}

			return &ClassificationStateChange{
				Session:          m.session,
				Pane:             pane,
				PID:              pid,
				Previous:         previous,
				Current:          event.Classification,
				Event:            event,
				Since:            state.Since,
				ConsecutiveCount: state.ConsecutiveCount,
			}
		}
	}

	// Add to history
	state.History = append(state.History, event)

	// Trim history if needed
	if len(state.History) > m.maxHistory {
		state.History = state.History[len(state.History)-m.maxHistory:]
	}
	return nil
}

// checkAlerts checks if alerts should be triggered for a pane.
// Must be called with m.mu held.
func (m *HealthMonitor) checkAlerts(pane string) []Alert {
	state, ok := m.states[pane]
	if !ok {
		return nil
	}

	duration := time.Since(state.Since)
	var alerts []Alert

	switch state.Classification {
	case ClassStuck:
		if duration >= m.stuckThreshold {
			alerts = append(alerts, Alert{
				Session:   m.session,
				Type:      AlertStuck,
				Pane:      pane,
				PID:       state.PID,
				State:     state.Classification,
				Duration:  duration,
				Timestamp: time.Now(),
				Message:   fmt.Sprintf("Agent %s has been stuck for %v", pane, duration.Round(time.Second)),
			})
		}

	case ClassZombie:
		// Alert immediately for zombies
		alerts = append(alerts, Alert{
			Session:   m.session,
			Type:      AlertZombie,
			Pane:      pane,
			PID:       state.PID,
			State:     state.Classification,
			Duration:  duration,
			Timestamp: time.Now(),
			Message:   fmt.Sprintf("Agent %s is a zombie process", pane),
		})

	case ClassIdle:
		if duration >= m.idleThreshold {
			alerts = append(alerts, Alert{
				Session:   m.session,
				Type:      AlertIdle,
				Pane:      pane,
				PID:       state.PID,
				State:     state.Classification,
				Duration:  duration,
				Timestamp: time.Now(),
				Message:   fmt.Sprintf("Agent %s has been idle for %v", pane, duration.Round(time.Second)),
			})
		}
	}

	return alerts
}

// sendAlert logs an alert and delivers it to the registered callbacks (ntm
// serve publishes them on the event bus). There is deliberately no alert
// channel: nothing in production drained the old one, so after 100 alerts
// every later alert logged a spurious "channel full, dropping" warning.
func (m *HealthMonitor) sendAlert(alert Alert) {
	monitorLogger().Info("alert sent",
		"type", alert.Type,
		"pane", alert.Pane,
		"state", alert.State,
		"duration", alert.Duration,
	)
	m.emitAlert(alert)
}

func (m *HealthMonitor) emitStateChange(change ClassificationStateChange) {
	for _, cb := range m.stateChangeCallbacks {
		if cb == nil {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					monitorLogger().Error("state change callback panicked", "panic", r, "pane", change.Pane)
				}
			}()
			cb(change)
		}()
	}
}

func (m *HealthMonitor) emitAlert(alert Alert) {
	for _, cb := range m.alertCallbacks {
		if cb == nil {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					monitorLogger().Error("alert callback panicked", "panic", r, "pane", alert.Pane)
				}
			}()
			cb(alert)
		}()
	}
}

// Global singleton monitor

var (
	globalMonitor   *HealthMonitor
	globalMonitorMu sync.RWMutex
)

// GetGlobalMonitor returns the global health monitor: the one
// InitGlobalMonitor installed, else a default (unstarted) one. A sync.Once
// here used to replace a monitor InitGlobalMonitor had installed and started
// with a fresh unstarted one on the first read, so every reader saw no states.
func GetGlobalMonitor() *HealthMonitor {
	globalMonitorMu.Lock()
	defer globalMonitorMu.Unlock()
	if globalMonitor == nil {
		cfg := config.DefaultProcessTriageConfig()
		globalMonitor = NewHealthMonitor(&cfg)
	}
	return globalMonitor
}

// InitGlobalMonitor initializes the global monitor with the given config.
// This must be called before GetGlobalMonitor if custom config is desired.
func InitGlobalMonitor(cfg *config.ProcessTriageConfig, opts ...HealthMonitorOption) *HealthMonitor {
	globalMonitorMu.Lock()
	defer globalMonitorMu.Unlock()

	if globalMonitor != nil {
		globalMonitor.Stop()
	}

	globalMonitor = NewHealthMonitor(cfg, opts...)
	return globalMonitor
}
