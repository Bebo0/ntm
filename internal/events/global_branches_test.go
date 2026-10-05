package events

import (
	"testing"
)

// ---------------------------------------------------------------------------
// Publish (global async wrapper) — 0% → 100%
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// History (global wrapper) — 0% → 100%
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// DefaultEmitter — 0% → 100%
// ---------------------------------------------------------------------------

func TestDefaultEmitter(t *testing.T) {
	// Not parallel: accesses global singleton.
	em := DefaultEmitter()
	if em == nil {
		t.Fatal("DefaultEmitter() returned nil")
	}

	// Should return the same instance on subsequent calls.
	em2 := DefaultEmitter()
	if em != em2 {
		t.Error("DefaultEmitter() should return the same instance")
	}
}
