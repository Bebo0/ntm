// Command reflectdead finds dead code that golang.org/x/tools/cmd/deadcode
// cannot report (bd-8gbr3). deadcode keeps every function RTA reaches, and RTA
// keeps every exported method of a type that ever reaches an interface value,
// because reflection could call it. So an exported method that no code calls,
// on a type that is live, never shows up as dead. On 2026-10-05 that hid a
// state GC nobody ran, an event-bus bridge nobody subscribed, two robot wait
// conditions that could never fire, compaction recovery that ran only with the
// dashboard open and a metric fed by an event nobody sent.
//
// It prints, in deadcode_gate.sh's <file>:<Func> entry format, each function
// under -filter that RTA reaches but no call-graph path from the program's
// init and main reaches.
package main

import (
	"flag"
	"fmt"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/callgraph/rta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

func main() {
	dir := flag.String("dir", ".", "module directory to analyze from")
	tags := flag.String("tags", "", "build tags")
	filter := flag.String("filter", "", "report only functions in packages with this path prefix")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: reflectdead [-dir d] [-tags t] [-filter prefix] <main package pattern>")
		os.Exit(2)
	}

	cfg := &packages.Config{Mode: packages.LoadAllSyntax | packages.NeedModule, Dir: *dir}
	if *tags != "" {
		cfg.BuildFlags = []string{"-tags=" + *tags}
	}
	initial, err := packages.Load(cfg, flag.Args()...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reflectdead: load:", err)
		os.Exit(1)
	}
	if packages.PrintErrors(initial) > 0 {
		os.Exit(1)
	}
	prog, pkgs := ssautil.AllPackages(initial, ssa.InstantiateGenerics)
	prog.Build()

	var roots []*ssa.Function
	for _, p := range ssautil.MainPackages(pkgs) {
		roots = append(roots, p.Func("init"), p.Func("main"))
	}
	if len(roots) == 0 {
		fmt.Fprintln(os.Stderr, "reflectdead: no main package in", flag.Args())
		os.Exit(1)
	}
	res := rta.Analyze(roots, true)

	// RTA's call graph has an edge for every static, dynamic and interface
	// call it resolved; what it reaches only by its reflection rule has none.
	called := make(map[*ssa.Function]bool)
	queue := append([]*ssa.Function(nil), roots...)
	for len(queue) > 0 {
		fn := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if fn == nil || called[fn] {
			continue
		}
		called[fn] = true
		if node := res.CallGraph.Nodes[fn]; node != nil {
			for _, edge := range node.Out {
				queue = append(queue, edge.Callee.Func)
			}
		}
	}

	moduleDir := ""
	if len(initial) > 0 && initial[0].Module != nil {
		moduleDir = initial[0].Module.Dir
	}
	entries := make(map[string]bool)
	for fn := range res.Reachable {
		if called[fn] {
			continue
		}
		origin := fn
		if fn.Origin() != nil {
			origin = fn.Origin()
		}
		if called[origin] || origin.Synthetic != "" || origin.Pkg == nil {
			continue
		}
		if !strings.HasPrefix(origin.Pkg.Pkg.Path(), *filter) {
			continue
		}
		pos := prog.Fset.Position(origin.Pos())
		if !pos.IsValid() || strings.HasSuffix(pos.Filename, "_test.go") {
			continue
		}
		file := pos.Filename
		if moduleDir != "" {
			if rel, err := filepath.Rel(moduleDir, file); err == nil {
				file = rel
			}
		}
		entries[filepath.ToSlash(file)+":"+funcName(origin)] = true
	}

	lines := make([]string, 0, len(entries))
	for entry := range entries {
		lines = append(lines, entry)
	}
	sort.Strings(lines)
	for _, line := range lines {
		fmt.Println(line)
	}
}

// funcName renders a function the way deadcode does: Func, or Recv.Method
// with the receiver's pointer and type arguments dropped.
func funcName(fn *ssa.Function) string {
	recv := fn.Signature.Recv()
	if recv == nil {
		return fn.Name()
	}
	t := recv.Type()
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	if named, ok := t.(*types.Named); ok {
		return named.Obj().Name() + "." + fn.Name()
	}
	return fn.Name()
}
