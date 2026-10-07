package context

import (
	"math"
	"sync"
	"testing"
	"time"
)

func forecastObservation(at time.Time, tokens int64) ForecastObservation {
	return ForecastObservation{Key: "%7", Generation: "process-41/transcript-a/model-a", Tokens: tokens, ContextLimit: 100000, ObservedAt: at}
}

func warmForecast(t *testing.T, f *ExhaustionForecaster, start time.Time) ExhaustionForecast {
	t.Helper()
	for i, tokens := range []int64{70000, 72000, 74000} {
		now := start.Add(time.Duration(i) * 30 * time.Second)
		got := f.Observe([]ForecastObservation{forecastObservation(now, tokens)}, now)
		if i < 2 {
			if len(got) != 0 {
				t.Fatalf("forecast before a minute of evidence: %+v", got)
			}
			continue
		}
		if len(got) != 1 {
			t.Fatalf("expected one forecast after three samples: %+v", got)
		}
		return got[0]
	}
	panic("unreachable")
}

func TestExhaustionForecasterObservedGrowth(t *testing.T) {
	start := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	f := NewExhaustionForecaster()
	got := warmForecast(t, f, start)
	if got.Level != "warning" || got.Key != "%7" || !got.ObservedAt.Equal(start.Add(time.Minute)) {
		t.Fatalf("incorrect identity/level/time: %+v", got)
	}
	p := got.Prediction
	if p.CurrentTokens != 74000 || p.ContextLimit != 100000 || p.TokenVelocity != 4000 ||
		p.MinutesToExhaustion != 6.5 || p.SampleCount != 3 || p.WindowDuration != 1 || !p.ShouldWarn || p.ShouldCompact {
		t.Fatalf("incorrect forecast: %+v", p)
	}
	// Urgency increases as the observed occupancy crosses 75%; the forecast
	// remains advisory, regardless of the predictor's ShouldCompact name.
	now := start.Add(90 * time.Second)
	escalated := f.Observe([]ForecastObservation{forecastObservation(now, 78000)}, now)
	if len(escalated) != 1 || escalated[0].Level != "urgent" {
		t.Fatalf("missing escalation: %+v", escalated)
	}
	now = start.Add(2 * time.Minute)
	if got := f.Observe([]ForecastObservation{forecastObservation(now, 80000)}, now); len(got) != 0 {
		t.Fatalf("same-risk repeat was not suppressed: %+v", got)
	}
}

func TestExhaustionForecasterEpochBoundaries(t *testing.T) {
	start := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for _, name := range []string{"process", "capacity", "compaction", "gap", "missing", "ambiguous", "stale", "future", "regressing", "conflicting_timestamp"} {
		t.Run(name, func(t *testing.T) {
			f := NewExhaustionForecaster()
			warmForecast(t, f, start)
			now := start.Add(90 * time.Second)
			observation := forecastObservation(now, 78000)
			expectRetained := true
			switch name {
			case "process":
				observation.Generation = "process-42/transcript-b/model-a"
			case "capacity":
				observation.ContextLimit = 200000
			case "compaction":
				observation.Tokens = 10000
			case "gap":
				now = start.Add(7 * time.Minute)
				observation.ObservedAt = now
			case "missing":
				f.Observe(nil, now)
			case "ambiguous":
				if got := f.Observe([]ForecastObservation{observation, observation}, now); len(got) != 0 {
					t.Fatalf("ambiguous reading generated a forecast: %+v", got)
				}
			case "stale":
				observation.ObservedAt = now.Add(-61 * time.Second)
				expectRetained = false
			case "future":
				observation.ObservedAt = now.Add(time.Second)
				expectRetained = false
			case "regressing":
				observation.ObservedAt = start.Add(59 * time.Second)
				expectRetained = false
			case "conflicting_timestamp":
				observation.ObservedAt = start.Add(time.Minute)
				expectRetained = false
			}
			if got := f.Observe([]ForecastObservation{observation}, now); len(got) != 0 {
				t.Fatalf("forecast bridged %s: %+v", name, got)
			}
			history := f.histories[observation.Key]
			if expectRetained {
				if history == nil || history.predictor.SampleCount() != 1 {
					t.Fatalf("new epoch must start with one observation: %+v", history)
				}
			} else if history != nil {
				t.Fatalf("untrustworthy observation retained history: %+v", history)
			}
		})
	}
}

