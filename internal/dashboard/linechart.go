package dashboard

import (
	"fmt"
	"html/template"
	"strings"
)

// renderLineChart builds a hand-rolled SVG line chart from []SpeedCostPoint.
// mode is one of "speed" (tick duration in seconds, higher = slower),
// "cost" (cost_usd), "commits", or "files". Returns "" for empty/invalid input.
// No external chart library — the dashboard is deliberately no-CDN.
//
// Layout: fixed 640x160 viewBox. Y axis auto-scales to the max value with a
// 10% headroom; X axis is evenly spaced across the points. A subtle area fill
// under the line and a value label at the last point help readability.
func renderLineChart(pts []SpeedCostPoint, mode string) template.HTML {
	if len(pts) < 2 {
		return ""
	}
	const w, h = 640.0, 160.0
	const padL, padR, padT, padB = 8.0, 46.0, 12.0, 20.0

	plotW := w - padL - padR
	plotH := h - padT - padB

	// Pull the numeric series per mode.
	values := make([]float64, len(pts))
	maxV := 0.0
	for i, p := range pts {
		var v float64
		switch mode {
		case "cost":
			v = p.Cost
		case "commits":
			v = float64(p.Commits)
		case "files":
			v = float64(p.Files)
		default:
			v = float64(p.Duration)
		}
		values[i] = v
		if v > maxV {
			maxV = v
		}
	}
	if maxV <= 0 {
		return ""
	}
	// Add headroom so the tallest point isn't flush against the top.
	scale := maxV * 1.12
	if scale <= 0 {
		scale = 1
	}

	// Build the polyline + area path + per-point tooltips (circles with <title>).
	var line, area, points strings.Builder
	var lastX, lastY float64
	step := plotW / float64(len(pts)-1)
	for i, v := range values {
		x := padL + step*float64(i)
		y := padT + plotH*(1.0-v/scale)
		if i == 0 {
			// SVG paths must start with a command; without the leading M the
			// line is invisible (getTotalLength()=0). Implicit lineto after M
			// is valid, so subsequent points stay bare coordinates.
			fmt.Fprintf(&line, "M%.1f,%.1f", x, y)
		} else {
			fmt.Fprintf(&line, " %.1f,%.1f", x, y)
		}
		lastX, lastY = x, y
		// One hover circle per point with a native <title> tooltip: time + value.
		tt := pointTitle(pts[i], v, mode)
		fmt.Fprintf(&points, `<circle cx="%.1f" cy="%.1f" r="3.5" class="pt"><title>%s</title></circle>`,
			x, y, template.HTMLEscapeString(tt))
	}
	// Area: close the line down to the baseline.
	fmt.Fprintf(&area, "M%.1f,%.1f", padL, padT+plotH)
	area.WriteString(line.String())
	fmt.Fprintf(&area, "L%.1f,%.1fZ", lastX, padT+plotH)

	// Value label + axis labels (first/last timestamps).
	var label string
	switch mode {
	case "cost":
		label = fmt.Sprintf("$%.3f", values[len(values)-1])
	case "commits":
		label = fmt.Sprintf("%d commits", int(values[len(values)-1]))
	case "files":
		label = fmt.Sprintf("%d files", int(values[len(values)-1]))
	default:
		label = fmt.Sprintf("%ds", int(values[len(values)-1]))
	}
	// Dither-kit color palette + gradient for each series.
	// color is the solid line + gradient stop; glow is the "bloom" halo.
	var color string
	var glowOpacity string
	switch mode {
	case "cost":
		color, glowOpacity = "#e8a33d", "0.55" // orange
	case "commits":
		color, glowOpacity = "#a78bfa", "0.55" // purple
	case "files":
		color, glowOpacity = "#7a8398", "0.45" // grey
	default:
		color, glowOpacity = "#2dd4a7", "0.60" // green
	}

	// Unique gradient + glow ids per mode so multiple charts don't collide.
	gid := "g" + mode
	glid := "gl" + mode

	var xLabels strings.Builder
	fmt.Fprintf(&xLabels, `<text x="%.1f" y="%.1f" class="ax-label" text-anchor="start">%s</text>`, padL, h-4, esc(pts[0].Label))
	fmt.Fprintf(&xLabels, `<text x="%.1f" y="%.1f" class="ax-label" text-anchor="end">%s</text>`, w-padR, h-4, esc(pts[len(pts)-1].Label))

	grid := ""
	// A light top gridline at the max for a reference.
	grid = fmt.Sprintf(`<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="ax-grid"/>`, padL, padT, padL+plotW, padT)

	// Dither-kit area chart: linear gradient fill (color→transparent), a
	// blurred "bloom" glow under the line, then the crisp line on top.
	return template.HTML(fmt.Sprintf(
		`<svg class="chart" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s over time">
<defs>
<linearGradient id="%s" x1="0" y1="0" x2="0" y2="1">
<stop offset="0%%" stop-color="%s" stop-opacity="0.50"/>
<stop offset="100%%" stop-color="%s" stop-opacity="0"/>
</linearGradient>
<filter id="%s" x="-20%%" y="-20%%" width="140%%" height="140%%">
<feGaussianBlur stdDeviation="5"/>
</filter>
</defs>
%s
<path d="%s" fill="url(#%s)"/>
<path d="%s" fill="none" stroke="%s" stroke-width="7" stroke-linecap="round" stroke-linejoin="round" opacity="%s" filter="url(#%s)"/>
<path d="%s" fill="none" stroke="%s" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/>
<circle cx="%.1f" cy="%.1f" r="3.5" fill="%s" class="pt-end"/>
<text x="%.1f" y="%.1f" class="chart-val" text-anchor="end" fill="%s">%s</text>
%s
%s
</svg>`,
		w, h, esc(modeTitle(mode)),
		gid, color, color,
		glid,
		grid, area.String(), gid,
		line.String(), color, glowOpacity, glid,
		line.String(), color,
		lastX, lastY, color,
		w-padR+4, lastY-8, color, esc(label),
		xLabels.String(),
		points.String(),
	))
}

// pointTitle returns the native tooltip text for a chart point: its time label
// and the value at that point, e.g. "16:44 · 29m52s" or "16:44 · $0.032".
func pointTitle(p SpeedCostPoint, v float64, mode string) string {
	label := p.Label
	if label == "" {
		label = "tick"
	}
	var val string
	switch mode {
	case "cost":
		val = fmt.Sprintf("$%.3f", v)
	case "commits":
		val = fmt.Sprintf("%d commits", int(v))
	case "files":
		val = fmt.Sprintf("%d files", int(v))
	default:
		val = fmt.Sprintf("%ds", int(v))
	}
	return label + " · " + val
}

func modeTitle(mode string) string {
	switch mode {
	case "cost":
		return "cost"
	case "commits":
		return "commits"
	case "files":
		return "files"
	default:
		return "speed"
	}
}

func esc(s string) string {
	if s == "" {
		return "—"
	}
	return template.HTMLEscapeString(s)
}
