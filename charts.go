package main

import (
	"fmt"
	"html/template"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sloccy/ollamail-aws/db"
)

// Server-rendered SVG charts for the dashboard's LLM turnaround-time visualizations.
// The app has no client-side charting dependency (plain HTMX + Bootstrap), so these
// build small, dependency-free inline SVGs from precomputed stats.

const (
	chartHeight = 220
	// boxChartWidth is lineChartWidth/3, matching the dashboard's 1fr:3fr chart-row grid
	// (box plot : line graph) so both SVGs render at the same on-screen height and scale
	// factor at any page width — see .chart-row in static/style.css.
	boxChartWidth  = 160
	lineChartWidth = 480
	chartPadL      = 54
	chartPadR      = 16
	chartPadT      = 16
	chartPadB      = 28
)

// quartiles sorts vals and returns the lowest value, first quartile, median, third
// quartile, and highest value using linear interpolation between closest ranks.
func quartiles(vals []int64) (lo, q1, med, q3, hi int64) {
	slices.Sort(vals)
	n := len(vals)
	if n == 0 {
		return 0, 0, 0, 0, 0
	}
	pct := func(p float64) int64 {
		if n == 1 {
			return vals[0]
		}
		idx := p * float64(n-1)
		loIdx := int(idx)
		hiIdx := loIdx + 1
		if hiIdx >= n {
			return vals[n-1]
		}
		frac := idx - float64(loIdx)
		return vals[loIdx] + int64(frac*float64(vals[hiIdx]-vals[loIdx]))
	}
	return vals[0], pct(0.25), pct(0.5), pct(0.75), vals[n-1]
}

// niceNum rounds x to a "nice" number (1, 2, 5, or 10 × a power of ten). When round is
// true it rounds to the nearest nice number; otherwise it rounds up. Used to pick clean,
// evenly-divisible Y-axis tick steps.
func niceNum(x float64, round bool) float64 {
	if x <= 0 {
		return 1
	}
	exp := math.Floor(math.Log10(x))
	f := x / math.Pow(10, exp)
	var nf float64
	switch {
	case round && f < 1.5:
		nf = 1
	case round && f < 3:
		nf = 2
	case round && f < 7:
		nf = 5
	case round:
		nf = 10
	case f <= 1:
		nf = 1
	case f <= 2:
		nf = 2
	case f <= 5:
		nf = 5
	default:
		nf = 10
	}
	return nf * math.Pow(10, exp)
}

// emptyTextBasePx is the "no data yet" font size, in the box chart's own coordinate
// space (boxChartWidth). The browser scales each chart's differently-sized viewBox to fill
// an (approximately) equal-width card, so a chart wider than boxChartWidth needs a
// proportionally larger inline font-size to end up the same on-screen size as the box
// chart's — which is the reference look here.
const emptyTextBasePx = 13.0

func emptyChartSVG(w int, msg string) template.HTML {
	fontPx := emptyTextBasePx * float64(w) / float64(boxChartWidth)
	return template.HTML(fmt.Sprintf( //nolint:gosec // msg is always one of this file's own constant strings, never user input, and is HTML-escaped regardless
		`<svg viewBox="0 0 %d %d" class="chart-svg" role="img" aria-label="%s"><text x="%d" y="%d" style="font-size:%.1fpx" text-anchor="middle" class="chart-empty">%s</text></svg>`,
		w, chartHeight, template.HTMLEscapeString(msg), w/2, chartHeight/2, fontPx, template.HTMLEscapeString(msg)))
}

