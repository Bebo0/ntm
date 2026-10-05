package assign

import (
	"math"
	"reflect"
	"testing"

	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

func TestBalancedAssignmentFiltersConfidenceBeforeLoad(t *testing.T) {
	for _, score := range []float64{0.1, math.NaN()} {
		for _, reverse := range []bool{false, true} {
			m := &Matcher{
				matrix: NewCapabilityMatrix(),
				config: DefaultMatcherConfig(),
			}
			m.matrix.base[tmux.AgentClaude][TaskFeature] = score
			m.matrix.base[tmux.AgentCodex][TaskFeature] = 0.9
			agents := []Agent{
				{ID: "weak", AgentType: tmux.AgentClaude, Idle: true},
				{ID: "qualified", AgentType: tmux.AgentCodex, Idle: true, Assignments: 5},
			}
			if reverse {
				agents[0], agents[1] = agents[1], agents[0]
			}
			beads := []Bead{
				{ID: "feature-1", TaskType: TaskFeature},
				{ID: "feature-2", TaskType: TaskFeature},
				{ID: "feature-3", TaskType: TaskFeature},
			}
			before := append([]Agent(nil), agents...)
			got := m.AssignTasks(beads, agents, StrategyBalanced)
			if len(got) != len(beads) {
				t.Fatalf("score=%v reverse=%v: got %d assignments, want %d", score, reverse, len(got), len(beads))
			}
			for _, assignment := range got {
				if assignment.Agent.ID != "qualified" || assignment.Score != 0.9 {
					t.Fatalf("ineligible agent selected: %+v", assignment)
				}
			}
			if !reflect.DeepEqual(agents, before) {
				t.Fatal("assignment mutated caller's agents")
			}
		}
	}
}

func TestBalancedAssignmentPreservesFairnessAmongEligibleAgents(t *testing.T) {
	m := &Matcher{matrix: NewCapabilityMatrix(), config: DefaultMatcherConfig()}
	m.matrix.base[tmux.AgentClaude][TaskFeature] = m.config.MinConfidence
	m.matrix.base[tmux.AgentCodex][TaskFeature] = 0.9
	agents := []Agent{
		{ID: "boundary", AgentType: tmux.AgentClaude, Idle: true},
		{ID: "strong", AgentType: tmux.AgentCodex, Idle: true, Assignments: 1},
	}
	got := m.AssignTasks([]Bead{
		{ID: "one", TaskType: TaskFeature},
		{ID: "two", TaskType: TaskFeature},
		{ID: "three", TaskType: TaskFeature},
	}, agents, StrategyBalanced)
	var ids []string
	for _, assignment := range got {
		ids = append(ids, assignment.Agent.ID)
	}
	if want := []string{"boundary", "strong", "boundary"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("got %v, want %v (confidence boundary remains eligible)", ids, want)
	}
}

func TestBalancedAssignmentStillRejectsUnavailableAndLowConfidenceAgents(t *testing.T) {
	m := &Matcher{matrix: NewCapabilityMatrix(), config: DefaultMatcherConfig()}
	agents := []Agent{
		{ID: "busy", AgentType: tmux.AgentCodex, Idle: false},
		{ID: "full", AgentType: tmux.AgentCodex, Idle: true, ContextUsage: 0.95},
		{ID: "low-score", AgentType: tmux.AgentCodex, Idle: true, ContextUsage: 0.8},
	}
	got := m.AssignTasks([]Bead{{ID: "one", TaskType: TaskFeature}}, agents, StrategyBalanced)
	if len(got) != 0 {
		t.Fatalf("expected no eligible agents, got %+v", got)
	}
}
