package assign

import "math"

// priorityBatchEdge is an eligible match. First occurrence of each bead
// defines its priority; worker identity, not slice position, defines capacity.
type priorityBatchEdge struct {
	bead   string
	worker string
	score  float64
}

// selectPriorityBatch returns candidate indexes in input order. It preserves
// the lexicographically preferred feasible set of beads, then maximizes total
// score for that set (at 1e-9 precision). Each bead and worker occurs once.
//
// First, augmenting paths admit beads in priority order without evicting any
// previously admitted bead. A flexible task can move to a different worker to
// free a specialist, unlike greedy assignment. Matchable bead sets form a
// transversal matroid, so this also fills the maximum feasible worker count.
// Then the shared min-cost flow solver optimizes the admitted batch, without
// trading a high-priority bead for easier, lower-priority work. No map iteration
// affects tie-breaking, and inputs are never mutated.
func selectPriorityBatch(edges []priorityBatchEdge) []int {
	var order []string
	adj := make(map[string][]int)
	workerIDs := make(map[string]bool)
	for i, edge := range edges {
		if !(edge.score >= 0 && edge.score <= 1) {
			continue // Fail closed for NaN, infinities, and invalid scores.
		}
		if _, seen := adj[edge.bead]; !seen {
			order = append(order, edge.bead)
		}
		adj[edge.bead] = append(adj[edge.bead], i)
		workerIDs[edge.worker] = true
	}

	owner := make(map[string]string)
	admitted := make(map[string]bool)
	var admit func(string, map[string]bool) bool
	admit = func(bead string, visited map[string]bool) bool {
		for _, index := range adj[bead] {
			worker := edges[index].worker
			if visited[worker] {
				continue
			}
			visited[worker] = true
			previous, occupied := owner[worker]
			if !occupied || admit(previous, visited) {
				owner[worker] = bead
				return true
			}
		}
		return false
	}
	for _, bead := range order {
		if admit(bead, make(map[string]bool)) {
			admitted[bead] = true
			if len(admitted) == len(workerIDs) {
				break // Every worker is occupied; no later bead can be added.
			}
		}
	}
	if len(admitted) == 0 {
		return nil
	}

	const source, sink = 0, 1
	const scoreScale int64 = 1_000_000_000
	graph := make(allocationFlowGraph, 2)
	beads := make(map[string]int)
	workers := make(map[string]int)
	type edgeRef struct{ candidate, node, edge int }
	var refs []edgeRef
	for _, id := range order {
		if !admitted[id] {
			continue
		}
		bead := len(graph)
		graph = append(graph, nil)
		beads[id] = bead
		graph.addEdge(source, bead, 1, 0)
	}
	for i, edge := range edges {
		bead, ok := beads[edge.bead]
		if !ok || !(edge.score >= 0 && edge.score <= 1) {
			continue
		}
		worker, ok := workers[edge.worker]
		if !ok {
			worker = len(graph)
			graph = append(graph, nil)
			workers[edge.worker] = worker
			graph.addEdge(worker, sink, 1, 0)
		}
		refs = append(refs, edgeRef{candidate: i, node: bead, edge: len(graph[bead])})
		cost := scoreScale - int64(math.Round(edge.score*float64(scoreScale)))
		graph.addEdge(bead, worker, 1, cost)
	}
	potential := make([]int64, len(graph))
	for flow := 0; flow < len(admitted); flow++ {
		if !graph.augment(source, sink, potential) {
			break
		}
	}
	selected := make([]int, 0, len(admitted))
	for _, ref := range refs {
		if graph[ref.node][ref.edge].capacity == 0 {
			selected = append(selected, ref.candidate)
		}
	}
	return selected
}
