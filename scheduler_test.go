package main

import "testing"

func TestRateExpr(t *testing.T) {
	tests := []struct {
		minutes int
		want    string
	}{
		{1, "rate(1 minute)"},
		{2, "rate(2 minutes)"},
		{5, "rate(5 minutes)"},
		{60, "rate(60 minutes)"},
	}
	for _, tc := range tests {
		if got := rateExpr(tc.minutes); got != tc.want {
			t.Errorf("rateExpr(%d) = %q, want %q", tc.minutes, got, tc.want)
		}
	}
}

func TestCadenceLabel(t *testing.T) {
	tests := []struct {
		minutes int
		want    string
	}{
		{0, "1 min"},
		{1, "1 min"},
		{5, "5 min"},
		{59, "59 min"},
		{60, "1 hr"},
		{90, "90 min"},
		{120, "2 hr"},
	}
	for _, tc := range tests {
		if got := cadenceLabel(tc.minutes); got != tc.want {
			t.Errorf("cadenceLabel(%d) = %q, want %q", tc.minutes, got, tc.want)
		}
	}
}

// newScanScheduler returns a disabled scheduler (client nil) when the schedule env is not
// configured, so the web UI works locally without AWS and UpdateInterval is a safe no-op.
func TestNewScanScheduler_DisabledWhenUnconfigured(t *testing.T) {
	s, err := newScanScheduler(t.Context(), Config{})
	if err != nil {
		t.Fatalf("newScanScheduler: %v", err)
	}
	if s == nil || s.client != nil {
		t.Fatalf("expected a disabled (client-nil) scheduler when unconfigured, got %+v", s)
	}
	if err := s.UpdateInterval(t.Context(), 5); err != nil {
		t.Fatalf("disabled scheduler UpdateInterval should be a no-op, got %v", err)
	}
}
