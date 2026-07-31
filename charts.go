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
	chartHeight = 165 // 220 * 0.75: ~25% shorter row (charts scale to their column width, so height is proportional to this)
	// boxChartWidth is lineChartWidth/3, matching the dashboard's 1fr:3fr chart-row grid
	// (box plot : scatter plot) so both SVGs render at the same on-screen height and scale
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

// niceTimeStepHours picks a step, in hours, from a fixed list of common calendar
// intervals (all divisors or multiples of 24h, so ticks land on a clean hour-of-day or
// midnight boundary) such that the span divides into at most 4 steps — capping the X-axis
// at 5 ticks, the same rule niceNum applies to the Y-axis.
func niceTimeStepHours(spanHours float64) float64 {
	for _, step := range []float64{1, 2, 3, 4, 6, 8, 12, 24, 48, 72, 120, 168, 240, 336, 504, 720, 1440} {
		if spanHours/step <= 4 {
			return step
		}
	}
	return 1440
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

	// boxPadR reserves extra right-side space (beyond the shared chartPadR) for the q1/q3
	// labels below, so they sit in their own gutter opposite hi/lo/med instead of sharing
	// a column with them.
	const boxPadR = 40
	plotLeft, plotRight := chartPadL, boxChartWidth-boxPadR
	cx := (plotLeft + plotRight) / 2
	const boxHalf = 20 // narrower box so it still fits the plot track alongside boxPadR

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %d %d" class="chart-svg" role="img" aria-label="LLM latency distribution over the last 30 days">`, boxChartWidth, chartHeight)

	// Label hi/lo/median on the left and q1/q3 on the right, in independent gutters, so a
	// quartile can never crowd the median (or vice versa) out of the plot: within each
	// gutter, a label is skipped only if it would land within minLabelGap px of another
	// label already drawn in that same gutter.
	const minLabelGap = 14.0
	labelGroup := func(x float64, anchor string, vals []int64) {
		var labeledY []float64
		for _, v := range vals {
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
			fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" class="chart-gridline"/>`, plotLeft, y, plotRight, y)
			fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="chart-axis-label" text-anchor="%s">%dms</text>`, x, y, anchor, v)
		}
	}
	labelGroup(float64(plotLeft-6), "end", []int64{hi, lo, med})
	labelGroup(float64(plotRight+6), "start", []int64{q3, q1})

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

// scatterPoint is one raw sample: the exact time an email was processed and the
// LLM latency for it, in ms.
type scatterPoint struct {
	t  time.Time
	ms int64
}

// buildLatencyScatterSVG renders a scatter plot of every raw latency sample against its
// actual timestamp — one dot per processed email. Unlike an hourly average, this shows
// the real spread and outliers instead of smoothing them away, and dots are deliberately
// not connected: latency from one email to the next isn't a continuous signal, so a line
// between them would imply a trend that isn't there.
func buildLatencyScatterSVG(samples []db.TurnaroundSample) template.HTML {
	if len(samples) == 0 {
		return emptyChartSVG(lineChartWidth, "No LLM latency data yet")
	}

	points := make([]scatterPoint, 0, len(samples))
	for _, s := range samples {
		t, err := time.Parse("2006-01-02 15:04:05", s.Timestamp)
		if err != nil {
			continue
		}
		points = append(points, scatterPoint{t: t.UTC(), ms: s.DurationMs})
	}
	if len(points) == 0 {
		return emptyChartSVG(lineChartWidth, "No LLM latency data yet")
	}
	sort.Slice(points, func(i, j int) bool { return points[i].t.Before(points[j].t) })

	maxMs := points[0].ms
	for _, p := range points {
		if p.ms > maxMs {
			maxMs = p.ms
		}
	}
	if maxMs <= 0 {
		maxMs = 1 // avoid a degenerate flat axis
	}
	// Round the step *up* (round=false) toward a nice number so step >= maxMs/4. This
	// guarantees niceMax/step <= 4, i.e. never more than 5 gridlines (0 + up to 4 steps),
	// while keeping ticks on clean 1/2/5×10ⁿ increments.
	step := niceNum(float64(maxMs)/4, false)
	niceMax := math.Ceil(float64(maxMs)/step) * step

	first, last := points[0].t, points[len(points)-1].t
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
	fmt.Fprintf(&b, `<svg viewBox="0 0 %d %d" class="chart-svg" role="img" aria-label="LLM latency per email over time">`, lineChartWidth, chartHeight)

	for a := 0.0; a <= niceMax+step/2; a += step {
		y := yFor(a)
		fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" class="chart-gridline"/>`, plotLeft, y, plotRight, y)
		fmt.Fprintf(&b, `<text x="%d" y="%.1f" class="chart-axis-label" text-anchor="end">%.0fms</text>`, plotLeft-6, y, a)
	}

	// X-axis ticks: the first and last are the earliest/latest sample's own timestamp.
	// Interior ticks, though, land on genuine calendar boundaries (midnight UTC, or a
	// clean hour-of-day) rather than on whatever sample happens to be nearby — snapping
	// them to real data points instead made them look unevenly spaced, since e.g. one
	// day's first sample might be at 08:00 and the next day's at 23:00. This mirrors the
	// Y-axis: gridlines sit at nice round values, independent of where the actual data
	// falls.
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
	labelFor := func(t time.Time) xTick {
		return xTick{xFor(t), t.Format(xLabelFormat)}
	}
	ticks := []xTick{labelFor(points[0].t)}
	lastTick := labelFor(points[len(points)-1].t)
	if len(points) > 1 {
		step := time.Duration(niceTimeStepHours(span) * float64(time.Hour))
		dayStart := time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, time.UTC)
		for t := dayStart; !t.After(last); t = t.Add(step) {
			if !t.After(first) {
				continue
			}
			tk := labelFor(t)
			if tk.x-ticks[len(ticks)-1].x < minTickGapPx || lastTick.x-tk.x < minTickGapPx {
				continue
			}
			ticks = append(ticks, tk)
		}
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

	// One dot per raw sample, deliberately not connected — see the function doc comment.
	// Dots are semi-transparent so overlapping samples (common with many emails in a
	// 30-day window) read as visibly denser rather than just stacking opaquely.
	for _, p := range points {
		x, y := xFor(p.t), yFor(float64(p.ms))
		fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="2" class="chart-dot"/>`, x, y)
	}

	b.WriteString(`</svg>`)
	return template.HTML(b.String()) //nolint:gosec // b only ever contains our own static markup plus %d/%.1f/date-formatted values, never free-form text
}
