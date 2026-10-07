package context

import (
	"sort"
	"sync"
	"time"
)

// ForecastObservation is one fresh occupancy reading, not cumulative billing
// usage. Key identifies the pane; Generation pins its process, transcript,
// model and ownership identity. Repeated cached readings are not new samples.
type ForecastObservation struct {
	Key          string
	Generation   string
	Tokens       int64
	ContextLimit int64
	ObservedAt   time.Time
}

// ExhaustionForecast is an advisory emitted when observed growth first meets
// the predictor's warning thresholds, escalates, or remains risky for a full
// window. It grants no permission to compact, interrupt, or replace a pane.
type ExhaustionForecast struct {
	Key        string
	Level      string
	ObservedAt time.Time
	Prediction Prediction
}

type forecastHistory struct {
	generation string
	limit      int64
	last       ForecastObservation
	sampledAt  time.Time
	predictor  *ContextPredictor
	level      int
	reportedAt time.Time
}

// ExhaustionForecaster feeds the existing velocity predictor with bounded,
// generation-local observations. State belongs to the resident observer;
// restarting it requires fresh evidence rather than inventing a prior trend.
type ExhaustionForecaster struct {
	mu        sync.Mutex
	config    PredictorConfig
	histories map[string]*forecastHistory
}

// NewExhaustionForecaster starts an empty observer using the predictor's
// standard sampling window and advisory thresholds.
func NewExhaustionForecaster() *ExhaustionForecaster {
	return &ExhaustionForecaster{
		config:    DefaultPredictorConfig(),
		histories: make(map[string]*forecastHistory),
	}
}

// Observe consumes a complete observation pass. Missing, ambiguous, stale,
// invalid, or regressing observations discard their history. Compaction,
// changed generation/window, and observation gaps start a new baseline.
// At most one sample per polling interval contributes, and at least two
// intervals must be observed before a trend is reported.
func (f *ExhaustionForecaster) Observe(observations []ForecastObservation, now time.Time) []ExhaustionForecast {
	f.mu.Lock()
	defer f.mu.Unlock()

	counts := make(map[string]int, len(observations))
	for _, observation := range observations {
		counts[observation.Key]++
	}
	for key := range f.histories {
		if counts[key] != 1 {
			delete(f.histories, key)
		}
	}

	var forecasts []ExhaustionForecast
	for _, observation := range observations {
		if counts[observation.Key] != 1 {
			continue
		}
		if forecast := f.observe(observation, now); forecast != nil {
			forecasts = append(forecasts, *forecast)
		}
	}
	sort.Slice(forecasts, func(i, j int) bool { return forecasts[i].Key < forecasts[j].Key })
	return forecasts
}

func (f *ExhaustionForecaster) observe(observation ForecastObservation, now time.Time) *ExhaustionForecast {
	if observation.Key == "" || observation.Generation == "" || observation.ContextLimit <= 0 ||
		observation.Tokens < 0 || observation.Tokens > observation.ContextLimit ||
		observation.ObservedAt.IsZero() || observation.ObservedAt.After(now) ||
		now.Sub(observation.ObservedAt) > 2*f.config.PollInterval {
		delete(f.histories, observation.Key)
		return nil
	}

	history := f.histories[observation.Key]
	if history != nil && (history.generation != observation.Generation || history.limit != observation.ContextLimit) {
		history = nil
	}
	if history != nil {
		if !observation.ObservedAt.After(history.last.ObservedAt) {
			// An identical cached record adds no evidence. A conflicting or
			// out-of-order one breaks the observation chain entirely.
			if !observation.ObservedAt.Equal(history.last.ObservedAt) || observation.Tokens != history.last.Tokens {
				delete(f.histories, observation.Key)
			}
			return nil
		}
		if observation.Tokens < history.last.Tokens || observation.ObservedAt.Sub(history.last.ObservedAt) > f.config.Window {
			history = nil
		}
	}
	if history == nil {
		history = &forecastHistory{
			generation: observation.Generation,
			limit:      observation.ContextLimit,
			predictor:  NewContextPredictor(f.config),
		}
		f.histories[observation.Key] = history
	}
	history.last = observation
	if !history.sampledAt.IsZero() && observation.ObservedAt.Sub(history.sampledAt) < f.config.PollInterval {
		return nil
	}
	history.sampledAt = observation.ObservedAt
	history.predictor.AddSampleAt(observation.Tokens, observation.ObservedAt)
	prediction := history.predictor.PredictExhaustionAt(observation.ContextLimit, now)
	if prediction == nil || prediction.WindowDuration < 2*f.config.PollInterval.Minutes() {
		return nil
	}

	level, name := 0, ""
	if prediction.ShouldWarn {
		level, name = 1, "warning"
	}
	if prediction.ShouldCompact {
		level, name = 2, "urgent"
	}
	if level == 0 {
		history.level = 0
		return nil
	}
	if level <= history.level && now.Sub(history.reportedAt) < f.config.Window {
		return nil
	}
	history.level, history.reportedAt = level, now
	return &ExhaustionForecast{
		Key: observation.Key, Level: name, ObservedAt: observation.ObservedAt,
		Prediction: *prediction,
	}
}
