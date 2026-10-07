package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Re-exec this test binary as the external CLI, before test flag parsing. The
// fixture accepts the published passive argv and rejects the old fake command.
func init() {
	if os.Getenv("NTM_TEST_PT_WATCH_PROCESS") != "1" || len(os.Args) < 2 ||
		(os.Args[1] != "agent" && os.Args[1] != "classify" && os.Args[1] != "--version" && os.Args[1] != "health") {
		return
	}
	args, _ := json.Marshal(os.Args[1:])
	_ = os.WriteFile(os.Getenv("NTM_TEST_PT_WATCH_ARGS"), args, 0600)
	if os.Args[1] == "--version" {
		fmt.Println("pt-core 2.2.1")
		os.Exit(0)
	}
	if reflect.DeepEqual(os.Args[1:], []string{"agent", "watch", "--help"}) {
		if os.Getenv("NTM_TEST_PT_WATCH_MODE") == "unsupported" {
			fmt.Println("unsupported agent surface")
		} else {
			fmt.Println("Watch for new candidates: --once --threshold --format")
		}
		os.Exit(0)
	}
	if !reflect.DeepEqual(os.Args[1:], []string{"agent", "watch", "--once", "--threshold", "low", "--format", "jsonl"}) {
		fmt.Fprintln(os.Stderr, "pt has no classify command; refusing unsupported argv")
		os.Exit(2)
	}
	switch os.Getenv("NTM_TEST_PT_WATCH_MODE") {
	case "slow":
		time.Sleep(time.Hour)
	case "empty":
		os.Exit(0)
	case "flood":
		fmt.Print(strings.Repeat("x", 11*1024*1024))
		os.Exit(0)
	case "stderr-flood":
		fmt.Fprint(os.Stderr, strings.Repeat("private-command-content", 4000))
		os.Exit(0)
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	fmt.Printf("{\"event\":\"candidate_detected\",\"timestamp\":%q,\"pid\":42,\"classification\":\"kill\",\"confidence\":0.98,\"severity\":\"critical\",\"command\":\"secret-prompt\"}\n", stamp)
	fmt.Printf("{\"event\":\"candidate_detected\",\"timestamp\":%q,\"pid\":43,\"classification\":\"spare\",\"confidence\":0.76}\n", stamp)
	switch os.Getenv("NTM_TEST_PT_WATCH_MODE") {
	case "failed":
		fmt.Fprintln(os.Stderr, "secret-prompt")
		os.Exit(1) // A successful WATCH is zero, unlike an agent PLAN.
	case "malformed":
		fmt.Println(`{"event":`)
	}
	os.Exit(0)
}

func ptWatchFixture(t *testing.T) (*PTAdapter, string) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "args.json")
	t.Setenv("NTM_TEST_PT_WATCH_PROCESS", "1")
	t.Setenv("NTM_TEST_PT_WATCH_ARGS", path)
	t.Setenv("NTM_TEST_PT_WATCH_MODE", "")
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	a := &PTAdapter{BaseAdapter: NewBaseAdapter(ToolPT, binary)}
	a.SetTimeout(5 * time.Second)
	return a, path
}

func TestPTPassiveWatchCommandContract(t *testing.T) {
	a, path := ptWatchFixture(t)
	result, err := a.ClassifyProcesses(context.Background(), []int{44, 43, 42, 42})
	if err != nil || len(result) != 3 {
		t.Fatalf("passive snapshot failed: %+v %v", result, err)
	}
	if result[0].PID != 42 || result[0].Classification != PTClassAbandoned || result[0].Confidence != 0.98 ||
		result[1].PID != 43 || result[1].Classification != PTClassUnknown || result[1].Confidence != 0 ||
		result[2].PID != 44 || result[2].Classification != PTClassUnknown || result[2].Confidence != 0 {
		t.Fatalf("missing/spared PIDs became healthy or evidence was lost: %+v", result)
	}
	// This test also compiles against the original adapter for a negative control.
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "secret-prompt") || !strings.Contains(string(encoded), `"source":"pt_agent_watch"`) ||
		!strings.Contains(string(encoded), `"abandonment_probability":0.76`) {
		t.Fatalf("invalid provenance or leaked command: %s", encoded)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if json.Unmarshal(data, &args) != nil || !reflect.DeepEqual(args, []string{"agent", "watch", "--once", "--threshold", "low", "--format", "jsonl"}) {
		t.Fatalf("used unsupported or acting CLI: %s", data)
	}
}

