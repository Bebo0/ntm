package robot

import (
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/caut"
)

// ---------------------------------------------------------------------------
// convertProviderUsage — missing branches (66.7% → 100%)
// ---------------------------------------------------------------------------

func TestConvertProviderUsage_WithAccount(t *testing.T) {

	acct := "user@example.com"
	payload := &caut.ProviderPayload{
		Provider: "claude",
		Source:   "cli",
		Account:  &acct,
	}

	got := convertProviderUsage(payload)
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got.Account != "user@example.com" {
		t.Errorf("Account = %q, want %q", got.Account, "user@example.com")
	}
}

func TestConvertProviderUsage_WithResetsAt(t *testing.T) {

	now := time.Now()
	usedPct := 50.0
	payload := &caut.ProviderPayload{
		Provider: "claude",
		Source:   "web",
		Usage: caut.UsageSnapshot{
			PrimaryRateWindow: &caut.RateWindow{
				UsedPercent: &usedPct,
				ResetsAt:    &now,
			},
		},
	}

	got := convertProviderUsage(payload)
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got.PrimaryWindow == nil {
		t.Fatal("PrimaryWindow should not be nil")
	}
	if got.PrimaryWindow.ResetsAt == "" {
		t.Error("ResetsAt should be set")
	}
	// Should be RFC3339 formatted.
	if !strings.Contains(got.PrimaryWindow.ResetsAt, "T") {
		t.Errorf("ResetsAt should be RFC3339 format, got %q", got.PrimaryWindow.ResetsAt)
	}
}

func TestConvertProviderUsage_WithStatus(t *testing.T) {

	msg := "rate limited"
	payload := &caut.ProviderPayload{
		Provider: "claude",
		Source:   "api",
		Status: &caut.StatusInfo{
			Indicator:   "major",
			Description: &msg,
		},
	}

	got := convertProviderUsage(payload)
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got.Status == nil {
		t.Fatal("Status should not be nil")
	}
	if got.Status.Operational {
		t.Error("expected Operational=false")
	}
	if got.Status.Message != "rate limited" {
		t.Errorf("Status.Message = %q, want %q", got.Status.Message, "rate limited")
	}
}

func TestConvertProviderUsage_StatusNoMessage(t *testing.T) {

	payload := &caut.ProviderPayload{
		Provider: "claude",
		Source:   "web",
		Status: &caut.StatusInfo{
			Indicator: "none",
		},
	}

	got := convertProviderUsage(payload)
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got.Status == nil {
		t.Fatal("Status should not be nil")
	}
	if !got.Status.Operational {
		t.Error("expected Operational=true")
	}
	if got.Status.Message != "" {
		t.Errorf("Status.Message = %q, want empty", got.Status.Message)
	}
}

func TestConvertProviderUsage_FullPayload(t *testing.T) {

	acct := "team@acme.com"
	usedPct := 89.1
	windowMins := 60
	resetTime := time.Date(2026, 2, 5, 20, 0, 0, 0, time.UTC)
	resetDesc := "Resets in 30 minutes"
	statusMsg := "All systems operational"

	payload := &caut.ProviderPayload{
		Provider: "claude",
		Account:  &acct,
		Source:   "web",
		Status: &caut.StatusInfo{
			Indicator:   "none",
			Description: &statusMsg,
		},
		Usage: caut.UsageSnapshot{
			PrimaryRateWindow: &caut.RateWindow{
				UsedPercent:      &usedPct,
				WindowMinutes:    &windowMins,
				ResetsAt:         &resetTime,
				ResetDescription: &resetDesc,
			},
		},
	}

	got := convertProviderUsage(payload)
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got.Account != "team@acme.com" {
		t.Errorf("Account = %q", got.Account)
	}
	if got.PrimaryWindow == nil {
		t.Fatal("PrimaryWindow should not be nil")
	}
	if got.PrimaryWindow.ResetDescription != "Resets in 30 minutes" {
		t.Errorf("ResetDescription = %q", got.PrimaryWindow.ResetDescription)
	}
	if got.Status == nil || got.Status.Message != "All systems operational" {
		t.Error("Status message mismatch")
	}
}
