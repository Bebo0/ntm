package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Dicklesworthstone/ntm/internal/robot"
	"github.com/Dicklesworthstone/ntm/tests/testutil"
)

// POST /api/v1/sessions/{id}/agents/interrupt carries the Idempotency-Key
// header into the durable operation ID and surfaces the idempotency refusals
// as client conflicts (409), not server faults.
func TestHandleAgentInterruptV1IdempotencyRefusalsAreConflicts(t *testing.T) {
	srv, _ := setupTestServer(t)

	for _, code := range []string{robot.ErrCodeIdempotencyConflict, robot.ErrCodeOperationInProgress} {
		t.Run(code, func(t *testing.T) {
			var gotKey string
			srv.interruptAgents = func(opts robot.InterruptOptions) (*robot.InterruptOutput, error) {
				gotKey = opts.IdempotencyKey
				return &robot.InterruptOutput{RobotResponse: robot.NewErrorResponse(
					fmt.Errorf("operation 'retask-7' refused"), code, "Query the durable receipt with --robot-send-receipt=retask-7",
				)}, nil
			}

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/proj/agents/interrupt",
				strings.NewReader(`{"message":"change course","force":true}`))
			req.Header.Set("Idempotency-Key", "retask-7")
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("sessionId", "proj")
			req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

			srv.handleAgentInterruptV1(rec, req)

			if gotKey != "retask-7" {
				t.Fatalf("IdempotencyKey = %q, want the Idempotency-Key header", gotKey)
			}
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body: %s", rec.Code, rec.Body.String())
			}
			var response APIError
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("unmarshal response: %v", err)
			}
			if response.Success || response.ErrorCode != code {
				t.Fatalf("response = %+v, want error_code %s", response, code)
			}
		})
	}
}

// TestHandleAgentInterruptV1IdempotencyKeyIsDurable drives the real handler,
// middleware and robot.GetInterrupt against a real tmux fixture pane:
//
//   - the first POST interrupts once and delivers the task once;
//   - an identical POST to the same process is answered by the HTTP replay
//     cache;
//   - after a simulated server restart (the in-memory replay cache is gone)
//     the identical POST is answered by the durable operation record —
//     replayed, with no second Ctrl+C and no second delivery;
//   - reusing the key with a different body is a 409 IDEMPOTENCY_CONFLICT.
func TestHandleAgentInterruptV1IdempotencyKeyIsDurable(t *testing.T) {
	testutil.RequireTmuxThrottled(t)
	srv, store := setupTestServer(t)

	robot.SetProjectionStore(store)
	t.Cleanup(func() { robot.SetProjectionStore(nil) })
	feed := robot.NewAttentionFeed(robot.AttentionFeedConfig{JournalSize: 100, RetentionPeriod: time.Hour})
	oldFeed := robot.GetAttentionFeed()
	robot.SetAttentionFeed(feed)
	t.Cleanup(func() {
		robot.SetAttentionFeed(oldFeed)
		feed.Stop()
	})

	fx := testutil.StartInterruptFixture(t, "rest")
	marker := fmt.Sprintf("ntm-rest-idem-%d", time.Now().UnixNano())
	key := "retask-" + marker

	post := func(task string) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		body := fmt.Sprintf(`{"panes":[%q],"message":%q,"force":true,"no_wait":true}`, fx.PaneID, task)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+fx.Session+"/agents/interrupt", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		srv.Router().ServeHTTP(rec, req)
		var payload map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode response (status %d): %v\nbody=%s", rec.Code, err, rec.Body.String())
		}
		return rec, payload
	}
	operationOf := func(payload map[string]any) map[string]any {
		op, _ := payload["operation"].(map[string]any)
		return op
	}

	firstRec, first := post("new task " + marker)
	if firstRec.Code != http.StatusOK || first["success"] != true {
		t.Fatalf("first POST status=%d payload=%v, want success", firstRec.Code, first)
	}
	if op := operationOf(first); op == nil || op["kind"] != "interrupt" || op["operation_id"] != key || op["replayed"] == true {
		t.Fatalf("first operation = %v, want a fresh interrupt operation bound to the Idempotency-Key", op)
	}
	fx.WaitForEvents(t, marker, 1, 1)

	cachedRec, _ := post("new task " + marker)
	fx.AssertQuiet(t, marker, 1, 1)
	if cachedRec.Code != http.StatusOK || cachedRec.Header().Get("X-Idempotent-Replay") != "true" {
		t.Fatalf("same-process retry status=%d replay-header=%q, want the HTTP cache replay",
			cachedRec.Code, cachedRec.Header().Get("X-Idempotent-Replay"))
	}

	// Simulated restart: the HTTP replay cache is per-process memory; the
	// durable operation record in the state store is what survives.
	previousCache := srv.idempotencyStore
	srv.idempotencyStore = NewIdempotencyStore(24 * time.Hour)
	previousCache.Stop()

	durableRec, durable := post("new task " + marker)
	fx.AssertQuiet(t, marker, 1, 1)
	if durableRec.Code != http.StatusOK || durable["success"] != true {
		t.Fatalf("post-restart retry status=%d payload=%v, want the replayed success", durableRec.Code, durable)
	}
	if durableRec.Header().Get("X-Idempotent-Replay") == "true" {
		t.Fatal("post-restart retry was served by the HTTP cache; the durable operation record was not exercised")
	}
	if op := operationOf(durable); op == nil || op["replayed"] != true || op["kind"] != "interrupt" {
		t.Fatalf("post-restart operation = %v, want the durable interrupt replay", op)
	}
	if durable["interrupted_at"] != first["interrupted_at"] {
		t.Fatalf("post-restart interrupted_at = %v, want the recorded %v", durable["interrupted_at"], first["interrupted_at"])
	}

	// A different body under the same key: the same process's HTTP cache
	// rejects it (422), and after another restart the durable binding does
	// (409 IDEMPOTENCY_CONFLICT). Neither touches the pane.
	cachedConflictRec, cachedConflict := post("different task " + marker)
	fx.AssertQuiet(t, marker, 1, 1)
	if cachedConflictRec.Code != http.StatusUnprocessableEntity || cachedConflict["error_code"] != ErrCodeIdempotentReplay {
		t.Fatalf("same-process conflicting reuse status=%d payload=%v, want 422 %s",
			cachedConflictRec.Code, cachedConflict, ErrCodeIdempotentReplay)
	}

	previousCache = srv.idempotencyStore
	srv.idempotencyStore = NewIdempotencyStore(24 * time.Hour)
	previousCache.Stop()

	conflictRec, conflict := post("different task " + marker)
	fx.AssertQuiet(t, marker, 1, 1)
	if conflictRec.Code != http.StatusConflict || conflict["error_code"] != robot.ErrCodeIdempotencyConflict {
		t.Fatalf("post-restart conflicting reuse status=%d payload=%v, want 409 IDEMPOTENCY_CONFLICT", conflictRec.Code, conflict)
	}
}
