// Package robot: send_idempotency.go holds the send-specific half of durable
// idempotent operations (#245): the command binding, the recorded outcome,
// per-target admissions, replay, and the send receipt outcome.
//
// A caller may supply an operation ID with --robot-send (or the REST
// Idempotency-Key header). The shared claim / replay / conflict / takeover
// protocol lives in operation_idempotency.go; this file binds a send to the
// caller's command spec — session, target selector, delivery toggles, and a
// digest of the caller's input message (see sendOperationBindingHash for why
// the selector and input message are bound rather than the resolved panes
// and delivered payload).
//
// Preflight failures (nothing typed) release the claim so the ID stays
// retryable; only real dispatch attempts record terminal outcomes.
package robot

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/state"
)

// sendOperationOutcome is the durable outcome record stored as JSON in the
// send_operations row and replayed verbatim to identical retries.
type sendOperationOutcome struct {
	Success        bool                 `json:"success"`
	SentAt         time.Time            `json:"sent_at"`
	Targets        []string             `json:"targets"`
	Successful     []string             `json:"successful"`
	Failed         []SendError          `json:"failed"`
	Admissions     []OperationAdmission `json:"admissions"`
	Error          string               `json:"error,omitempty"`
	ErrorCode      string               `json:"error_code,omitempty"`
	MessagePreview string               `json:"message_preview,omitempty"`
}

// sendOperationBindingHash binds an operation to the caller's canonical
// COMMAND spec: session, target selector, delivery behavior, and a digest
// of the caller's INPUT message.
//
// The selector (not the resolved pane list) keeps a byte-identical retry a
// replay even when pane topology changed between attempts: under --all or
// --type the caller cannot re-specify "the original targets", only the
// original selector. List-valued selectors are sorted so logically
// identical retries bind identically.
//
// The input message (opts.Message, before CASS injection) is used rather
// than the delivered payload because CASS injection is time-varying: a
// byte-identical `--with-cass` retry would otherwise digest differently and
// be rejected as a conflict. The receipt's PayloadSHA256 still records the
// exact post-transformation bytes NTM attempted to deliver.
//
// Delivery-behavior flags (--enter/--submit, --clear-input) are bound too:
// reusing an operation ID with a different submit behavior is a different
// operation, not a replay of the original.
//
// The actuation kind is deliberately NOT hashed here: send bindings predate
// kinds, and hashing it would turn every pre-upgrade operation ID into a
// conflict. Cross-kind reuse is caught by the stored kind column instead.
func sendOperationBindingHash(opts SendOptions) string {
	b := newOperationBindingHasher()
	b.field(opts.Session)
	if opts.All {
		b.field("all")
	}
	b.field(opts.Pane)
	b.list(opts.Panes)
	b.list(opts.AgentTypes)
	b.list(opts.Exclude)
	enter := "default"
	if opts.Enter != nil {
		enter = strconv.FormatBool(*opts.Enter)
	}
	b.field(enter)
	b.field(strconv.FormatBool(opts.ClearInput))
	// The --with-cass/--with-memory TOGGLES are part of the command; the
	// injected content is deliberately not (it varies between attempts).
	b.field(strconv.FormatBool(opts.WithCASS))
	// CROSS-VERSION STABILITY: WithMemory joined the binding in v1.24.0.
	// It is written only when true so a command recorded by an earlier NTM
	// (which had no WithMemory field) still hashes identically when retried
	// without --with-memory — otherwise every pre-upgrade operation ID would
	// replay as IDEMPOTENCY_CONFLICT instead of its recorded outcome. The
	// conditional field is unambiguous: it is followed by a 64-hex-char
	// digest field, so "true" can never be confused with a message digest.
	if opts.WithMemory {
		b.field(strconv.FormatBool(opts.WithMemory))
	}
	inputSHA, _ := operationPayloadDigest(opts.Message)
	b.field(inputSHA)
	return b.sum()
}