// buildBoxPlotSVG renders a single box-and-whisker plot summarizing the latency
// distribution across all samples (min/Q1/median/Q3/max).
func buildBoxPlotSVG(samples []db.TurnaroundSample) template.HTML {
	if len(samples) == 0 {
		return emptyChartSVG(boxChartWidth, "No LLM latency data yet")
	}
	vals := make([]int64, len(samples))
	for i, s := range samples {
		vals[i] = s.DurationMs
	}
	lo, q1, med, q3, hi := quartiles(vals)

	plotTop, plotBottom := chartPadT, chartHeight-chartPadB
	plotH := float64(plotBottom - plotTop)
	scale := func(v int64) float64 {
		if hi == lo {
			return float64(plotBottom)
		}
		return float64(plotBottom) - (float64(v-lo)/float64(hi-lo))*plotH
	}

	cx := (chartPadL + (boxChartWidth - chartPadR)) / 2
	const boxHalf = 24 // narrower box to fit boxChartWidth=160's tighter plot area

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %d %d" class="chart-svg" role="img" aria-label="LLM latency distribution over the last 30 days">`, boxChartWidth, chartHeight)

	// Label every quartile boundary (hi/lo/med/q3/q1), skipping any that would land within
	// minLabelGap px of an already-drawn label (e.g. a tight IQR) to avoid overlapping
	// text. Order matters: hi/lo (the whisker extremes) and med (the median, the most
	// useful single number in the plot) are checked first so they're never the ones
	// dropped — if anything has to collapse for space, it's a quartile (q3/q1).
	const minLabelGap = 14.0
	var labeledY []float64
	for _, v := range []int64{hi, lo, med, q3, q1} {
		y := scale(v)
		tooClose := false
		for _, py := range labeledY {
			if d := y - py; d > -minLabelGap && d < minLabelGap {
				tooClose = true
				break
			}
		}
		if tooClose {
			continue
		}
		labeledY = append(labeledY, y)
		fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" class="chart-gridline"/>`, chartPadL, y, boxChartWidth-chartPadR, y)
		fmt.Fprintf(&b, `<text x="%d" y="%.1f" class="chart-axis-label" text-anchor="end">%dms</text>`, chartPadL-6, y+4, v)
	}

	// Whisker: min-to-max vertical line plus caps at each end.
	fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" class="chart-whisker"/>`, cx, scale(hi), cx, scale(lo))
	for _, v := range []int64{hi, lo} {
		y := scale(v)
		fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" class="chart-whisker"/>`, cx-boxHalf/2, y, cx+boxHalf/2, y)
	}

	// Box: Q1-to-Q3, with a median line through it.
	boxY, boxH := scale(q3), scale(q1)-scale(q3)
	fmt.Fprintf(&b, `<rect x="%d" y="%.1f" width="%d" height="%.1f" class="chart-box"/>`, cx-boxHalf, boxY, boxHalf*2, boxH)
	medY := scale(med)
	fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" class="chart-median"/>`, cx-boxHalf, medY, cx+boxHalf, medY)

	fmt.Fprintf(&b, `<text x="%d" y="%d" class="chart-axis-label" text-anchor="middle">%d email(s), last 30 days</text>`, boxChartWidth/2, chartHeight-8, len(samples))

	b.WriteString(`</svg>`)
	return template.HTML(b.String()) //nolint:gosec // b only ever contains our own static markup plus %d/%.1f-formatted numbers, never free-form text
}

type hourBucket struct {
	hour time.Time
	sum  int64
	n    int
}

func (b hourBucket) avg() float64 { return float64(b.sum) / float64(b.n) }

