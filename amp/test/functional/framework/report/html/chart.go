package html

import (
	"fmt"
	"html"
	"html/template"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// chartView is a rendered chart ready for the template: the title, the
// self-contained inline SVG (no external JS/CSS/CDN, no <script>, no src=), and
// a data table duplicating the series' exact values (a graph can't show precise
// numbers).
type chartView struct {
	Title string
	SVG   template.HTML
	Table template.HTML
}

// axisTicks is the number of intervals between axis labels; ticks+1 labels per
// axis (0/25/50/75/100% of the range) with gridlines, so a value can be read off
// the chart, not just its endpoints.
const axisTicks = 4

// SVG canvas geometry. The plot area sits inside the margins; the right margin
// reserves room for the series legend.
const (
	chartW       = 680
	chartH       = 340
	marginLeft   = 56
	marginRight  = 150
	marginTop    = 28
	marginBottom = 44
	histBuckets  = 10
)

// seriesColors is a small categorical palette that stays legible on both the
// light and dark report backgrounds. Series cycle through it.
var seriesColors = []string{
	"#3b82f6", "#f97316", "#22c55e", "#a855f7",
	"#ef4444", "#06b6d4", "#eab308", "#64748b",
}

// buildChartView renders one core.Chart into a chartView. An unknown kind and a
// chart with no drawable data both fall back to a small "no data" placeholder so
// the section never renders a broken SVG.
func buildChartView(c core.Chart) chartView {
	var body string
	switch c.Kind {
	case core.ChartLine:
		body = renderLine(c)
	case core.ChartBar:
		body = renderBar(c)
	case core.ChartHistogram:
		body = renderHistogram(c)
	default:
		body = ""
	}
	if body == "" {
		body = emptyChart()
	}
	return chartView{Title: c.Title, SVG: template.HTML(svgWrap(c, body)), Table: chartTable(c)}
}

// plotL/plotR/plotT/plotB are the plot-area edges in canvas coordinates.
func plotL() float64 { return marginLeft }
func plotR() float64 { return chartW - marginRight }
func plotT() float64 { return marginTop }
func plotB() float64 { return chartH - marginBottom }

// svgWrap wraps a chart body in the <svg> element and draws the axes, the
// axis-range labels, and the axis titles shared by every kind. The SVG is
// inline-HTML5 (no xmlns, no external refs) so it is CSP-safe; text and axes use
// the report's CSS custom properties so the chart is theme-aware.
func svgWrap(c core.Chart, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart-svg" viewBox="0 0 %d %d" role="img" preserveAspectRatio="xMidYMid meet">`, chartW, chartH)
	// Axes.
	fmt.Fprintf(&b, `<line x1="%s" y1="%s" x2="%s" y2="%s" stroke="var(--muted)" stroke-width="1"/>`,
		f(plotL()), f(plotT()), f(plotL()), f(plotB()))
	fmt.Fprintf(&b, `<line x1="%s" y1="%s" x2="%s" y2="%s" stroke="var(--muted)" stroke-width="1"/>`,
		f(plotL()), f(plotB()), f(plotR()), f(plotB()))
	// Axis titles.
	if c.YLabel != "" {
		fmt.Fprintf(&b, `<text x="%s" y="%s" fill="var(--muted)" font-size="11" text-anchor="middle" transform="rotate(-90 %s %s)">%s</text>`,
			f(16), f((plotT()+plotB())/2), f(16), f((plotT()+plotB())/2), esc(c.YLabel))
	}
	if c.XLabel != "" {
		fmt.Fprintf(&b, `<text x="%s" y="%s" fill="var(--muted)" font-size="11" text-anchor="middle">%s</text>`,
			f((plotL()+plotR())/2), f(chartH-6), esc(c.XLabel))
	}
	b.WriteString(body)
	b.WriteString(legend(c.Series))
	b.WriteString(`</svg>`)
	return b.String()
}

// legend draws a colored-swatch + name key down the reserved right margin.
func legend(series []core.ChartSeries) string {
	if len(series) == 0 {
		return ""
	}
	var b strings.Builder
	x := plotR() + 14
	for i, s := range series {
		y := plotT() + float64(i)*18
		name := s.Name
		if name == "" {
			name = fmt.Sprintf("series %d", i+1)
		}
		fmt.Fprintf(&b, `<rect x="%s" y="%s" width="10" height="10" fill="%s"/>`, f(x), f(y), color(i))
		fmt.Fprintf(&b, `<text x="%s" y="%s" fill="var(--fg)" font-size="11">%s</text>`, f(x+14), f(y+9), esc(name))
	}
	return b.String()
}

// renderLine plots one polyline per series over a shared axis range. Series with
// no points are skipped; an empty chart yields "".
func renderLine(c core.Chart) string {
	xr, yr, ok := ranges(c.Series)
	if !ok {
		return ""
	}
	var b strings.Builder
	b.WriteString(yTicks(yr))
	b.WriteString(xTicks(xr))
	for i, s := range c.Series {
		if len(s.Points) == 0 {
			continue
		}
		pts := make([]string, 0, len(s.Points))
		for _, p := range s.Points {
			pts = append(pts, f(sx(p.X, xr))+","+f(sy(p.Y, yr)))
		}
		fmt.Fprintf(&b, `<polyline fill="none" stroke="%s" stroke-width="2" points="%s"/>`, color(i), strings.Join(pts, " "))
	}
	return b.String()
}

// renderBar plots one rect per point across every series' points, grouped by
// point index (one category per index) — the run-to-run spread view. The Y axis
// is zero-based.
func renderBar(c core.Chart) string {
	var vals []float64
	n := 0
	for _, s := range c.Series {
		if len(s.Points) > n {
			n = len(s.Points)
		}
		for _, p := range s.Points {
			vals = append(vals, p.Y)
		}
	}
	if n == 0 {
		return ""
	}
	_, ymax := minMax(vals)
	if ymax <= 0 {
		ymax = 1
	}
	yr := rng{0, ymax}
	ns := len(c.Series)
	var b strings.Builder
	b.WriteString(yTicks(yr))
	slot := (plotR() - plotL()) / float64(n)
	group := slot * 0.7
	bw := group / float64(ns)
	for si, s := range c.Series {
		for pi, p := range s.Points {
			x := plotL() + float64(pi)*slot + (slot-group)/2 + float64(si)*bw
			y := sy(p.Y, yr)
			fmt.Fprintf(&b, `<rect x="%s" y="%s" width="%s" height="%s" fill="%s"/>`,
				f(x), f(y), f(bw*0.92), f(plotB()-y), color(si))
			// Value label above the bar, and the category index below the axis.
			fmt.Fprintf(&b, `<text x="%s" y="%s" fill="var(--fg)" font-size="10" text-anchor="middle">%s</text>`,
				f(x+bw*0.46), f(y-3), esc(num(p.Y)))
			if si == 0 {
				fmt.Fprintf(&b, `<text x="%s" y="%s" fill="var(--muted)" font-size="10" text-anchor="middle">%s</text>`,
					f(plotL()+float64(pi)*slot+slot/2), f(plotB()+14), esc(num(p.X)))
			}
		}
	}
	return b.String()
}

// renderHistogram buckets every point's Y value (across all series) into
// histBuckets equal-width bins and draws one count bar per bin.
func renderHistogram(c core.Chart) string {
	var vals []float64
	for _, s := range c.Series {
		for _, p := range s.Points {
			vals = append(vals, p.Y)
		}
	}
	if len(vals) == 0 {
		return ""
	}
	lo, hi := minMax(vals)
	if hi <= lo {
		hi = lo + 1
	}
	width := (hi - lo) / float64(histBuckets)
	counts := make([]int, histBuckets)
	for _, v := range vals {
		idx := int((v - lo) / width)
		if idx >= histBuckets {
			idx = histBuckets - 1
		}
		if idx < 0 {
			idx = 0
		}
		counts[idx]++
	}
	maxCount := 0
	for _, ct := range counts {
		if ct > maxCount {
			maxCount = ct
		}
	}
	if maxCount == 0 {
		return ""
	}
	yr := rng{0, float64(maxCount)}
	var b strings.Builder
	b.WriteString(yTicks(yr))
	b.WriteString(xTicks(rng{lo, hi}))
	slot := (plotR() - plotL()) / float64(histBuckets)
	for i, ct := range counts {
		x := plotL() + float64(i)*slot
		y := sy(float64(ct), yr)
		fmt.Fprintf(&b, `<rect x="%s" y="%s" width="%s" height="%s" fill="%s"/>`,
			f(x+1), f(y), f(slot-2), f(plotB()-y), color(0))
		if ct > 0 {
			fmt.Fprintf(&b, `<text x="%s" y="%s" fill="var(--fg)" font-size="10" text-anchor="middle">%d</text>`,
				f(x+slot/2), f(y-3), ct)
		}
	}
	return b.String()
}

// yTicks writes axisTicks+1 evenly-spaced Y labels with a faint horizontal
// gridline at each (drawn first, so data plots on top), so a value can be read
// off the chart rather than just its endpoints.
func yTicks(yr rng) string {
	var b strings.Builder
	for i := 0; i <= axisTicks; i++ {
		val := yr.min + float64(i)/float64(axisTicks)*(yr.max-yr.min)
		y := sy(val, yr)
		if i > 0 { // i==0 is the X axis line itself
			fmt.Fprintf(&b, `<line x1="%s" y1="%s" x2="%s" y2="%s" stroke="var(--border)" stroke-width="1"/>`,
				f(plotL()), f(y), f(plotR()), f(y))
		}
		fmt.Fprintf(&b, `<text x="%s" y="%s" fill="var(--muted)" font-size="10" text-anchor="end">%s</text>`,
			f(plotL()-6), f(y+3), esc(num(val)))
	}
	return b.String()
}

// xTicks writes axisTicks+1 evenly-spaced numeric X labels with a faint vertical
// gridline at each (for line/histogram, whose X is continuous; bar charts label
// each category directly instead).
func xTicks(xr rng) string {
	var b strings.Builder
	for i := 0; i <= axisTicks; i++ {
		val := xr.min + float64(i)/float64(axisTicks)*(xr.max-xr.min)
		x := sx(val, xr)
		if i > 0 && i < axisTicks {
			fmt.Fprintf(&b, `<line x1="%s" y1="%s" x2="%s" y2="%s" stroke="var(--border)" stroke-width="1"/>`,
				f(x), f(plotT()), f(x), f(plotB()))
		}
		anchor := "middle"
		switch i {
		case 0:
			anchor = "start"
		case axisTicks:
			anchor = "end"
		}
		fmt.Fprintf(&b, `<text x="%s" y="%s" fill="var(--muted)" font-size="10" text-anchor="%s">%s</text>`,
			f(x), f(plotB()+14), anchor, esc(num(val)))
	}
	return b.String()
}

// chartTable renders the chart's series as an HTML table of exact values: one
// row per distinct X (sorted), one column per series (— where a series has no
// point at that X). A graph shows shape; this shows the numbers.
func chartTable(c core.Chart) template.HTML {
	xset := map[float64]struct{}{}
	for _, s := range c.Series {
		for _, p := range s.Points {
			xset[p.X] = struct{}{}
		}
	}
	if len(xset) == 0 {
		return ""
	}
	xs := make([]float64, 0, len(xset))
	for x := range xset {
		xs = append(xs, x)
	}
	sort.Float64s(xs)
	byX := make([]map[float64]float64, len(c.Series))
	for i, s := range c.Series {
		m := make(map[float64]float64, len(s.Points))
		for _, p := range s.Points {
			m[p.X] = p.Y
		}
		byX[i] = m
	}
	xlab := c.XLabel
	if xlab == "" {
		xlab = "x"
	}
	var b strings.Builder
	b.WriteString(`<table class="chart-data"><thead><tr><th>` + esc(xlab) + `</th>`)
	for i, s := range c.Series {
		name := s.Name
		if name == "" {
			name = fmt.Sprintf("series %d", i+1)
		}
		b.WriteString(`<th>` + esc(name) + `</th>`)
	}
	b.WriteString(`</tr></thead><tbody>`)
	for _, x := range xs {
		b.WriteString(`<tr><td>` + esc(num(x)) + `</td>`)
		for i := range c.Series {
			if y, ok := byX[i][x]; ok {
				b.WriteString(`<td>` + esc(num(y)) + `</td>`)
			} else {
				b.WriteString(`<td>&mdash;</td>`)
			}
		}
		b.WriteString(`</tr>`)
	}
	b.WriteString(`</tbody></table>`)
	return template.HTML(b.String())
}

// emptyChart is the placeholder body for a chart with no drawable data.
func emptyChart() string {
	return fmt.Sprintf(`<text x="%s" y="%s" fill="var(--muted)" font-size="12" text-anchor="middle">no data</text>`,
		f(chartW/2), f(chartH/2))
}

// rng is a numeric range with a guarded span (min==max is widened so a constant
// series still maps to a visible axis rather than dividing by zero).
type rng struct{ min, max float64 }

func (r rng) span() float64 {
	s := r.max - r.min
	if s == 0 {
		return 1
	}
	return s
}

// ranges computes the shared X and Y ranges over every series' points. The Y
// range is zero-based when all values are non-negative (the natural baseline for
// throughput/latency). It reports ok=false when no series carries any point.
func ranges(series []core.ChartSeries) (xr, yr rng, ok bool) {
	first := true
	var xmin, xmax, ymin, ymax float64
	for _, s := range series {
		for _, p := range s.Points {
			if first {
				xmin, xmax, ymin, ymax = p.X, p.X, p.Y, p.Y
				first = false
				continue
			}
			xmin, xmax = math.Min(xmin, p.X), math.Max(xmax, p.X)
			ymin, ymax = math.Min(ymin, p.Y), math.Max(ymax, p.Y)
		}
	}
	if first {
		return rng{}, rng{}, false
	}
	if ymin > 0 {
		ymin = 0
	}
	return rng{xmin, xmax}, rng{ymin, ymax}, true
}

// sx/sy map a data coordinate into the SVG plot area (y is inverted — SVG's
// origin is top-left).
func sx(x float64, xr rng) float64 {
	return plotL() + (x-xr.min)/xr.span()*(plotR()-plotL())
}

func sy(y float64, yr rng) float64 {
	return plotB() - (y-yr.min)/yr.span()*(plotB()-plotT())
}

func minMax(vals []float64) (float64, float64) {
	if len(vals) == 0 {
		return 0, 0
	}
	mn, mx := vals[0], vals[0]
	for _, v := range vals[1:] {
		mn, mx = math.Min(mn, v), math.Max(mx, v)
	}
	return mn, mx
}

// color returns the palette color for series index i, cycling.
func color(i int) string { return seriesColors[i%len(seriesColors)] }

// f formats a coordinate compactly (no trailing zeros).
func f(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// num formats a data value for an axis/point label, trimming needless decimals.
func num(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'g', 4, 64)
}

// esc escapes text for safe inclusion in SVG text nodes.
func esc(s string) string { return html.EscapeString(s) }
