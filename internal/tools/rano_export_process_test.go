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

// The test binary is the external CLI fixture. This runs before flag parsing
// in the child only; no shell scripts or installed observer are required.
func init() {
	if os.Getenv("NTM_TEST_RANO_EXPORT_PROCESS") != "1" || len(os.Args) < 2 || (os.Args[1] != "export" && os.Args[1] != "stats") {
		return
	}
	args, _ := json.Marshal(os.Args[1:])
	if err := os.WriteFile(os.Getenv("NTM_TEST_RANO_EXPORT_ARGS"), args, 0600); err != nil {
		os.Exit(9)
	}
	if os.Args[1] != "export" {
		fmt.Fprintln(os.Stderr, "rano has no stats subcommand")
		os.Exit(2)
	}
	switch os.Getenv("NTM_TEST_RANO_EXPORT_MODE") {
	case "slow":
		time.Sleep(time.Hour)
	case "fail":
		fmt.Fprintln(os.Stdout, `{}`)
		fmt.Fprintln(os.Stderr, "private command content")
		os.Exit(3)
	case "bad":
		fmt.Fprintln(os.Stdout, `{"event":`)
		os.Exit(0)
	case "flood":
		fmt.Fprint(os.Stdout, strings.Repeat("x", 11*1024*1024))
		os.Exit(0)
	case "empty":
		os.Exit(0)
	}
	ts := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	fmt.Printf("{\"ts\":%q,\"event\":\"connect\",\"pid\":42,\"comm\":\"codex\",\"provider\":\"openai\"}\n", ts)
	fmt.Printf("{\"ts\":%q,\"event\":\"close\",\"pid\":42,\"provider\":\"openai\"}\n", ts)
	os.Exit(0)
}

func ranoExportProcessFixture(t *testing.T) (*RanoAdapter, string) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(t.TempDir(), "args.json")
	t.Setenv("NTM_TEST_RANO_EXPORT_PROCESS", "1")
	t.Setenv("NTM_TEST_RANO_EXPORT_ARGS", argsPath)
	t.Setenv("NTM_TEST_RANO_EXPORT_MODE", "")
	a := &RanoAdapter{BaseAdapter: NewBaseAdapter(ToolRano, binary)}
	a.SetTimeout(5 * time.Second)
	return a, argsPath
}

func TestRanoExportCommandContract(t *testing.T) {
	a, argsPath := ranoExportProcessFixture(t)
	stats, err := a.GetAllProcessStatsWithWindow(context.Background(), "1h30m")
	if err != nil || len(stats) != 1 {
		t.Fatalf("supported export failed: %+v %v", stats, err)
	}
	// JSON assertion also compiles against the pre-fix type: restoring the
	// original adapter makes this same subprocess regression fail.
	encoded, _ := json.Marshal(stats[0])
	var record map[string]interface{}
	if err := json.Unmarshal(encoded, &record); err != nil {
		t.Fatal(err)
	}
	if record["connection_count"] != float64(1) {
		t.Fatalf("lost connection count: %s", encoded)
	}
	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if err := json.Unmarshal(data, &args); err != nil {
		t.Fatal(err)
	}
	if len(args) != 9 || !reflect.DeepEqual(args[:5], []string{"export", "--format", "jsonl", "--fields", "ts,event,pid,comm,provider"}) || args[5] != "--since" || args[7] != "--until" {
		t.Fatalf("unsupported flags or sensitive export fields: %v", args)
	}
	start, e1 := time.Parse(time.RFC3339, args[6])
	end, e2 := time.Parse(time.RFC3339, args[8])
	if e1 != nil || e2 != nil || end.Sub(start) != 90*time.Minute+2*time.Second {
		t.Fatalf("window was ignored or not fixed for the query: %v", args)
	}
}

func TestRanoExportProcessFailuresAndCancellation(t *testing.T) {
	a, _ := ranoExportProcessFixture(t)
	for _, mode := range []string{"fail", "bad", "flood", "slow"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("NTM_TEST_RANO_EXPORT_MODE", mode)
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			stats, err := a.GetAllProcessStatsWithWindow(ctx, "5m")
			if err == nil || stats != nil {
				t.Fatalf("failed command returned partial success: %+v %v", stats, err)
			}
			if strings.Contains(err.Error(), "private command content") {
				t.Fatalf("leaked stderr: %v", err)
			}
			if mode == "slow" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("lost deadline: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.GetAllProcessStatsWithWindow(ctx, "5m"); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
	if _, err := a.GetAllProcessStatsWithWindow(nil, "5m"); err == nil {
		t.Fatal("accepted nil context")
	}
}

func TestRanoExportPIDSelectionAndEmptyEvidence(t *testing.T) {
	a, _ := ranoExportProcessFixture(t)
	for _, pid := range []int{42, 77} {
		stat, err := a.GetProcessStatsWithWindow(context.Background(), pid, "5m")
		if err != nil || stat.PID != pid {
			t.Fatalf("PID lookup = %+v %v", stat, err)
		}
	}
	if _, err := a.GetProcessStats(context.Background(), 0); err == nil {
		t.Fatal("accepted zero PID")
	}
	t.Setenv("NTM_TEST_RANO_EXPORT_MODE", "empty")
	stats, err := a.GetAllProcessStats(context.Background())
	if err != nil || stats == nil || len(stats) != 0 {
		t.Fatalf("checked-empty export = %+v %v", stats, err)
	}
}
