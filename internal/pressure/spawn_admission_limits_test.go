package pressure

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestSpawnAdmissionAgentTypeLimits(t *testing.T) {
	for _, tc := range []struct {
		name               string
		running, requested map[string]int
		limits             map[string]int
		want               SpawnAdmissionDecision
		reason             string
	}{
		{"cannot borrow other type capacity", map[string]int{"claude": 2}, map[string]int{"claude": 1}, map[string]int{"claude": 2, "codex": 10}, SpawnAdmissionRefuse, "agent_type_limit_exceeded"},
		{"exact boundary", map[string]int{"claude": 1}, map[string]int{"claude": 1}, map[string]int{"claude": 2}, SpawnAdmissionAdmit, "headroom_available"},
		{"unrequested type already above cap", map[string]int{"claude": 4}, map[string]int{"codex": 1}, map[string]int{"claude": 2, "codex": 3}, SpawnAdmissionAdmit, "headroom_available"},
		{"zero disables only that type limit", map[string]int{"claude": 4}, map[string]int{"claude": 3}, map[string]int{"claude": 0}, SpawnAdmissionAdmit, "headroom_available"},
		{"unconfigured type", map[string]int{"omp": 2}, map[string]int{"omp": 3}, map[string]int{"claude": 1}, SpawnAdmissionAdmit, "headroom_available"},
		{"mixed batch is refused as a whole", map[string]int{"codex": 1}, map[string]int{"claude": 1, "codex": 2}, map[string]int{"claude": 3, "codex": 2}, SpawnAdmissionRefuse, "agent_type_limit_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			running, _ := sumSpawnCounts(tc.running)
			requested, _ := sumSpawnCounts(tc.requested)
			in := SpawnAdmissionInput{RequestedAgents: requested, RequestedPanes: requested + 1, RunningAgents: running,
				MaxAgents: 100, RunningByType: tc.running, RequestedByType: tc.requested, MaxAgentsByType: tc.limits}
			before, _ := json.Marshal(in)
			got := EvaluateSpawnAdmission(in)
			if got.Decision != tc.want || got.Reason != tc.reason || !got.AgentInventoryAvailable {
				t.Fatalf("admission = %+v", got)
			}
			after, _ := json.Marshal(in)
			if string(before) != string(after) {
				t.Fatal("admission mutated input maps")
			}
		})
	}
}

func TestSpawnAdmissionAgentTypeEvidenceIsSortedAndDetached(t *testing.T) {
	in := SpawnAdmissionInput{RequestedAgents: 5, RequestedPanes: 6, RunningAgents: 3,
		MaxAgents: 100, RequestedByType: map[string]int{"codex": 2, "claude": 3},
		RunningByType: map[string]int{"codex": 2, "claude": 1}, MaxAgentsByType: map[string]int{"codex": 3, "claude": 3}}
	want := []SpawnAdmissionAgentLimit{
		{AgentType: "claude", Running: 1, Requested: 3, Projected: 4, Limit: 3, Headroom: 0, BlocksRequest: true},
		{AgentType: "codex", Running: 2, Requested: 2, Projected: 4, Limit: 3, Headroom: 0, BlocksRequest: true},
	}
	var first string
	for i := 0; i < 100; i++ {
		out := EvaluateSpawnAdmission(in)
		if !reflect.DeepEqual(out.AgentTypeLimits, want) || !strings.Contains(out.Hint, "claude: running 1 + requested 3") {
			t.Fatalf("incomplete or unordered refusal: %+v", out)
		}
		encoded, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = string(encoded)
		} else if string(encoded) != first {
			t.Fatal("non-deterministic receipt")
		}
	}
	out := EvaluateSpawnAdmission(in)
	in.MaxAgentsByType["claude"] = 999
	in.RunningByType["claude"] = 0
	if !reflect.DeepEqual(out.AgentTypeLimits, want) {
		t.Fatal("receipt aliases input")
	}
}

func TestSpawnAdmissionInventoryFailureNeverBecomesEmptyFleet(t *testing.T) {
	for _, tc := range []struct {
		name   string
		max    int
		limits map[string]int
	}{
		{"global cap", 10, nil}, {"type cap", 0, map[string]int{"claude": 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := EvaluateSpawnAdmission(SpawnAdmissionInput{RequestedAgents: 1, RequestedPanes: 2, MaxAgents: tc.max,
				MaxAgentsByType: tc.limits, RequestedByType: map[string]int{"claude": 1},
				RunningByType: map[string]int{}, InventoryError: "tmux inventory timed out"})
			if out.Decision != SpawnAdmissionDefer || out.Reason != "agent_inventory_unavailable" || out.AgentInventoryAvailable ||
				out.AgentInventoryError != "tmux inventory timed out" || len(out.AgentTypeLimits) != 0 {
				t.Fatalf("unknown fleet authorized or published as checked empty: %+v", out)
			}
		})
	}
	out := EvaluateSpawnAdmission(SpawnAdmissionInput{RequestedAgents: 1, RequestedPanes: 2, InventoryError: "offline"})
	if out.Decision != SpawnAdmissionAdmit || out.AgentInventoryAvailable || out.AgentInventoryError == "" {
		t.Fatal("uncapped behavior changed or inventory error hidden")
	}
}

