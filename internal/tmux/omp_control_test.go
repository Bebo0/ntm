package tmux

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOMPControlUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket protocol")
	}
	for _, tc := range []struct {
		name, path, stateInstance, responseInstance, status string
		mode                                                os.FileMode
		wantError, uncertain                                bool
		wantPosts                                           int32
	}{
		{name: "send", path: "/send", status: "queued", mode: 0600, wantPosts: 1},
		{name: "stage", path: "/stage", status: "staged", mode: 0600, wantPosts: 1},
		{name: "stale instance", path: "/send", stateInstance: "old", mode: 0600, wantError: true},
		{name: "public socket", path: "/send", mode: 0660, wantError: true},
		{name: "uncertain", path: "/send", status: "delivery_unknown", mode: 0600, wantError: true, uncertain: true, wantPosts: 1},
		{name: "response mismatch", path: "/send", responseInstance: "replacement", status: "queued", mode: 0600, wantError: true, uncertain: true, wantPosts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			socket := filepath.Join(dir, "s")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(socket, tc.mode); err != nil {
				t.Fatal(err)
			}
			var posts atomic.Int32
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-OMP-Instance") != "current" {
					t.Error("missing instance header")
				}
				instance := "current"
				if r.Method == http.MethodGet {
					if r.URL.Path != "/state" {
						t.Errorf("unexpected read path %s", r.URL.Path)
					}
					if tc.stateInstance != "" {
						instance = tc.stateInstance
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"instance_id": instance, "run_id": "run-1", "ready": true})
					return
				}
				posts.Add(1)
				if r.URL.Path != tc.path {
					t.Errorf("mutation path %q, want %q", r.URL.Path, tc.path)
				}
				if tc.responseInstance != "" {
					instance = tc.responseInstance
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"instance_id": instance, "status": tc.status})
			})}
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(func() { _ = server.Close() })
			log := installOMPMetadataFixture(t, dir, socket+"|current|run-1")
			_, err = OMPControlContext(context.Background(), "%7", tc.path, map[string]any{"request_id": "request-1", "text": "hello"})
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v, wantError=%v", err, tc.wantError)
			}
			if tc.uncertain && !errors.Is(err, ErrOMPDeliveryUncertain) {
				t.Fatalf("expected uncertain outcome, got %v", err)
			}
			if got := posts.Load(); got != tc.wantPosts {
				t.Fatalf("posts=%d, want %d", got, tc.wantPosts)
			}
			assertOMPMetadataOnly(t, log)
		})
	}
}

func TestOMPUnavailableHasNoTerminalFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix metadata fixture")
	}
	dir := t.TempDir()
	log := installOMPMetadataFixture(t, dir, filepath.Join(dir, "absent.sock")+"|current|run-1")
	if _, err := OMPControlContext(context.Background(), "%7", "/send", map[string]any{"text": "do not type"}); err == nil {
		t.Fatal("missing socket accepted")
	}
	assertOMPMetadataOnly(t, log)
}

func installOMPMetadataFixture(t *testing.T, dir, metadata string) string {
	t.Helper()
	binary, log := filepath.Join(dir, "tmux"), filepath.Join(dir, "commands")
	fixture := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$NTM_TEST_OMP_LOG\"\ncase \"$*\" in *display-message*) printf '%s\\n' \"$NTM_TEST_OMP_METADATA\";; *) exit 91;; esac\n"
	if err := os.WriteFile(binary, []byte(fixture), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NTM_TMUX_BINARY", binary)
	t.Setenv("NTM_TEST_OMP_LOG", log)
	t.Setenv("NTM_TEST_OMP_METADATA", metadata)
	return log
}

func assertOMPMetadataOnly(t *testing.T, log string) {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if !strings.Contains(line, "display-message") || strings.Contains(line, "send-keys") || strings.Contains(line, "paste-buffer") {
			t.Fatalf("unexpected terminal operation: %s", line)
		}
	}
}
