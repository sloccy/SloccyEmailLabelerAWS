package main

import (
	"strings"
	"testing"

	"github.com/sloccy/ollamail-aws/db"
)

func TestQuartiles(t *testing.T) {
	// 1..10: median of an even count is the average of the two middle values (5,6) = 5.5,
	// truncated to int64 by pct's linear interpolation -> 5.
	vals := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	lo, q1, med, q3, hi := quartiles(vals)
	if lo != 1 || hi != 10 {
		t.Errorf("lo/hi = %d/%d, want 1/10", lo, hi)
	}
	if med != 5 {
		t.Errorf("median = %d, want 5", med)
	}
	// idx = p*(n-1): pct(0.25) -> idx 2.25 -> interpolate between vals[2]=3 and vals[3]=4 -> 3;
	// pct(0.75) -> idx 6.75 -> interpolate between vals[6]=7 and vals[7]=8 -> 7.
	if q1 != 3 || q3 != 7 {
		t.Errorf("q1/q3 = %d/%d, want 3/7", q1, q3)
	}
}

func TestQuartilesSingleValue(t *testing.T) {
	lo, q1, med, q3, hi := quartiles([]int64{42})
	if lo != 42 || q1 != 42 || med != 42 || q3 != 42 || hi != 42 {
		t.Errorf("single-value quartiles = %d,%d,%d,%d,%d, want all 42", lo, q1, med, q3, hi)
	}
}

func TestQuartilesEmpty(t *testing.T) {
	lo, q1, med, q3, hi := quartiles(nil)
	if lo != 0 || q1 != 0 || med != 0 || q3 != 0 || hi != 0 {
		t.Errorf("empty quartiles = %d,%d,%d,%d,%d, want all 0", lo, q1, med, q3, hi)
	}
}

func TestBuildBoxPlotSVGEmpty(t *testing.T) {
	svg := string(buildBoxPlotSVG(nil))
	if !strings.Contains(svg, "<svg") || !strings.Contains(svg, "No LLM latency data yet") {
		t.Errorf("expected empty-state SVG, got %s", svg)
	}
}

func TestBuildBoxPlotSVGWithData(t *testing.T) {
	samples := []db.TurnaroundSample{{DurationMs: 100}, {DurationMs: 500}, {DurationMs: 900}}
	svg := string(buildBoxPlotSVG(samples))
	if !strings.Contains(svg, "<svg") || !strings.Contains(svg, "chart-box") || !strings.Contains(svg, "chart-median") {
		t.Errorf("expected box plot with box/median elements, got %s", svg)
	}
}

func TestBuildLatencyScatterSVGEmpty(t *testing.T) {
	svg := string(buildLatencyScatterSVG(nil))
	if !strings.Contains(svg, "<svg") || !strings.Contains(svg, "No LLM latency data yet") {
		t.Errorf("expected empty-state SVG, got %s", svg)
	}
}

func TestBuildLatencyScatterSVGPlotsRawSamples(t *testing.T) {
	// Raw samples, including two in the same hour: each should get its own dot (no
	// averaging/bucketing), and dots must never be connected by a line.
	samples := []db.TurnaroundSample{
		{Timestamp: "2026-07-01 08:00:00", DurationMs: 200},
		{Timestamp: "2026-07-01 08:30:00", DurationMs: 400},
		{Timestamp: "2026-07-03 09:00:00", DurationMs: 600}, // 2 days later, no data in between
	}
	svg := string(buildLatencyScatterSVG(samples))
	if strings.Contains(svg, "<polyline") {
		t.Errorf("expected no polyline (dots must not be connected), got %s", svg)
	}
	if got := strings.Count(svg, `class="chart-dot"`); got != len(samples) {
		t.Errorf("got %d chart-dot circles, want %d: %s", got, len(samples), svg)
	}
}
