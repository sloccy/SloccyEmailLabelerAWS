package db

import (
	"testing"
	"time"
)

// TestIsTraceStale exercises the pure threshold predicate IsSuggestionTraceStale wraps
// around a DynamoDB lookup — see isTraceStale's doc comment for why the DynamoDB-touching
// wrapper itself can't be unit tested directly in this package.
func TestIsTraceStale(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name         string
		lastActivity string
		want         bool
	}{
		{"just now: not stale", now.Format(tsLayout), false},
		{"just under the threshold: not stale", now.Add(-traceStaleAfter + time.Second).Format(tsLayout), false},
		{"just over the threshold: stale", now.Add(-traceStaleAfter - time.Second).Format(tsLayout), true},
		{"long idle: stale", now.Add(-1 * time.Hour).Format(tsLayout), true},
		{"unparseable timestamp fails open (not stale)", "not-a-timestamp", false},
		{"empty timestamp fails open (not stale)", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isTraceStale(c.lastActivity, now); got != c.want {
				t.Errorf("isTraceStale(%q, now) = %v, want %v", c.lastActivity, got, c.want)
			}
		})
	}
}
