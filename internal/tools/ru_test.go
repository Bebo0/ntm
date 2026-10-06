package tools

import (
	"context"
	"slices"
	"testing"
	"time"
)

// TestRUCapabilitiesReadHelpFromStderr: ru prints --help on stderr, so a
// stdout-only probe never saw --json.
func TestRUCapabilitiesReadHelpFromStderr(t *testing.T) {
	adapter := NewRUAdapter()
	if _, installed := adapter.Detect(); !installed {
		t.Skip("ru not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	caps, err := adapter.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities() error: %v", err)
	}
	if !slices.Contains(caps, CapRobotMode) {
		t.Fatalf("Capabilities() = %v, want robot mode from ru's --json help line", caps)
	}
}
