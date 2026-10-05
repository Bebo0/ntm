package assign

import (
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"testing"
)

func TestPriorityBatchRepairsGreedyStranding(t *testing.T) {
	edges := []priorityBatchEdge{
		{"flexible", "specialist", .95},
		{"flexible", "generalist", .8},
		{"restricted", "specialist", .9},
	}
	if got, want := selectPriorityBatch(edges), []int{1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v: move the flexible task, fill both workers", got, want)
	}
}

func TestPriorityBatchOptimizesScoreWithoutTradingPriority(t *testing.T) {
	edges := []priorityBatchEdge{
		{"critical", "a", .9},
		{"critical", "b", .8},
		{"next", "a", .85},
		{"next", "b", .3},
		{"later", "a", 1},
		{"later", "b", 1},
	}
	if got, want := selectPriorityBatch(edges), []int{1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v: maximize the admitted pair, not the later task", got, want)
	}
}

func TestPriorityBatchIdentityAndInvalidScores(t *testing.T) {
	edges := []priorityBatchEdge{
		{"invalid", "c", math.NaN()},
		{"invalid", "c", math.Inf(1)},
		{"invalid", "c", -.1},
		{"invalid", "c", 1.1},
		{"first", "a", .8},
		{"first", "a", .9},
		{"first", "b", .7},
		{"second", "b", 0},
	}
	if got, want := selectPriorityBatch(edges), []int{5, 7}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v: duplicate IDs are not extra capacity", got, want)
	}
	if got := selectPriorityBatch(nil); got != nil {
		t.Fatalf("empty candidates: got %v, want nil", got)
	}
	if got := selectPriorityBatch(edges[:4]); got != nil {
		t.Fatalf("invalid candidates: got %v, want nil", got)
	}
}

func TestPriorityBatchRetainsSubThousandthScoreDifferences(t *testing.T) {
	edges := []priorityBatchEdge{{"one", "a", .9001}, {"one", "b", .9002}}
	if got, want := selectPriorityBatch(edges), []int{1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPriorityBatchAgreesWithExhaustiveSearch(t *testing.T) {
	rng := rand.New(rand.NewSource(4052026))
	for trial := 0; trial < 1000; trial++ {
		var edges []priorityBatchEdge
		for bead := 0; bead < 4; bead++ {
			for worker := 0; worker < 4; worker++ {
				if rng.Intn(3) != 0 {
					edges = append(edges, priorityBatchEdge{
						bead: fmt.Sprint(bead), worker: fmt.Sprint(worker),
						score: float64(rng.Intn(1001)) / 1000,
					})
				}
			}
		}
		before := append([]priorityBatchEdge(nil), edges...)
		var order []string
		adj := make(map[string][]int)
		for i, edge := range edges {
			if _, ok := adj[edge.bead]; !ok {
				order = append(order, edge.bead)
			}
			adj[edge.bead] = append(adj[edge.bead], i)
		}
		// With four beads, a bit mask expresses the lexicographic objective
		// exactly: admitting any earlier bead outweighs every later bead.
		bestMask, bestScore, maxCount := -1, int64(-1), 0
		used := make(map[string]bool)
		var visit func(int, int, int64, int)
		visit = func(position, mask int, score int64, count int) {
			if position == len(order) {
				maxCount = max(maxCount, count)
				if mask > bestMask || (mask == bestMask && score > bestScore) {
					bestMask, bestScore = mask, score
				}
				return
			}
			visit(position+1, mask, score, count)
			for _, i := range adj[order[position]] {
				edge := edges[i]
				if used[edge.worker] {
					continue
				}
				used[edge.worker] = true
				visit(position+1, mask|(1<<(len(order)-1-position)), score+int64(math.Round(edge.score*1e9)), count+1)
				used[edge.worker] = false
			}
		}
		visit(0, 0, 0, 0)
		got := selectPriorityBatch(edges)
		mask, score := 0, int64(0)
		beads, workers := make(map[string]bool), make(map[string]bool)
		last := -1
		for _, index := range got {
			if index <= last || index >= len(edges) {
				t.Fatalf("trial %d: unordered/invalid indexes %v", trial, got)
			}
			last = index
			edge := edges[index]
			if beads[edge.bead] || workers[edge.worker] {
				t.Fatalf("trial %d: reused bead or worker", trial)
			}
			beads[edge.bead], workers[edge.worker] = true, true
			for pos, bead := range order {
				if bead == edge.bead {
					mask |= 1 << (len(order) - 1 - pos)
				}
			}
			score += int64(math.Round(edge.score * 1e9))
		}
		if mask != bestMask || score != bestScore || len(got) != maxCount {
			t.Fatalf("trial %d: got mask=%b score=%d count=%d; want %b %d %d; edges=%+v", trial, mask, score, len(got), bestMask, bestScore, maxCount, edges)
		}
		if !reflect.DeepEqual(edges, before) || !reflect.DeepEqual(got, selectPriorityBatch(edges)) {
			t.Fatalf("trial %d: mutated input or nondeterministic result", trial)
		}
	}
}

func BenchmarkPriorityBatch(b *testing.B) {
	var edges []priorityBatchEdge
	for bead := 0; bead < 200; bead++ {
		for worker := 0; worker < 32; worker++ {
			edges = append(edges, priorityBatchEdge{
				bead: fmt.Sprint(bead), worker: fmt.Sprint(worker),
				score: float64(300+(bead*17+worker*31)%701) / 1000,
			})
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		selectPriorityBatch(edges)
	}
}
