package pt

import (
	"bytes"
	"log/slog"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/config"
	"github.com/Dicklesworthstone/ntm/internal/tools"
)

func TestNewHealthMonitor(t *testing.T) {
	cfg := config.DefaultProcessTriageConfig()
	m := NewHealthMonitor(&cfg)

	if m == nil {
		t.Fatal("expected non-nil monitor")
	}
	if m.config == nil {
		t.Error("expected non-nil config")
	}
	if m.pidMap == nil {
		t.Error("expected non-nil pidMap")
	}
	if m.ptAdapter == nil {
		t.Error("expected non-nil ptAdapter")
	}
	if m.states == nil {
		t.Error("expected non-nil states map")
	}
	if m.running {
		t.Error("expected monitor not to be running initially")
	}
}

func TestHealthMonitorOptions(t *testing.T) {
	cfg := config.DefaultProcessTriageConfig()
	stateChangeCalls := 0
	alertCalls := 0

	m := NewHealthMonitor(&cfg,
		withSessionForTest("test-session"),
		WithStateChangeCallback(func(ClassificationStateChange) {
			stateChangeCalls++
		}),
		WithAlertCallback(func(Alert) {
			alertCalls++
		}),
		withRanoForTest(false),
	)

	if m.session != "test-session" {
		t.Errorf("expected session 'test-session', got %q", m.session)
	}
	if len(m.stateChangeCallbacks) != 1 {
		t.Errorf("expected 1 state change callback, got %d", len(m.stateChangeCallbacks))
	}
	if len(m.alertCallbacks) != 1 {
		t.Errorf("expected 1 alert callback, got %d", len(m.alertCallbacks))
	}
	if m.useRano {
		t.Error("expected useRano to be false")
	}
	if stateChangeCalls != 0 || alertCalls != 0 {
		t.Error("callbacks should not fire during monitor construction")
	}
}

func TestClassificationMapping(t *testing.T) {
	tests := []struct {
		name     string
		ptClass  string
		expected Classification
	}{
		{"useful maps to useful", "useful", ClassUseful},
		{"abandoned maps to stuck", "abandoned", ClassStuck},
		{"zombie maps to zombie", "zombie", ClassZombie},
		{"unknown maps to unknown", "unknown", ClassUnknown},
		{"empty maps to unknown", "", ClassUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Note: We can't directly test mapPTClassification as it takes tools.PTClassification
			// This is more of a documentation test
		})
	}
}

func TestAgentState(t *testing.T) {
	state := &AgentState{
		Pane:             "test__cc_1",
		PID:              12345,
		Classification:   ClassUseful,
		Confidence:       0.95,
		Since:            time.Now(),
		LastCheck:        time.Now(),
		History:          []ClassificationEvent{},
		ConsecutiveCount: 1,
	}

	if state.Pane != "test__cc_1" {
		t.Errorf("expected pane 'test__cc_1', got %q", state.Pane)
	}
	if state.PID != 12345 {
		t.Errorf("expected PID 12345, got %d", state.PID)
	}
	if state.Classification != ClassUseful {
		t.Errorf("expected classification useful, got %s", state.Classification)
	}
}

func TestAlert(t *testing.T) {
	alert := Alert{
		Type:      AlertStuck,
		Pane:      "test__cc_1",
		PID:       12345,
		State:     ClassStuck,
		Duration:  10 * time.Minute,
		Timestamp: time.Now(),
		Message:   "Agent test__cc_1 has been stuck for 10m0s",
	}

	if alert.Type != AlertStuck {
		t.Errorf("expected alert type stuck, got %s", alert.Type)
	}
	if alert.Pane != "test__cc_1" {
		t.Errorf("expected pane 'test__cc_1', got %q", alert.Pane)
	}
}

func TestGetAllStates(t *testing.T) {
	cfg := config.DefaultProcessTriageConfig()
	m := NewHealthMonitor(&cfg)

	states := m.GetAllStates()
	if len(states) != 0 {
		t.Errorf("expected 0 states, got %d", len(states))
	}
}

