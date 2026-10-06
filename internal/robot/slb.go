// Package robot provides machine-readable output for AI agents.
// slb.go implements the --robot-slb-* commands.
package robot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/tools"
)

// SLBPendingOutput represents the output for --robot-slb-pending.
type SLBPendingOutput struct {
	RobotResponse
	Count   int             `json:"count"`
	Pending json.RawMessage `json:"pending"`
}

// SLBActionOutput represents the output for --robot-slb-approve and --robot-slb-deny.
type SLBActionOutput struct {
	RobotResponse
	RequestID string          `json:"request_id"`
	Result    json.RawMessage `json:"result,omitempty"`
}

// GetSLBPending returns pending SLB approval requests.
// This function returns the data struct directly, enabling CLI/REST parity.
func GetSLBPending() (*SLBPendingOutput, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	adapter := tools.NewSLBAdapter()
	output := &SLBPendingOutput{
		RobotResponse: NewRobotResponse(true),
		Count:         0,
		Pending:       json.RawMessage("[]"),
	}

	if _, installed := adapter.Detect(); !installed {
		output.RobotResponse = NewErrorResponse(
			fmt.Errorf("slb not installed"),
			ErrCodeDependencyMissing,
			"Install slb to enable two-person approvals",
		)
		return output, nil
	}

	raw, err := adapter.Pending(ctx)
	if err != nil {
		code := ErrCodeInternalError
		hint := "Run 'slb pending --json' to diagnose"
		if errors.Is(err, tools.ErrTimeout) {
			code = ErrCodeTimeout
			hint = "SLB timed out; try again or check daemon status"
		}
		output.RobotResponse = NewErrorResponse(err, code, hint)
		return output, nil
	}

	if len(raw) > 0 {
		output.Pending = raw
	}
	output.Count = countJSONArray(output.Pending)
	return output, nil
}

// PrintSLBPending outputs pending SLB approvals as JSON/TOON.
// This is a thin wrapper around GetSLBPending() for CLI output.
func PrintSLBPending() error {
	output, err := GetSLBPending()
	if err != nil {
		return err
	}
	return encodeTerminalRobotOutput(output, output.RobotResponse, "robot slb pending failed")
}

// GetSLBApprove approves a pending SLB request.
// This function returns the data struct directly, enabling CLI/REST parity.
func GetSLBApprove(requestID string) (*SLBActionOutput, error) {
	requestID = strings.TrimSpace(requestID)
	output := &SLBActionOutput{
		RobotResponse: NewRobotResponse(true),
		RequestID:     requestID,
	}

	if requestID == "" {
		output.RobotResponse = NewErrorResponse(
			fmt.Errorf("missing request id"),
			ErrCodeInvalidFlag,
			"Provide a request ID: ntm --robot-slb-approve=req-123",
		)
		return output, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	adapter := tools.NewSLBAdapter()
	if _, installed := adapter.Detect(); !installed {
		output.RobotResponse = NewErrorResponse(
			fmt.Errorf("slb not installed"),
			ErrCodeDependencyMissing,
			"Install slb to enable two-person approvals",
		)
		return output, nil
	}

	raw, err := adapter.Approve(ctx, tools.SLBSessionFromEnv(), requestID)
	if err != nil {
		output.RobotResponse = slbReviewError(err)
		return output, nil
	}

	output.Result = raw
	return output, nil
}

// PrintSLBApprove outputs the SLB approval response as JSON/TOON.
// This is a thin wrapper around GetSLBApprove() for CLI output.
func PrintSLBApprove(requestID string) error {
	output, err := GetSLBApprove(requestID)
	if err != nil {
		return err
	}
	return encodeTerminalRobotOutput(output, output.RobotResponse, "robot slb approve failed")
}

// GetSLBDeny denies a pending SLB request.
// This function returns the data struct directly, enabling CLI/REST parity.
func GetSLBDeny(requestID, reason string) (*SLBActionOutput, error) {
	requestID = strings.TrimSpace(requestID)
	output := &SLBActionOutput{
		RobotResponse: NewRobotResponse(true),
		RequestID:     requestID,
	}

	if requestID == "" {
		output.RobotResponse = NewErrorResponse(
			fmt.Errorf("missing request id"),
			ErrCodeInvalidFlag,
			"Provide a request ID: ntm --robot-slb-deny=req-123 --reason='Too risky'",
		)
		return output, nil
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		output.RobotResponse = NewErrorResponse(
			fmt.Errorf("missing denial reason"),
			ErrCodeInvalidFlag,
			"slb rejects need a reason: ntm --robot-slb-deny=req-123 --reason='Too risky'",
		)
		return output, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	adapter := tools.NewSLBAdapter()
	if _, installed := adapter.Detect(); !installed {
		output.RobotResponse = NewErrorResponse(
			fmt.Errorf("slb not installed"),
			ErrCodeDependencyMissing,
			"Install slb to enable two-person approvals",
		)
		return output, nil
	}

	raw, err := adapter.Reject(ctx, tools.SLBSessionFromEnv(), requestID, reason)
	if err != nil {
		output.RobotResponse = slbReviewError(err)
		return output, nil
	}

	output.Result = raw
	return output, nil
}

// PrintSLBDeny outputs the SLB denial response as JSON/TOON.
// This is a thin wrapper around GetSLBDeny() for CLI output.
func PrintSLBDeny(requestID, reason string) error {
	output, err := GetSLBDeny(requestID, reason)
	if err != nil {
		return err
	}
	return encodeTerminalRobotOutput(output, output.RobotResponse, "robot slb deny failed")
}

// slbReviewError maps a failed slb approve/reject to a robot error.
func slbReviewError(err error) RobotResponse {
	msg := err.Error()
	switch {
	case errors.Is(err, tools.ErrTimeout):
		return NewErrorResponse(err, ErrCodeTimeout, "SLB timed out; try again or check daemon status")
	case errors.Is(err, tools.ErrSLBSessionRequired):
		return NewErrorResponse(err, ErrCodePermissionDenied, "Reviews are signed by your slb session: export SLB_SESSION_ID and SLB_SESSION_KEY")
	case strings.Contains(msg, "request not found"):
		return NewErrorResponse(err, ErrCodeNotFound, "List reviewable requests with ntm --robot-slb-pending")
	case strings.Contains(msg, "cannot review your own request"), strings.Contains(msg, "session key does not match"):
		return NewErrorResponse(err, ErrCodePermissionDenied, "Review from a different slb session than the requester, using that session's key")
	default:
		return NewErrorResponse(err, ErrCodeInternalError, "Check the request with 'slb review <id>'")
	}
}

func countJSONArray(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return 0
	}
	return len(list)
}
