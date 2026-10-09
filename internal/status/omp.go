package status

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

// ObserveOMP reads authenticated native state. Terminal content and window
// activity cannot establish whether an OMP turn is running.
func ObserveOMP(ctx context.Context, pane tmux.Pane, now time.Time) StateObservation {
	data, err := tmux.OMPControlContext(ctx, pane.ID, "/state", nil)
	return observeOMPData(pane, data, err, now)
}

func observeOMPData(pane tmux.Pane, data []byte, readErr error, now time.Time) StateObservation {
	result := StateObservation{
		Status:     AgentStatus{PaneID: pane.ID, PaneName: pane.Title, AgentType: "omp", State: StateUnknown, UpdatedAt: now},
		ObservedAt: now, Freshness: FreshnessUnavailable,
	}
	var state struct {
		Ready        bool      `json:"ready"`
		ObservedAt   time.Time `json:"observed_at"`
		LastActivity time.Time `json:"last_activity"`
		Idle         *bool     `json:"idle"`
		Pending      *bool     `json:"pending"`
		Draft        *bool     `json:"draft"`
		Tools        *int      `json:"tools"`
		AsyncBusy    *bool     `json:"async_busy"`
	}
	err := readErr
	if err == nil {
		err = json.Unmarshal(data, &state)
	}
	if err == nil && (!state.Ready || state.Idle == nil || state.Pending == nil || state.Draft == nil || state.Tools == nil || state.AsyncBusy == nil || *state.Tools < 0) {
		err = fmt.Errorf("OMP native state is incomplete or not ready")
	}
	if err == nil && (state.ObservedAt.IsZero() || now.Sub(state.ObservedAt) > 10*time.Second || state.ObservedAt.Sub(now) > time.Second) {
		err = fmt.Errorf("OMP native state is stale or has an invalid timestamp")
	}
	if err != nil {
		result.Error = err.Error()
	} else {
		result.ObservedAt = state.ObservedAt
		result.Status.UpdatedAt = state.ObservedAt
		result.Status.LastActive = state.LastActivity
		result.Status.State = StateWorking
		if *state.Idle && !*state.Pending && *state.Tools == 0 && !*state.AsyncBusy {
			result.Status.State = StateIdle
		}
		result.Status.HasDraft = *state.Draft
		result.Freshness = FreshnessFresh
		result.Confidence = 1
	}
	result.Evidence = []ObservationEvidence{{Provenance: ProvenanceOMPNative, ObservedAt: result.ObservedAt, Freshness: result.Freshness, Confidence: result.Confidence, Error: result.Error}}
	return result
}