func TestRunningState(t *testing.T) {
	cfg := config.DefaultProcessTriageConfig()
	m := NewHealthMonitor(&cfg)

	if m.Running() {
		t.Error("expected monitor not to be running initially")
	}

	// Note: We can't easily test Start() without pt being available
	// This would require mocking the ptAdapter
}

func TestHealthMonitor_StartWaitsForConcurrentStop(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	cfg := config.DefaultProcessTriageConfig()
	m := NewHealthMonitor(&cfg)
	m.ptAdapter.InvalidateStatusCache()

	m.mu.Lock()
	m.running = true
	m.stopCh = make(chan struct{})
	m.doneCh = make(chan struct{})
	m.mu.Unlock()

	stopped := make(chan struct{})
	go func() {
		m.Stop()
		close(stopped)
	}()

	deadline := time.Now().Add(time.Second)
	for m.Running() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if m.Running() {
		t.Fatal("stop did not begin before timeout")
	}

	started := make(chan error, 1)
	go func() {
		started <- m.Start()
	}()

	select {
	case err := <-started:
		t.Fatalf("Start returned before Stop completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(m.doneCh)

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return after done channel closed")
	}

	select {
	case err := <-started:
		if err == nil {
			t.Fatal("expected Start to fail when pt is unavailable")
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not resume after Stop completed")
	}

	if m.Running() {
		t.Fatal("monitor should remain stopped after failed start")
	}
}

func TestGlobalMonitor(t *testing.T) {
	// Note: This modifies global state, so be careful
	m1 := GetGlobalMonitor()
	if m1 == nil {
		t.Fatal("expected non-nil global monitor")
	}

	// Getting global monitor again should return same instance
	m2 := GetGlobalMonitor()
	if m1 != m2 {
		t.Error("expected same global monitor instance")
	}
}

func TestInitGlobalMonitor(t *testing.T) {
	cfg := config.DefaultProcessTriageConfig()
	cfg.CheckInterval = 60 // Different from default

	m := InitGlobalMonitor(&cfg, withSessionForTest("custom-session"))
	if m == nil {
		t.Fatal("expected non-nil monitor")
	}
	if m.session != "custom-session" {
		t.Errorf("expected session 'custom-session', got %q", m.session)
	}
	if m.config.CheckInterval != 60 {
		t.Errorf("expected check interval 60, got %d", m.config.CheckInterval)
	}
}

// The monitor InitGlobalMonitor installs (and the caller starts) is the one
// every reader gets. A sync.Once in GetGlobalMonitor used to replace it with a
// fresh, unstarted monitor on the first read, so the dashboard and robot
// agent-health saw no process states even under `ntm serve`.
func TestGetGlobalMonitorReturnsTheInstalledMonitor(t *testing.T) {
	globalMonitorMu.Lock()
	previous := globalMonitor
	globalMonitor = nil
	globalMonitorMu.Unlock()
	t.Cleanup(func() {
		globalMonitorMu.Lock()
		globalMonitor = previous
		globalMonitorMu.Unlock()
	})

	cfg := config.DefaultProcessTriageConfig()
	installed := InitGlobalMonitor(&cfg, withSessionForTest("installed"))
	if got := GetGlobalMonitor(); got != installed {
		t.Fatal("GetGlobalMonitor replaced the monitor InitGlobalMonitor installed")
	}
}

func TestMapPTClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    tools.PTClassification
		expected Classification
	}{
		{"useful", tools.PTClassUseful, ClassUseful},
		{"abandoned_maps_to_stuck", tools.PTClassAbandoned, ClassStuck},
		{"zombie", tools.PTClassZombie, ClassZombie},
		{"unknown", tools.PTClassUnknown, ClassUnknown},
		{"empty_string_maps_to_unknown", tools.PTClassification(""), ClassUnknown},
		{"arbitrary_maps_to_unknown", tools.PTClassification("foobar"), ClassUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := mapPTClassification(tt.input)
			if got != tt.expected {
				t.Errorf("mapPTClassification(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestUpdateState(t *testing.T) {
	cfg := config.DefaultProcessTriageConfig()
	m := NewHealthMonitor(&cfg)
	now := time.Now()

	// Test creating new state
	event1 := ClassificationEvent{
		Classification: ClassUseful,
		Confidence:     0.95,
		Timestamp:      now,
		Reason:         "test reason",
		NetworkActive:  true,
	}

	m.mu.Lock()
	change1 := m.updateState("test__cc_1", 12345, event1)
	m.mu.Unlock()

	state := m.GetAllStates()["test__cc_1"]
	if state == nil {
		t.Fatal("expected non-nil state")
	}
	if state.Pane != "test__cc_1" {
		t.Errorf("expected pane 'test__cc_1', got %q", state.Pane)
	}
	if state.PID != 12345 {
		t.Errorf("expected PID 12345, got %d", state.PID)
	}
	if state.Classification != ClassUseful {
		t.Errorf("expected classification useful, got %s", state.Classification)
	}
	if state.ConsecutiveCount != 1 {
		t.Errorf("expected consecutive count 1, got %d", state.ConsecutiveCount)
	}
	if len(state.History) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(state.History))
	}
	if change1 == nil {
		t.Fatal("expected initial classification change")
	}
	if !change1.Initial {
		t.Error("expected initial change to be marked Initial")
	}
	if change1.Previous != ClassUnknown {
		t.Errorf("expected previous classification unknown, got %s", change1.Previous)
	}
	if change1.Current != ClassUseful {
		t.Errorf("expected current classification useful, got %s", change1.Current)
	}

	// Test updating with same classification (consecutive count increases)
	event2 := ClassificationEvent{
		Classification: ClassUseful,
		Confidence:     0.98,
		Timestamp:      now.Add(time.Second),
		Reason:         "still useful",
	}

	m.mu.Lock()
	change2 := m.updateState("test__cc_1", 12345, event2)
	m.mu.Unlock()

	state = m.GetAllStates()["test__cc_1"]
	if state.ConsecutiveCount != 2 {
		t.Errorf("expected consecutive count 2, got %d", state.ConsecutiveCount)
	}
	if state.Confidence != 0.98 {
		t.Errorf("expected confidence 0.98, got %f", state.Confidence)
	}
	if len(state.History) != 2 {
		t.Errorf("expected 2 history entries, got %d", len(state.History))
	}
	if change2 != nil {
		t.Fatal("expected no classification change when state repeats")
	}

	// Test updating with different classification (consecutive count resets)
	event3 := ClassificationEvent{
		Classification: ClassStuck,
		Confidence:     0.85,
		Timestamp:      now.Add(2 * time.Second),
		Reason:         "now stuck",
	}

	m.mu.Lock()
	change3 := m.updateState("test__cc_1", 12345, event3)
	m.mu.Unlock()

	state = m.GetAllStates()["test__cc_1"]
	if state.Classification != ClassStuck {
		t.Errorf("expected classification stuck, got %s", state.Classification)
	}
	if state.ConsecutiveCount != 1 {
		t.Errorf("expected consecutive count 1 after change, got %d", state.ConsecutiveCount)
	}
	if len(state.History) != 3 {
		t.Errorf("expected 3 history entries, got %d", len(state.History))
	}
	if change3 == nil {
		t.Fatal("expected classification change when state flips")
	}
	if change3.Initial {
		t.Error("expected non-initial change for later transition")
	}
	if change3.Previous != ClassUseful {
		t.Errorf("expected previous classification useful, got %s", change3.Previous)
	}
	if change3.Current != ClassStuck {
		t.Errorf("expected current classification stuck, got %s", change3.Current)
	}
}

func TestUpdateStateHistoryTrimming(t *testing.T) {
	cfg := config.DefaultProcessTriageConfig()
	m := NewHealthMonitor(&cfg)
	m.maxHistory = 5 // Set a small limit for testing

	now := time.Now()

	// Add more events than maxHistory
	for i := 0; i < 10; i++ {
		event := ClassificationEvent{
			Classification: ClassUseful,
			Confidence:     0.9,
			Timestamp:      now.Add(time.Duration(i) * time.Second),
			Reason:         "test",
		}
		m.mu.Lock()
		_ = m.updateState("test__cc_1", 12345, event)
		m.mu.Unlock()
	}

	state := m.GetAllStates()["test__cc_1"]
	if len(state.History) != 5 {
		t.Errorf("expected history to be trimmed to 5, got %d", len(state.History))
	}
}

func TestCheckAlertsStuck(t *testing.T) {
	cfg := config.DefaultProcessTriageConfig()
	cfg.StuckThreshold = 1 // 1 second for testing
	alertCh := make(chan Alert, 10)

	m := NewHealthMonitor(&cfg, withSessionForTest("test-session"), withAlertChannelForTest(alertCh))

	// Add a state that's been stuck for longer than threshold
	stuckSince := time.Now().Add(-5 * time.Second)
	m.mu.Lock()
	m.states["test__cc_1"] = &AgentState{
		Pane:           "test__cc_1",
		PID:            12345,
		Classification: ClassStuck,
		Since:          stuckSince,
		LastCheck:      time.Now(),
	}
	alerts := m.checkAlerts("test__cc_1")
	m.mu.Unlock()
	for _, alert := range alerts {
		m.sendAlert(alert)
	}

	select {
	case alert := <-alertCh:
		if alert.Type != AlertStuck {
			t.Errorf("expected alert type stuck, got %s", alert.Type)
		}
		if alert.Pane != "test__cc_1" {
			t.Errorf("expected pane 'test__cc_1', got %q", alert.Pane)
		}
		if alert.Session != "test-session" {
			t.Errorf("expected session 'test-session', got %q", alert.Session)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("expected stuck alert to be sent")
	}
}

func TestCheckAlertsZombie(t *testing.T) {
	cfg := config.DefaultProcessTriageConfig()
	alertCh := make(chan Alert, 10)

	m := NewHealthMonitor(&cfg, withAlertChannelForTest(alertCh))

	// Add a zombie state - should alert immediately
	m.mu.Lock()
	m.states["test__cc_1"] = &AgentState{
		Pane:           "test__cc_1",
		PID:            12345,
		Classification: ClassZombie,
		Since:          time.Now(),
		LastCheck:      time.Now(),
	}
	alerts := m.checkAlerts("test__cc_1")
	m.mu.Unlock()
	for _, alert := range alerts {
		m.sendAlert(alert)
	}

	select {
	case alert := <-alertCh:
		if alert.Type != AlertZombie {
			t.Errorf("expected alert type zombie, got %s", alert.Type)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("expected zombie alert to be sent immediately")
	}
}

func TestCheckAlertsIdle(t *testing.T) {
	cfg := config.DefaultProcessTriageConfig()
	cfg.IdleThreshold = 1 // 1 second for testing
	alertCh := make(chan Alert, 10)

	m := NewHealthMonitor(&cfg, withAlertChannelForTest(alertCh))

	// Add a state that's been idle for longer than threshold
	idleSince := time.Now().Add(-5 * time.Second)
	m.mu.Lock()
	m.states["test__cc_1"] = &AgentState{
		Pane:           "test__cc_1",
		PID:            12345,
		Classification: ClassIdle,
		Since:          idleSince,
		LastCheck:      time.Now(),
	}
	alerts := m.checkAlerts("test__cc_1")
	m.mu.Unlock()
	for _, alert := range alerts {
		m.sendAlert(alert)
	}

	select {
	case alert := <-alertCh:
		if alert.Type != AlertIdle {
			t.Errorf("expected alert type idle, got %s", alert.Type)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("expected idle alert to be sent")
	}
}

func TestCheckAlertsNoAlertBelowThreshold(t *testing.T) {
	cfg := config.DefaultProcessTriageConfig()
	cfg.StuckThreshold = 60 // 60 seconds threshold
	cfg.IdleThreshold = 120 // 120 seconds threshold
	alertCh := make(chan Alert, 10)

	m := NewHealthMonitor(&cfg, withAlertChannelForTest(alertCh))

	// Add a stuck state that's NOT past threshold
	m.mu.Lock()
	m.states["test__cc_1"] = &AgentState{
		Pane:           "test__cc_1",
		PID:            12345,
		Classification: ClassStuck,
		Since:          time.Now(), // Just started being stuck
		LastCheck:      time.Now(),
	}
	alerts := m.checkAlerts("test__cc_1")
	m.mu.Unlock()
	for _, alert := range alerts {
		m.sendAlert(alert)
	}

	select {
	case <-alertCh:
		t.Error("did not expect alert for state below threshold")
	case <-time.After(50 * time.Millisecond):
		// Good - no alert
	}
}

func TestCheckAlertsNonexistentPane(t *testing.T) {
	cfg := config.DefaultProcessTriageConfig()
	m := NewHealthMonitor(&cfg)

	// Should not panic for nonexistent pane
	m.mu.Lock()
	alerts := m.checkAlerts("nonexistent")
	m.mu.Unlock()
	if len(alerts) != 0 {
		t.Fatalf("expected no alerts, got %d", len(alerts))
	}
}

func TestStateChangeCallbackInvokedForInitialAndTransitionOnly(t *testing.T) {
	cfg := config.DefaultProcessTriageConfig()
	var changes []ClassificationStateChange
	m := NewHealthMonitor(&cfg, withSessionForTest("callback-session"), WithStateChangeCallback(func(change ClassificationStateChange) {
		changes = append(changes, change)
	}))

	now := time.Now()
	events := []ClassificationEvent{
		{
			Classification: ClassUseful,
			Confidence:     0.95,
			Timestamp:      now,
			Reason:         "initial",
		},
		{
			Classification: ClassUseful,
			Confidence:     0.99,
			Timestamp:      now.Add(time.Second),
			Reason:         "steady",
		},
		{
			Classification: ClassStuck,
			Confidence:     0.80,
			Timestamp:      now.Add(2 * time.Second),
			Reason:         "regressed",
		},
	}

	for _, event := range events {
		m.mu.Lock()
		change := m.updateState("test__cc_1", 12345, event)
		m.mu.Unlock()
		if change != nil {
			m.emitStateChange(*change)
		}
	}

	if len(changes) != 2 {
		t.Fatalf("expected 2 emitted state changes, got %d", len(changes))
	}
	if !changes[0].Initial {
		t.Error("expected first callback to be initial")
	}
	if changes[0].Session != "callback-session" {
		t.Errorf("expected callback session 'callback-session', got %q", changes[0].Session)
	}
	if changes[0].Previous != ClassUnknown || changes[0].Current != ClassUseful {
		t.Errorf("unexpected initial transition: %s -> %s", changes[0].Previous, changes[0].Current)
	}
	if changes[1].Initial {
		t.Error("expected second callback to be a real transition")
	}
	if changes[1].Previous != ClassUseful || changes[1].Current != ClassStuck {
		t.Errorf("unexpected transition: %s -> %s", changes[1].Previous, changes[1].Current)
	}
}

// TestSendAlertReachesEveryCallbackWithoutAQueue covers the delivery path ntm
// serve depends on. Alerts used to be pushed onto a 100-slot channel that
// nothing in production drained; past 100 alerts each one logged a "channel
// full, dropping alert" warning. Delivery is now callbacks only.
func TestSendAlertReachesEveryCallbackWithoutAQueue(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	cfg := config.DefaultProcessTriageConfig()
	var seen, also []Alert
	m := NewHealthMonitor(&cfg,
		WithAlertCallback(func(alert Alert) {
			seen = append(seen, alert)
		}),
		WithAlertCallback(func(alert Alert) {
			also = append(also, alert)
		}),
	)

	for i := 0; i < 150; i++ {
		m.sendAlert(Alert{Type: AlertIdle, Pane: "filler"})
	}
	alert := Alert{
		Session:   "callback-session",
		Type:      AlertStuck,
		Pane:      "test__cc_1",
		PID:       12345,
		State:     ClassStuck,
		Duration:  time.Minute,
		Timestamp: time.Now(),
		Message:   "test alert",
	}

	m.sendAlert(alert)

	if len(seen) != 151 || len(also) != 151 {
		t.Fatalf("expected every alert at both callbacks, got %d and %d", len(seen), len(also))
	}
	if last := seen[150]; last.Type != AlertStuck || last.Pane != "test__cc_1" {
		t.Fatalf("unexpected callback alert: %#v", last)
	}
	if logged.Len() != 0 {
		t.Fatalf("delivering alerts logged warnings:\n%s", logged.String())
	}
}

func TestGetAllStatesWithPopulatedStates(t *testing.T) {
	cfg := config.DefaultProcessTriageConfig()
	m := NewHealthMonitor(&cfg)

	// Populate some states
	now := time.Now()
	m.mu.Lock()
	m.states["test__cc_1"] = &AgentState{
		Pane:           "test__cc_1",
		PID:            12345,
		Classification: ClassUseful,
		Since:          now,
		LastCheck:      now,
	}
	m.states["test__cod_1"] = &AgentState{
		Pane:           "test__cod_1",
		PID:            12346,
		Classification: ClassWaiting,
		Since:          now,
		LastCheck:      now,
	}
	m.mu.Unlock()

	states := m.GetAllStates()
	if len(states) != 2 {
		t.Errorf("expected 2 states, got %d", len(states))
	}

	if states["test__cc_1"] == nil {
		t.Error("expected test__cc_1 state")
	}
	if states["test__cod_1"] == nil {
		t.Error("expected test__cod_1 state")
	}

	// Verify states are copies
	states["test__cc_1"].PID = 99999
	originalStates := m.GetAllStates()
	if originalStates["test__cc_1"].PID == 99999 {
		t.Error("GetAllStates should return copies, not originals")
	}
}

func TestClassificationConstants(t *testing.T) {
	// Verify classification string values
	tests := []struct {
		class    Classification
		expected string
	}{
		{ClassUseful, "useful"},
		{ClassWaiting, "waiting"},
		{ClassIdle, "idle"},
		{ClassStuck, "stuck"},
		{ClassZombie, "zombie"},
		{ClassUnknown, "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			if string(tt.class) != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, string(tt.class))
			}
		})
	}
}

func TestAlertTypeConstants(t *testing.T) {
	// Verify alert type string values
	tests := []struct {
		alertType AlertType
		expected  string
	}{
		{AlertStuck, "stuck"},
		{AlertZombie, "zombie"},
		{AlertIdle, "idle"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			if string(tt.alertType) != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, string(tt.alertType))
			}
		})
	}
}

