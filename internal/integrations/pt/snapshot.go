package pt

import (
	"context"
	"errors"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/config"
	"github.com/Dicklesworthstone/ntm/internal/integrations/rano"
)

// SampleSession obtains one passive health snapshot for a named tmux session.
// It uses the same sampler as the resident monitor, but does not start it,
// mutate the global monitor, emit callbacks, or persist observations. Each
// result begins a new observation history, not a claim about prior duration.
// Rano enrichment is left to the resident monitor; a standalone health read
// must not open or migrate an observer database as an incidental side effect.
// PT's own configuration, safety filters and threshold semantics still apply.
func SampleSession(ctx context.Context, session string) (map[string]*AgentState, error) {
	if ctx == nil {
		return nil, errors.New("PT snapshot requires a context")
	}
	if session == "" {
		return nil, errors.New("PT snapshot requires a named session")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cfg := config.DefaultProcessTriageConfig()
	cfg.UseRanoData = false
	monitor := NewHealthMonitor(&cfg)
	monitor.session = session
	monitor.pidMap = rano.NewPIDMap(session)
	if err := monitor.sample(ctx); err != nil {
		return nil, err
	}
	return monitor.GetAllStates(), nil
}