func TestSpawnAdmissionRequiresCompleteTypeCountEvidence(t *testing.T) {
	for _, tc := range []struct {
		name               string
		requested, running map[string]int
		reason             string
	}{
		{"missing request", nil, map[string]int{}, "invalid_request"},
		{"partial request", map[string]int{"codex": 1}, map[string]int{}, "invalid_request"},
		{"negative request", map[string]int{"claude": 3, "codex": -1}, map[string]int{}, "invalid_request"},
		{"empty request type", map[string]int{"": 2}, map[string]int{}, "invalid_request"},
		{"missing inventory", map[string]int{"claude": 2}, nil, "agent_inventory_unavailable"},
		{"inconsistent inventory", map[string]int{"claude": 2}, map[string]int{"claude": 1}, "agent_inventory_unavailable"},
		{"negative inventory", map[string]int{"claude": 2}, map[string]int{"claude": -1}, "agent_inventory_unavailable"},
		{"checked empty", map[string]int{"claude": 2}, map[string]int{}, "headroom_available"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := EvaluateSpawnAdmission(SpawnAdmissionInput{RequestedAgents: 2, RequestedPanes: 3,
				RequestedByType: tc.requested, RunningByType: tc.running, MaxAgentsByType: map[string]int{"claude": 3}})
			if out.Reason != tc.reason {
				t.Fatalf("admission = %+v", out)
			}
			if tc.reason != "headroom_available" && out.Decision == SpawnAdmissionAdmit {
				t.Fatal("incomplete evidence authorized spawn")
			}
		})
	}
}

func TestSpawnAdmissionCountOverflowCannotBypassCaps(t *testing.T) {
	maxValue := int(^uint(0) >> 1)
	for _, typeCap := range []bool{false, true} {
		in := SpawnAdmissionInput{RequestedAgents: 1, RequestedPanes: 1, RunningAgents: maxValue, MaxAgents: maxValue,
			CurrentPanes: maxValue, RequestedByType: map[string]int{"claude": 1}, RunningByType: map[string]int{"claude": maxValue}}
		want := "agent_limit_exceeded"
		if typeCap {
			in.MaxAgents = 0
			in.MaxAgentsByType = map[string]int{"claude": maxValue}
			want = "agent_type_limit_exceeded"
		}
		out := EvaluateSpawnAdmission(in)
		if out.Decision != SpawnAdmissionRefuse || out.Reason != want || out.ProjectedPanes != maxValue || out.AgentHeadroom < 0 {
			t.Fatalf("overflow bypassed cap or corrupted receipt: %+v", out)
		}
	}
	if _, ok := sumSpawnCounts(map[string]int{"claude": maxValue, "codex": 1}); ok {
		t.Fatal("overflowed count map accepted")
	}
}

func TestSpawnAdmissionPreservesSharedBudgetAndPressure(t *testing.T) {
	in := SpawnAdmissionInput{RequestedAgents: 2, RequestedPanes: 3, RunningAgents: 2, MaxAgents: 3,
		RequestedByType: map[string]int{"claude": 2}, RunningByType: map[string]int{"codex": 2}, MaxAgentsByType: map[string]int{"claude": 9}}
	if out := EvaluateSpawnAdmission(in); out.Reason != "agent_limit_exceeded" {
		t.Fatalf("per-type caps replaced shared budget: %+v", out)
	}
	in.MaxAgents, in.LargeSpawnThreshold = 100, 2
	for _, level := range []Level{LevelHigh, LevelCritical} {
		in.Pressure = Snapshot{Overall: level, Limiting: []Source{SourceMemory}}
		out := EvaluateSpawnAdmission(in)
		if out.Reason != "pressure_"+level.String() || out.Decision == SpawnAdmissionAdmit {
			t.Fatalf("per-type caps bypassed pressure: %+v", out)
		}
	}
	in.MaxAgentsByType = map[string]int{"claude": -1}
	if out := EvaluateSpawnAdmission(in); out.Reason != "invalid_agent_type_limits" {
		t.Fatal("negative type limit accepted")
	}
}

func TestSpawnAdmissionSmallFleetMatchesIndependentCountOracle(t *testing.T) {
	// Exhaustive two-type fleets. This oracle uses only small integer sums;
	// it does not call any production count or eligibility helper.
	for runningA := 0; runningA <= 3; runningA++ {
		for runningB := 0; runningB <= 3; runningB++ {
			for requestedA := 0; requestedA <= 3; requestedA++ {
				for requestedB := 0; requestedB <= 3; requestedB++ {
					if requestedA+requestedB == 0 {
						continue
					}
					for capA := 0; capA <= 3; capA++ {
						for capB := 0; capB <= 3; capB++ {
							for totalCap := 0; totalCap <= 10; totalCap++ {
								wantAdmit := (totalCap == 0 || runningA+runningB+requestedA+requestedB <= totalCap) &&
									(capA == 0 || requestedA == 0 || runningA+requestedA <= capA) &&
									(capB == 0 || requestedB == 0 || runningB+requestedB <= capB)
								in := SpawnAdmissionInput{RequestedAgents: requestedA + requestedB, RequestedPanes: requestedA + requestedB + 1,
									RunningAgents: runningA + runningB, MaxAgents: totalCap,
									RequestedByType: map[string]int{"claude": requestedA, "codex": requestedB},
									RunningByType:   map[string]int{"claude": runningA, "codex": runningB},
									MaxAgentsByType: map[string]int{"claude": capA, "codex": capB}}
								out := EvaluateSpawnAdmission(in)
								if (out.Decision == SpawnAdmissionAdmit) != wantAdmit {
									t.Fatalf("oracle mismatch input=%+v got=%+v want_admit=%t", in, out, wantAdmit)
								}
							}
						}
					}
				}
			}
		}
	}
}