// buildLatencyLineSVG renders a line graph of average latency per 1-hour window, in time
// order. Hours with no samples simply have no point, so the line bridges the gap by
// connecting straight to the next hour that does have data.
func buildLatencyLineSVG(samples []db.TurnaroundSample) template.HTML {
	if len(samples) == 0 {
		return emptyChartSVG(lineChartWidth, "No LLM latency data yet")
	}

	buckets := map[int64]*hourBucket{}
	for _, s := range samples {
		t, err := time.Parse("2006-01-02 15:04:05", s.Timestamp)
		if err != nil {
			continue
		}
		t = t.UTC().Truncate(time.Hour)
		key := t.Unix()
		hb := buckets[key]
		if hb == nil {
			hb = &hourBucket{hour: t}
			buckets[key] = hb
		}
		hb.sum += s.DurationMs
		hb.n++
	}
	if len(buckets) == 0 {
		return emptyChartSVG(lineChartWidth, "No LLM latency data yet")
	}

	points := make([]hourBucket, 0, len(buckets))
	for _, hb := range buckets {
		points = append(points, *hb)
	}
	sort.Slice(points, func(i, j int) bool { return points[i].hour.Before(points[j].hour) })

	maxAvg := points[0].avg()
	for _, p := range points {
		if a := p.avg(); a > maxAvg {
			maxAvg = a
		}
	}
	if maxAvg <= 0 {
		maxAvg = 1 // avoid a degenerate flat axis
	}
	// Round the step *up* (round=false) toward a nice number so step >= maxAvg/4. This
	// guarantees niceMax/step <= 4, i.e. never more than 5 gridlines (0 + up to 4 steps),
	// while keeping ticks on clean 1/2/5×10ⁿ increments.
	step := niceNum(maxAvg/4, false)
	niceMax := math.Ceil(maxAvg/step) * step

	first, last := points[0].hour, points[len(points)-1].hour
	span := last.Sub(first).Hours()
	if span == 0 {
		span = 1
	}

	plotLeft, plotRight := chartPadL, lineChartWidth-chartPadR
	plotTop, plotBottom := chartPadT, chartHeight-chartPadB
	plotW, plotH := float64(plotRight-plotLeft), float64(plotBottom-plotTop)

	xFor := func(t time.Time) float64 {
		return float64(plotLeft) + (t.Sub(first).Hours()/span)*plotW
	}
	yFor := func(a float64) float64 {
		return float64(plotBottom) - (a/niceMax)*plotH
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %d %d" class="chart-svg" role="img" aria-label="Average LLM latency per hour">`, lineChartWidth, chartHeight)

	for a := 0.0; a <= niceMax+step/2; a += step {
		y := yFor(a)
		fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" class="chart-gridline"/>`, plotLeft, y, plotRight, y)
		fmt.Fprintf(&b, `<text x="%d" y="%.1f" class="chart-axis-label" text-anchor="end">%.0fms</text>`, plotLeft-6, y+4, a)
	}

	// X-axis ticks: up to 5, each snapped to a real data point's hour bucket (rather than
	// an evenly time-spaced instant) so a tick directly under a point always shows the same
	// time as that point's hover tooltip. The first and last points are always shown; up to
	// 3 more are chosen as the points nearest each evenly time-spaced target between them,
	// then dropped if they'd land within minTickGapPx of a neighbor — samples can cluster
	// unevenly in time (e.g. business hours vs. overnight gaps), so nearest-to-target alone
	// doesn't guarantee even pixel spacing.
	const xTicks = 5
	const minTickGapPx = 48.0
	// A span under a day means multiple ticks could land on the same calendar date, so
	// include the hour in the label to keep them distinguishable.
	xLabelFormat := "Jan 2"
	if span < 24 {
		xLabelFormat = "Jan 2 15:04"
	}
	type xTick struct {
		x     float64
		label string
	}
	labelFor := func(idx int) xTick {
		t := points[idx].hour
		return xTick{xFor(t), t.Format(xLabelFormat)}
	}
	ticks := []xTick{labelFor(0)}
	lastTick := labelFor(len(points) - 1)
	for i := 1; i < xTicks-1; i++ {
		target := float64(i) / float64(xTicks-1) * span
		best, bestDiff := 0, math.Inf(1)
		for j, p := range points {
			if d := math.Abs(p.hour.Sub(first).Hours() - target); d < bestDiff {
				best, bestDiff = j, d
			}
		}
		tk := labelFor(best)
		if tk.x-ticks[len(ticks)-1].x < minTickGapPx || lastTick.x-tk.x < minTickGapPx {
			continue
		}
		ticks = append(ticks, tk)
	}
	if len(points) > 1 {
		ticks = append(ticks, lastTick)
	}
	for i, tk := range ticks {
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%d" x2="%.1f" y2="%d" class="chart-tick"/>`, tk.x, plotBottom, tk.x, plotBottom+4)
		anchor := "middle"
		if i == 0 {
			anchor = "start"
		} else if i == len(ticks)-1 {
			anchor = "end"
		}
		fmt.Fprintf(&b, `<text x="%.1f" y="%d" class="chart-axis-label" text-anchor="%s">%s</text>`, tk.x, chartHeight-8, anchor, tk.label)
	}

	coords := make([]string, 0, len(points))
	for _, p := range points {
		coords = append(coords, fmt.Sprintf("%.1f,%.1f", xFor(p.hour), yFor(p.avg())))
	}
	fmt.Fprintf(&b, `<polyline points="%s" class="chart-line"/>`, strings.Join(coords, " "))
	for _, p := range points {
		x, y := xFor(p.hour), yFor(p.avg())
		fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="2.5" class="chart-point"/>`, x, y)
		// Invisible, larger hover target carrying the tooltip. fill="transparent" is
		// painted (unlike fill:none) so it still receives pointer events for the native
		// <title>, which shows the exact UTC hour and average on hover.
		fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="9" fill="transparent"><title>%s UTC &#183; %.0fms avg</title></circle>`,
			x, y, p.hour.Format("Jan 2 15:04"), p.avg())
	}

	b.WriteString(`</svg>`)
	return template.HTML(b.String()) //nolint:gosec // b only ever contains our own static markup plus %d/%.1f/date-formatted values, never free-form text
}
