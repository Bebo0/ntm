package caut

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// installFakeCaut puts a stand-in caut first on PATH whose usage command runs
// body and logs each invocation's arguments to the returned file.
func installFakeCaut(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$*\" >> \"" + calls + "\"\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "caut"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

func cautCallCount(t *testing.T, calls string) int {
	t.Helper()
	data, err := os.ReadFile(calls)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

func TestRefreshIfStaleFillsCacheFromCautUsage(t *testing.T) {
	installFakeCaut(t, `case "$*" in
  *"--provider claude"*) echo '{"schemaVersion":"caut.v1","generatedAt":"2026-10-06T00:00:00Z","command":"usage","data":[{"provider":"claude","source":"oauth","usage":{"primary":{"usedPercent":64},"updatedAt":"2026-10-06T00:00:00Z"}}],"errors":[]}' ;;
  *) echo '{"schemaVersion":"caut.v1","generatedAt":"2026-10-06T00:00:00Z","command":"usage","data":[],"errors":["not configured"]}' ;;
esac`)
	poller := NewUsagePoller()
	poller.RefreshIfStale(context.Background(), time.Minute)

	status := poller.GetCache().GetStatus()
	if status == nil || status.ProviderCount != 1 || status.QuotaPercent != 64 {
		t.Fatalf("status = %+v, want claude's 64%% as the only provider", status)
	}
	if p := status.Providers[0]; p.Name != "claude" || !p.HasQuota || p.QuotaUsed != 64 {
		t.Fatalf("provider = %+v, want claude at 64%%", p)
	}
	if _, err := poller.GetCache().GetLastError(); err != nil {
		t.Fatalf("unexpected cache error: %v", err)
	}
}

// A caut that fails every read records the failure and is not re-run by each
// reader until maxAge passes; the dashboard refreshes every 30s.
func TestRefreshIfStaleThrottlesFailingCaut(t *testing.T) {
	calls := installFakeCaut(t, `echo "error: no credentials" >&2; exit 1`)
	poller := NewUsagePoller()

	poller.RefreshIfStale(context.Background(), time.Minute)
	first := cautCallCount(t, calls)
	if first == 0 {
		t.Fatal("caut was never called")
	}
	if _, err := poller.GetCache().GetLastError(); err == nil {
		t.Fatal("failed caut read left no error for the panel to show")
	}
	poller.RefreshIfStale(context.Background(), time.Minute)
	if got := cautCallCount(t, calls); got != first {
		t.Fatalf("second refresh within maxAge called caut again (%d calls, want %d)", got, first)
	}
}

func TestRefreshIfStaleWithoutCautLeavesCacheEmpty(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	poller := NewUsagePoller()
	poller.RefreshIfStale(context.Background(), time.Minute)
	if status := poller.GetCache().GetStatus(); status != nil {
		t.Fatalf("status = %+v without caut, want none", status)
	}
}

func TestNewUsagePoller(t *testing.T) {
	poller := NewUsagePoller()
	if poller == nil {
		t.Fatal("NewUsagePoller returned nil")
	}

	if poller.cache == nil {
		t.Error("cache not initialized")
	}
}

func TestUsagePoller_GetCache(t *testing.T) {
	poller := NewUsagePoller()

	cache := poller.GetCache()
	if cache == nil {
		t.Error("GetCache should not return nil")
	}

	// Verify it's the same cache
	if cache != poller.cache {
		t.Error("GetCache should return internal cache")
	}
}

func TestGlobalPoller(t *testing.T) {
	// GetGlobalPoller should return non-nil
	poller := GetGlobalPoller()
	if poller == nil {
		t.Error("GetGlobalPoller returned nil")
	}

	// Multiple calls should return same instance
	poller2 := GetGlobalPoller()
	if poller != poller2 {
		t.Error("GetGlobalPoller should return singleton")
	}
}