func TestExhaustionForecasterCachedAndDenseObservations(t *testing.T) {
	start := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	f := NewExhaustionForecaster()
	cached := forecastObservation(start, 72000)
	for i := range 12 {
		now := start.Add(time.Duration(i) * 5 * time.Second)
		if got := f.Observe([]ForecastObservation{cached}, now); len(got) != 0 {
			t.Fatalf("cached record manufactured a prediction: %+v", got)
		}
	}
	if count := f.histories[cached.Key].predictor.SampleCount(); count != 1 {
		t.Fatalf("cached record manufactured %d samples", count)
	}
	f = NewExhaustionForecaster()
	for i := range 12 {
		now := start.Add(time.Duration(i) * 5 * time.Second)
		if got := f.Observe([]ForecastObservation{forecastObservation(now, 70000+int64(i)*500)}, now); len(got) != 0 {
			t.Fatalf("sub-minute burst generated a prediction: %+v", got)
		}
	}
	if count := f.histories[cached.Key].predictor.SampleCount(); count != 2 {
		t.Fatalf("dense polling contributed %d samples, want 2", count)
	}
	// A decrease between accepted sample intervals must still reset the trend.
	now := start.Add(56 * time.Second)
	f.Observe([]ForecastObservation{forecastObservation(now, 10000)}, now)
	if count := f.histories[cached.Key].predictor.SampleCount(); count != 1 {
		t.Fatalf("unsampled compaction retained old history: %d", count)
	}
}

func TestExhaustionForecasterInvalidInputs(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for _, name := range []string{"key", "generation", "negative_tokens", "capacity", "over_capacity", "timestamp"} {
		t.Run(name, func(t *testing.T) {
			observation := forecastObservation(now, 74000)
			switch name {
			case "key":
				observation.Key = ""
			case "generation":
				observation.Generation = ""
			case "negative_tokens":
				observation.Tokens = -1
			case "capacity":
				observation.ContextLimit = 0
			case "over_capacity":
				observation.Tokens = 100001
			case "timestamp":
				observation.ObservedAt = time.Time{}
			}
			f := NewExhaustionForecaster()
			if got := f.Observe([]ForecastObservation{observation}, now); len(got) != 0 || len(f.histories) != 0 {
				t.Fatalf("invalid input admitted: %+v, histories=%d", got, len(f.histories))
			}
		})
	}
}

func TestExhaustionForecasterIndependentPanesAndPruning(t *testing.T) {
	start := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	f := NewExhaustionForecaster()
	for i := range 3 {
		now := start.Add(time.Duration(i) * 30 * time.Second)
		a := forecastObservation(now, 70000+int64(i)*2000)
		b := a
		b.Key = "%2"
		b.Generation = "different process"
		got := f.Observe([]ForecastObservation{a, b}, now)
		if i == 2 && (len(got) != 2 || got[0].Key != "%2" || got[1].Key != "%7") {
			t.Fatalf("forecasts are not independent and sorted: %+v", got)
		}
	}
	f.Observe(nil, start.Add(2*time.Minute))
	if len(f.histories) != 0 {
		t.Fatalf("removed panes retained history: %d", len(f.histories))
	}
}

func TestExhaustionForecasterReminderAndRearm(t *testing.T) {
	start := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	f := NewExhaustionForecaster()
	warmForecast(t, f, start)
	// A sustained 1,000-token/30-second growth trajectory stays urgent while
	// the sliding window expires its older observations. Remind after 5m.
	var times []time.Time
	for i := 3; i <= 20; i++ {
		now := start.Add(time.Duration(i) * 30 * time.Second)
		got := f.Observe([]ForecastObservation{forecastObservation(now, 74000+int64(i-2)*1000)}, now)
		if len(got) > 0 {
			times = append(times, now)
		}
	}
	if len(times) != 2 || times[1].Sub(times[0]) < 5*time.Minute {
		t.Fatalf("unexpected escalation/reminder schedule: %v", times)
	}
	// Compaction is a new epoch and can produce a new warning after enough
	// fresh growth evidence; it must not inherit the prior episode's silence.
	newStart := start.Add(11 * time.Minute)
	got := warmForecast(t, f, newStart)
	if got.Level != "warning" {
		t.Fatalf("warning not rearmed: %+v", got)
	}
}

func TestPredictExhaustionAtExcludesFutureSamples(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	p := NewContextPredictor(DefaultPredictorConfig())
	p.AddSampleAt(70000, now.Add(-time.Minute))
	p.AddSampleAt(72000, now.Add(-30*time.Second))
	p.AddSampleAt(99000, now.Add(time.Minute))
	if got := p.PredictExhaustionAt(100000, now); got != nil {
		t.Fatalf("future sample affected prediction: %+v", got)
	}
	p.AddSampleAt(74000, now)
	got := p.PredictExhaustionAt(100000, now)
	if got == nil || got.CurrentTokens != 74000 || math.Abs(got.TokenVelocity-4000) > 1e-9 {
		t.Fatalf("historical prediction incorrect: %+v", got)
	}
}

func TestExhaustionForecasterConcurrentObservations(t *testing.T) {
	f := NewExhaustionForecaster()
	now := time.Now()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range 50 {
				at := now.Add(time.Duration(i+j) * time.Second)
				f.Observe([]ForecastObservation{forecastObservation(at, 74000+int64(i+j))}, at)
			}
		}(i)
	}
	wg.Wait()
}