func TestPTPassiveWatchFailureIsNotPrefixSuccess(t *testing.T) {
	for _, mode := range []string{"failed", "malformed", "flood", "stderr-flood", "slow"} {
		t.Run(mode, func(t *testing.T) {
			a, _ := ptWatchFixture(t)
			t.Setenv("NTM_TEST_PT_WATCH_MODE", mode)
			if mode == "slow" {
				a.SetTimeout(50 * time.Millisecond)
			}
			result, err := a.ClassifyProcesses(context.Background(), []int{42})
			if err == nil || result != nil || strings.Contains(err.Error(), "secret-prompt") || strings.Contains(err.Error(), "private-command-content") {
				t.Fatalf("failure reported data or leaked stderr: %+v %v", result, err)
			}
			if mode == "slow" && (!errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrTimeout)) {
				t.Fatalf("deadline identity lost: %v", err)
			}
		})
	}
}

func TestPTPassiveWatchEmptyInvalidAndCancelled(t *testing.T) {
	a, path := ptWatchFixture(t)
	for _, pids := range [][]int{{0}, {-1}} {
		if _, err := a.ClassifyProcesses(context.Background(), pids); err == nil {
			t.Fatal("invalid PID accepted")
		}
	}
	if _, err := a.ClassifyProcesses(nil, []int{42}); err == nil {
		t.Fatal("nil context accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.ClassifyProcesses(ctx, []int{42}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	result, err := a.ClassifyProcesses(context.Background(), nil)
	if err != nil || result == nil || len(result) != 0 {
		t.Fatalf("empty request: %+v %v", result, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("invalid/cancelled/empty request invoked PT")
	}
	t.Setenv("NTM_TEST_PT_WATCH_MODE", "empty")
	single, err := a.ClassifyProcess(context.Background(), 42)
	if err != nil || single.Classification != PTClassUnknown || single.Confidence != 0 {
		t.Fatalf("empty watch became health proof: %+v %v", single, err)
	}
}

func TestPTPassiveWatchSchema(t *testing.T) {
	now := time.Now().UTC()
	valid := fmt.Sprintf(`{"event":"candidate_detected","timestamp":%q,"pid":42,"classification":"kill","confidence":0.95}`, now.Format(time.RFC3339Nano))
	cases := map[string]string{
		"invalid": "{", "null": "null", "array": "[]", "no-event": "{}",
		"missing-confidence":     strings.Replace(valid, `,"confidence":0.95`, "", 1),
		"negative-confidence":    strings.Replace(valid, "0.95", "-1", 1),
		"over-one":               strings.Replace(valid, "0.95", "1.1", 1),
		"below-threshold":        strings.Replace(valid, "0.95", "0.1", 1),
		"bad-pid":                strings.Replace(valid, `"pid":42`, `"pid":0`, 1),
		"fractional-pid":         strings.Replace(valid, `"pid":42`, `"pid":42.5`, 1),
		"unknown-recommendation": strings.Replace(valid, `"kill"`, `"useful"`, 1),
		"stale":                  strings.Replace(valid, now.Format(time.RFC3339Nano), now.Add(-time.Hour).Format(time.RFC3339Nano), 1),
		"future":                 strings.Replace(valid, now.Format(time.RFC3339Nano), now.Add(time.Hour).Format(time.RFC3339Nano), 1),
		"duplicate":              valid + "\n" + valid,
		"trailing-corruption":    valid + "\n[",
		"not-one-shot":           strings.Replace(valid, "candidate_detected", "severity_escalated", 1),
		"oversize":               strings.Repeat("x", 65*1024),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			results, err := parsePTWatch([]byte(data), map[int]struct{}{42: {}}, now.Add(-time.Second), now)
			if err == nil || results != nil {
				t.Fatalf("accepted invalid evidence: %+v %v", results, err)
			}
		})
	}
	data := "{\"event\":\"goal_violated\"}\n" + valid + "\n"
	results, err := parsePTWatch([]byte(data), map[int]struct{}{43: {}}, now.Add(-time.Second), now)
	if err != nil || len(results) != 1 || results[0].PID != 43 || results[0].Classification != PTClassUnknown {
		t.Fatalf("host/unrequested event attributed: %+v %v", results, err)
	}
}

func TestPTWatchHealthRequiresThePassiveContract(t *testing.T) {
	a, _ := ptWatchFixture(t)
	for _, mode := range []string{"", "unsupported"} {
		t.Setenv("NTM_TEST_PT_WATCH_MODE", mode)
		health, err := a.Health(context.Background())
		if err != nil || health.Healthy != (mode == "") {
			t.Fatalf("health = %+v, %v", health, err)
		}
	}
}