func TestClassificationEventFields(t *testing.T) {
	now := time.Now()
	event := ClassificationEvent{
		Classification: ClassUseful,
		Confidence:     0.95,
		Timestamp:      now,
		Reason:         "test reason",
		NetworkActive:  true,
	}

	if event.Classification != ClassUseful {
		t.Errorf("expected classification useful, got %s", event.Classification)
	}
	if event.Confidence != 0.95 {
		t.Errorf("expected confidence 0.95, got %f", event.Confidence)
	}
	if event.Timestamp != now {
		t.Errorf("timestamp mismatch")
	}
	if event.Reason != "test reason" {
		t.Errorf("expected reason 'test reason', got %q", event.Reason)
	}
	if !event.NetworkActive {
		t.Error("expected NetworkActive to be true")
	}
}

// Test-local option helpers standing in for removed production options.
func withSessionForTest(session string) HealthMonitorOption {
	return func(m *HealthMonitor) { m.session = session }
}

// withAlertChannelForTest collects alerts through the production callback
// path (the monitor has no alert channel of its own).
func withAlertChannelForTest(ch chan Alert) HealthMonitorOption {
	return WithAlertCallback(func(alert Alert) {
		select {
		case ch <- alert:
		default:
		}
	})
}

func withRanoForTest(enabled bool) HealthMonitorOption {
	return func(m *HealthMonitor) { m.useRano = enabled }
}
