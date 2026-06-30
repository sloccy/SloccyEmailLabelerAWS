package main

import (
	"html/template"
	"strings"
	"testing"
	"time"
)

func TestDict(t *testing.T) {
	t.Run("even pairs", func(t *testing.T) {
		m := dict("a", 1, "b", "hello")
		if m["a"] != 1 {
			t.Errorf("a = %v", m["a"])
		}
		if m["b"] != "hello" {
			t.Errorf("b = %v", m["b"])
		}
	})

	t.Run("odd pairs: last key dropped", func(t *testing.T) {
		m := dict("x", 10, "y")
		if m["x"] != 10 {
			t.Errorf("x = %v", m["x"])
		}
		if _, ok := m["y"]; ok {
			t.Error("dangling key y should not be in map")
		}
	})

	t.Run("empty input", func(t *testing.T) {
		m := dict()
		if len(m) != 0 {
			t.Errorf("expected empty map, got len=%d", len(m))
		}
	})

	t.Run("non-string key is silently skipped", func(t *testing.T) {
		m := dict(42, "val")
		if len(m) != 1 {
			t.Errorf("expected 1 entry with empty-string key, got len=%d", len(m))
		}
	})
}

func TestParseTS(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantOK  bool
		wantUTC string // empty means don't check value
	}{
		{name: "valid timestamp", input: "2024-03-15 09:30:00", wantOK: true, wantUTC: "2024-03-15 09:30:00"},
		{name: "empty string", input: "", wantOK: false},
		{name: "wrong format", input: "15/03/2024", wantOK: false},
		{name: "partial", input: "2024-03-15", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseTS(tc.input)
			if ok != tc.wantOK {
				t.Fatalf("parseTS(%q) ok = %v, want %v", tc.input, ok, tc.wantOK)
			}
			if ok && tc.wantUTC != "" {
				// Round-trip: format back in UTC for comparison.
				asUTC := got.UTC().Format(tsLayout)
				if asUTC != tc.wantUTC {
					t.Errorf("parsed UTC = %q, want %q", asUTC, tc.wantUTC)
				}
			}
		})
	}
}

func TestFmtdate(t *testing.T) {
	// Use a fixed time in UTC so the test is timezone-independent via Local() round-trip.
	// parseTS returns t.Local(), so we construct the input in the local zone.
	loc := time.Local
	ts := time.Date(2024, 3, 15, 9, 30, 0, 0, time.UTC).In(loc)
	input := ts.Format(tsLayout)

	got := fmtdate(input)
	if got == "--" {
		t.Fatal("fmtdate returned -- for valid input")
	}
	// The format is "2 Jan, 15:04" — just check it's non-empty and contains "Mar".
	if !strings.Contains(got, "Mar") {
		t.Errorf("fmtdate(%q) = %q, want it to contain 'Mar'", input, got)
	}

	t.Run("invalid returns --", func(t *testing.T) {
		if got := fmtdate(""); got != "--" {
			t.Errorf("fmtdate('') = %q, want '--'", got)
		}
		if got := fmtdate("bad"); got != "--" {
			t.Errorf("fmtdate('bad') = %q, want '--'", got)
		}
	})
}

func TestFmtdateStacked(t *testing.T) {
	loc := time.Local
	ts := time.Date(2024, 6, 1, 14, 5, 0, 0, time.UTC).In(loc)
	input := ts.Format(tsLayout)

	got := fmtdateStacked(input)
	s := string(got)
	if !strings.Contains(s, "<br>") {
		t.Errorf("fmtdateStacked should contain <br>, got %q", s)
	}
	if !strings.Contains(s, "text-muted") {
		t.Errorf("fmtdateStacked should contain text-muted span, got %q", s)
	}

	t.Run("invalid returns --", func(t *testing.T) {
		if got := fmtdateStacked(""); got != template.HTML("--") {
			t.Errorf("fmtdateStacked('') = %q, want '--'", got)
		}
	})
}

func TestFmtinterval(t *testing.T) {
	tests := []struct {
		secs int
		want string
	}{
		{3600, "1h"},
		{7200, "2h"},
		{3601, "1h"}, // integer division
		{60, "1m"},
		{90, "1m"},
		{120, "2m"},
		{59, "59s"},
		{1, "1s"},
		{0, "0s"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			got := fmtinterval(tc.secs)
			if got != tc.want {
				t.Errorf("fmtinterval(%d) = %q, want %q", tc.secs, got, tc.want)
			}
		})
	}
}

func TestFmtretention(t *testing.T) {
	tests := []struct {
		days int64
		want string
	}{
		{1, "1 day"},
		{2, "2 days"},
		{30, "30 days"},
		{365, "1 year"},
		{730, "2 years"},
		{364, "364 days"},
		{366, "366 days"}, // not divisible by 365
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			got := fmtretention(tc.days)
			if got != tc.want {
				t.Errorf("fmtretention(%d) = %q, want %q", tc.days, got, tc.want)
			}
		})
	}
}

func TestToJSON(t *testing.T) {
	tests := []struct {
		name  string
		input any
		want  string
	}{
		{name: "string", input: "hello", want: `"hello"`},
		{name: "number", input: 42, want: "42"},
		{name: "slice", input: []int{1, 2, 3}, want: "[1,2,3]"},
		{name: "map", input: map[string]int{"a": 1}, want: `{"a":1}`},
		{name: "nil", input: nil, want: "null"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := string(toJSON(tc.input))
			if got != tc.want {
				t.Errorf("toJSON(%v) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