// admissionsFromSendOutput derives typed per-target admission states from a
// finished dispatch result.
func admissionsFromSendOutput(output *SendOutput) []OperationAdmission {
	if output == nil {
		return nil
	}
	successful := make(map[string]bool, len(output.Successful))
	for _, target := range output.Successful {
		successful[target] = true
	}
	failures := make(map[string]string, len(output.Failed))
	for _, failure := range output.Failed {
		failures[failure.Pane] = failure.Error
	}

	admissions := make([]OperationAdmission, 0, len(output.Targets))
	for _, target := range output.Targets {
		if successful[target] {
			admissions = append(admissions, OperationAdmission{Target: target, State: AdmissionSubmitted})
			continue
		}
		// Presence check, not message check: a failure recorded with an
		// empty error string is still a rejection, not "never attempted".
		if msg, failed := failures[target]; failed {
			admissions = append(admissions, OperationAdmission{
				Target: target, State: AdmissionRejected, Error: msg,
			})
			continue
		}
		admissions = append(admissions, OperationAdmission{Target: target, State: AdmissionNotAttempted})
	}
	return admissions
}

// applyReplayedSendOutcome restores a stored outcome onto a fresh SendOutput
// so an identical retry observes the original result without a second send.
func applyReplayedSendOutcome(output *SendOutput, op *state.SendOperation) error {
	var outcome sendOperationOutcome
	if err := json.Unmarshal([]byte(op.OutcomeJSON), &outcome); err != nil {
		return fmt.Errorf("decode stored send outcome: %w", err)
	}
	output.Success = outcome.Success
	output.Error = outcome.Error
	output.ErrorCode = outcome.ErrorCode
	output.SentAt = outcome.SentAt
	output.Targets = outcome.Targets
	output.Successful = outcome.Successful
	output.Failed = outcome.Failed
	if outcome.MessagePreview != "" {
		output.MessagePreview = outcome.MessagePreview
	}
	output.Operation = replayedOperationInfo(op, outcome.Admissions)
	// A replayed failure is terminal for THIS operation ID: keystrokes may
	// already have landed, so the retry semantics the caller opted into
	// forbid a second delivery attempt under the same ID. Say so instead of
	// letting the caller retry the same command forever.
	if !outcome.Success && output.Hint == "" {
		output.Hint = "recorded outcome replayed without re-sending; use a new operation ID to attempt delivery again"
	}
	return nil
}

// completeSendOperation persists the terminal outcome for a claimed send
// and attaches the operation info to the output. Best-effort: a persistence
// failure surfaces as a warning rather than failing the send that already
// happened.
func completeSendOperation(op *durableOperation, output *SendOutput) {
	admissions := admissionsFromSendOutput(output)
	info, warning := op.complete(sendOperationOutcome{
		Success:        output.Success,
		SentAt:         output.SentAt,
		Targets:        output.Targets,
		Successful:     output.Successful,
		Failed:         output.Failed,
		Admissions:     admissions,
		Error:          output.Error,
		ErrorCode:      output.ErrorCode,
		MessagePreview: output.MessagePreview,
	}, admissions)
	if warning != "" {
		output.Warnings = append(output.Warnings, warning)
		return
	}
	output.Operation = info
}

// SendReceiptOutcome is the recorded terminal result of a completed send
// operation as exposed by receipt queries.
type SendReceiptOutcome struct {
	Success    bool        `json:"success"`
	SentAt     time.Time   `json:"sent_at"`
	Targets    []string    `json:"targets"`
	Successful []string    `json:"successful"`
	Failed     []SendError `json:"failed"`
	Error      string      `json:"error,omitempty"`
	ErrorCode  string      `json:"error_code,omitempty"`
}

func sendReceiptOutcomeFromJSON(outcomeJSON string) (*SendReceiptOutcome, error) {
	var outcome sendOperationOutcome
	if err := json.Unmarshal([]byte(outcomeJSON), &outcome); err != nil {
		return nil, fmt.Errorf("decode stored send outcome: %w", err)
	}
	return &SendReceiptOutcome{
		Success:    outcome.Success,
		SentAt:     outcome.SentAt,
		Targets:    outcome.Targets,
		Successful: outcome.Successful,
		Failed:     outcome.Failed,
		Error:      outcome.Error,
		ErrorCode:  outcome.ErrorCode,
	}, nil
}
