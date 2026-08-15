package dashboard

import (
	"strings"
	"testing"
)

func TestRenderLineChart(t *testing.T) {
	// Empty / single point → empty output.
	if got := string(renderLineChart(nil, "speed")); got != "" {
		t.Errorf("nil -> expected '', got %q", got)
	}
	if got := string(renderLineChart([]SpeedCostPoint{{Label: "a", Duration: 10}}, "speed")); got != "" {
		t.Errorf("single point -> expected '', got %q", got)
	}

	pts := []SpeedCostPoint{
		{Label: "14:00", Duration: 1800, Cost: 0.032},
		{Label: "15:00", Duration: 900, Cost: 0.032},
		{Label: "16:00", Duration: 600, Cost: 0.064},
	}
	svg := string(renderLineChart(pts, "speed"))
	if !strings.Contains(svg, "<svg") || !strings.Contains(svg, `<path d="`) {
		t.Fatalf("speed chart missing line path: %q", svg)
	}
	// SVG paths must start with a command: without the leading M the path is
	// invalid and the line renders invisible (regression guard — PR #1 gate).
	if !strings.Contains(svg, `<path d="M`) {
		t.Fatalf("line path must begin with M command: %q", svg)
	}
	// Dither-kit look: gradient defs + bloom glow filter + per-series palette.
	if !strings.Contains(svg, "<linearGradient") {
		t.Errorf("chart missing dither gradient fill: %q", svg)
	}
	if !strings.Contains(svg, "<feGaussianBlur") {
		t.Errorf("chart missing dither bloom glow: %q", svg)
	}
	if !strings.Contains(svg, "#2dd4a7") {
		t.Errorf("speed chart should use dither green: %q", svg)
	}
	if !strings.Contains(svg, "role=\"img\"") {
		t.Errorf("chart missing role=img for a11y")
	}
	if !strings.Contains(svg, "aria-label=\"speed over time\"") {
		t.Errorf("chart missing aria-label: %q", svg)
	}
	// Last value label should appear.
	if !strings.Contains(svg, "600s") {
		t.Errorf("speed chart missing last-value label '600s': %q", svg)
	}
	// Axis timestamps.
	if !strings.Contains(svg, "14:00") || !strings.Contains(svg, "16:00") {
		t.Errorf("chart missing x-axis labels: %q", svg)
	}
	// Per-point hover tooltips (<title> inside circles) with time + value.
	if !strings.Contains(svg, "15:00 · 900s") {
		t.Errorf("chart missing per-point tooltip '15:00 · 900s': %q", svg)
	}
	if !strings.Contains(svg, "<title>") {
		t.Errorf("chart missing <title> tooltips: %q", svg)
	}

	costSvg := string(renderLineChart(pts, "cost"))
	if !strings.Contains(costSvg, "aria-label=\"cost over time\"") {
		t.Errorf("cost chart missing aria-label: %q", costSvg)
	}
	if !strings.Contains(costSvg, "$0.064") {
		t.Errorf("cost chart missing last-value label '$0.064': %q", costSvg)
	}

	// commits + files modes.
	cp := []SpeedCostPoint{
		{Label: "a", Duration: 100, Cost: 0.03, Commits: 2, Files: 7},
		{Label: "b", Duration: 100, Cost: 0.03, Commits: 3, Files: 5},
		{Label: "c", Duration: 100, Cost: 0.03, Commits: 4, Files: 9},
	}
	comSvg := string(renderLineChart(cp, "commits"))
	if !strings.Contains(comSvg, "aria-label=\"commits over time\"") || !strings.Contains(comSvg, "4 commits") {
		t.Errorf("commits chart wrong: %q", comSvg)
	}
	filSvg := string(renderLineChart(cp, "files"))
	if !strings.Contains(filSvg, "aria-label=\"files over time\"") || !strings.Contains(filSvg, "9 files") {
		t.Errorf("files chart wrong: %q", filSvg)
	}
}
