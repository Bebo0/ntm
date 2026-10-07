package workflow

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type ErrorType string

const (
	ErrorAgentCrash    ErrorType = "agent_crash"
	ErrorAgentError    ErrorType = "agent_error"
	ErrorTriggerFailed ErrorType = "trigger_failed"
	ErrorTimeout       ErrorType = "timeout"
	ErrorValidation    ErrorType = "validation"
)

// WorkflowError is one error raised into a template's error_handling policy.
// AgentID is the faulted pane for agent_crash/agent_error and empty for
// stage-level errors (timeout, trigger_failed). Runners record raised errors
// in the checkpoint's errors list.
type WorkflowError struct {
	Type      ErrorType `json:"type"`
	Stage     string    `json:"stage"`
	AgentID   string    `json:"agent_id,omitempty"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
	Retryable bool      `json:"retryable,omitempty"`
}

func (e *WorkflowError) Error() string {
	return fmt.Sprintf("workflow %s at %s: %s", e.Type, e.Stage, e.Message)
}

type ErrorHandlingConfig struct {
	OnAgentCrash       ErrorAction `toml:"on_agent_crash"`
	OnAgentError       ErrorAction `toml:"on_agent_error"`
	OnTriggerFailed    ErrorAction `toml:"on_trigger_failed"`
	StageTimeoutMin    int         `toml:"stage_timeout_minutes"`
	OnTimeout          ErrorAction `toml:"on_timeout"`
	MaxRetriesPerStage int         `toml:"max_retries_per_stage"`
	// MaxRestartsPerAgent bounds restart_agent per agent for the handler's
	// lifetime (one workflow run). A fault past the budget aborts instead,
	// so a crash loop cannot restart an agent forever. Zero permits none.
	MaxRestartsPerAgent int `toml:"-"`
}

// WorkflowErrorActions is supplied by a coordinator or CLI adapter.
type WorkflowErrorActions interface {
	RestartAgent(context.Context, string) error
	Pause(context.Context, string) error
	SkipStage(context.Context) error
	Abort(context.Context, error) error
	RetryStage(context.Context) error
	Notify(context.Context, *WorkflowError, string) error
}
type ErrorHandler struct {
	config   ErrorHandlingConfig
	actions  WorkflowErrorActions
	mu       sync.Mutex
	retries  map[string]int // by stage
	restarts map[string]int // by agent
}

func NewErrorHandler(config ErrorHandlingConfig, actions WorkflowErrorActions) *ErrorHandler {
	return &ErrorHandler{config: config, actions: actions, retries: make(map[string]int), restarts: make(map[string]int)}
}
func (h *ErrorHandler) Handle(ctx context.Context, err *WorkflowError) error {
	if err == nil {
		return nil
	}
	if err.Timestamp.IsZero() {
		err.Timestamp = time.Now()
	}
	if h.actions == nil {
		return fmt.Errorf("workflow error actions are not configured")
	}
	action := h.action(err.Type)
	switch action {
	case ErrorActionRestartAgent:
		if err.AgentID == "" {
			return fmt.Errorf("restart_agent needs the faulted agent, but %s names none", err.Type)
		}
		h.mu.Lock()
		restarts := h.restarts[err.AgentID]
		if restarts < h.config.MaxRestartsPerAgent {
			h.restarts[err.AgentID] = restarts + 1
			h.mu.Unlock()
			return h.actions.RestartAgent(ctx, err.AgentID)
		}
		h.mu.Unlock()
		return h.actions.Abort(ctx, fmt.Errorf("agent %s exhausted its restart budget (%d restart(s)): %w", err.AgentID, h.config.MaxRestartsPerAgent, err))
	case ErrorActionPause:
		return h.actions.Pause(ctx, err.Message)
	case ErrorActionSkipStage:
		return h.actions.SkipStage(ctx)
	case ErrorActionAbort:
		return h.actions.Abort(ctx, err)
	case ErrorActionNotify:
		return h.actions.Notify(ctx, err, "workflow error")
	case ErrorActionRetry:
		h.mu.Lock()
		retries := h.retries[err.Stage]
		if retries < h.config.MaxRetriesPerStage {
			h.retries[err.Stage] = retries + 1
			h.mu.Unlock()
			return h.actions.RetryStage(ctx)
		}
		h.mu.Unlock()
		return h.actions.Abort(ctx, err)
	default:
		return fmt.Errorf("no error action configured for %s", err.Type)
	}
}
func (h *ErrorHandler) action(kind ErrorType) ErrorAction {
	switch kind {
	case ErrorAgentCrash:
		return h.config.OnAgentCrash
	case ErrorAgentError:
		return h.config.OnAgentError
	case ErrorTriggerFailed:
		return h.config.OnTriggerFailed
	case ErrorTimeout:
		return h.config.OnTimeout
	default:
		return ErrorActionAbort
	}
}

// TimeoutMonitor invokes its handler once when the named stage remains current at expiry.
type TimeoutMonitor struct {
	timeout time.Duration
	handler *ErrorHandler
	current func() string
	cancel  context.CancelFunc
	done    chan struct{}
}

func NewTimeoutMonitor(timeout time.Duration, handler *ErrorHandler, current func() string) *TimeoutMonitor {
	return &TimeoutMonitor{timeout: timeout, handler: handler, current: current}
}
func (m *TimeoutMonitor) Start(ctx context.Context, stage string) {
	m.StopAndWait()
	if m.timeout <= 0 || m.handler == nil || m.current == nil {
		return
	}
	run, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.done = make(chan struct{})
	done := m.done
	go func() {
		defer close(done)
		timer := time.NewTimer(m.timeout)
		defer timer.Stop()
		select {
		case <-run.Done():
			return
		case <-timer.C:
			if m.current() == stage {
				_ = m.handler.Handle(run, &WorkflowError{Type: ErrorTimeout, Stage: stage, Message: "stage timeout"})
			}
		}
	}()
}
func (m *TimeoutMonitor) Stop() {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
}

// StopAndWait drains any in-flight action before the runner releases its
// checkpoint lease. Start/StopAndWait are owner-goroutine operations and must
// not be called from the timeout action itself.
func (m *TimeoutMonitor) StopAndWait() {
	m.Stop()
	if m.done != nil {
		<-m.done
		m.done = nil
	}
}
