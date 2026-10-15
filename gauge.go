package main

// Renders the repo health score as a self-contained semicircular gauge SVG
// — four fixed status bands (the validated palette from the dataviz skill,
// not an eyeballed rainbow), a needle at the score's position, and a hero
// number + status label (the label stays in neutral ink with a small
// colored swatch carrying the status hue, per "text never wears the data
// color" — the swatch is the icon half of "icon + label").

import (
	"fmt"
	"html/template"
	"math"
	"strings"
)

const (
	gaugeCX     = 150.0
	gaugeCY     = 140.0 // the pivot — the arc and needle only ever occupy y <= gaugeCY by construction
	gaugeOuterR = 128.0
	gaugeInnerR = 90.0
	gaugeGapDeg = 2.0 // angular gap between bands, echoing the skill's 2px surface-gap rule
)

// gaugeBands are the four fixed status steps, left (low score) to right
// (high score) — same thresholds/colors as scoreLabel/scoreColor in score.go.
var gaugeBands = []struct {
	Label      string
	Color      string
	Start, End float64
}{
	{"Poor", "#d03b3b", 0, 40},
	{"Needs attention", "#ec835a", 40, 60},
	{"Fair", "#fab219", 60, 80},
	{"Good", "#0ca30c", 80, 100},
}

func polar(cx, cy, r, deg float64) (float64, float64) {
	rad := deg * math.Pi / 180
	return cx + r*math.Cos(rad), cy - r*math.Sin(rad)
}

// scoreToAngle maps a 0-100 score onto the gauge's 180°(low, pointing
// left)→0°(high, pointing right) sweep, through 90° (pointing up) at 50.
func scoreToAngle(score float64) float64 {
	return 180 - (score/100)*180
}

// arcSegmentPath builds a donut-segment between two angles as a sampled
// polyline rather than an SVG elliptical-arc command. Two points plus a
// radius don't uniquely determine an arc — a second circle (centered
// elsewhere) generally passes through the same two points too, and the
// sweep/large-arc flags can end up tracing *that* circle instead of the one
// actually centered at (gaugeCX, gaugeCY), which is what produced a warped,
// off-center band the first time this was rendered and looked at. Sampling
// points directly off our own polar() function has no such ambiguity.
func arcSegmentPath(outerR, innerR, startDeg, endDeg float64) string {
	const steps = 16 // per band (~45°); well smooth enough at this radius
	var b strings.Builder
	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		x, y := polar(gaugeCX, gaugeCY, outerR, startDeg+(endDeg-startDeg)*t)
		if i == 0 {
			fmt.Fprintf(&b, "M %.2f %.2f ", x, y)
		} else {
			fmt.Fprintf(&b, "L %.2f %.2f ", x, y)
		}
	}
	for i := steps; i >= 0; i-- {
		t := float64(i) / float64(steps)
		x, y := polar(gaugeCX, gaugeCY, innerR, startDeg+(endDeg-startDeg)*t)
		fmt.Fprintf(&b, "L %.2f %.2f ", x, y)
	}
	b.WriteString("Z")
	return b.String()
}

// renderGaugeSVG is trusted output: every value written into it is our own
// computed number or a fixed string (band labels, CSS classes) — nothing
// user-supplied is interpolated unescaped, so marking it template.HTML is safe.
func renderGaugeSVG(score RepoScore) template.HTML {
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="gauge" viewBox="0 0 300 225" role="img" aria-label="Dependency health score: %d out of 100, %s">`,
		score.Value, score.Label)

	for _, band := range gaugeBands {
		startDeg := scoreToAngle(band.Start)
		endDeg := scoreToAngle(band.End)
		gap := gaugeGapDeg / 2
		if band.Start > 0 {
			startDeg -= gap
		}
		if band.End < 100 {
			endDeg += gap
		}
		path := arcSegmentPath(gaugeOuterR, gaugeInnerR, startDeg, endDeg)
		fmt.Fprintf(&b, `<path d="%s" fill="%s"/>`, path, band.Color)
	}

	// Band-end anchors — plain text, neutral ink; the arc itself (not the
	// text) carries the status color.
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="start" class="gauge-endlabel">Poor</text>`,
		gaugeCX-gaugeOuterR+2, gaugeCY+18)
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="end" class="gauge-endlabel">Good</text>`,
		gaugeCX+gaugeOuterR-2, gaugeCY+18)

	// Needle — geometrically confined to y <= gaugeCY (see scoreToAngle: the
	// sweep never goes below the horizontal), so all text below is placed
	// with enough clearance from gaugeCY that it can never collide with it,
	// regardless of the needle's angle.
	needleAngle := scoreToAngle(float64(score.Value))
	tipX, tipY := polar(gaugeCX, gaugeCY, gaugeInnerR-12, needleAngle)
	leftX, leftY := polar(gaugeCX, gaugeCY, 7, needleAngle+90)
	rightX, rightY := polar(gaugeCX, gaugeCY, 7, needleAngle-90)
	fmt.Fprintf(&b, `<path d="M %.2f %.2f L %.2f %.2f L %.2f %.2f Z" class="gauge-needle"/>`,
		leftX, leftY, tipX, tipY, rightX, rightY)
	fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="8" class="gauge-pivot"/>`, gaugeCX, gaugeCY)

	// Hero number, centered, well clear of the pivot/needle.
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle" class="gauge-value">%d</text>`,
		gaugeCX, gaugeCY+48, score.Value)

	// Status swatch (the "icon") + label (neutral ink) — approximately
	// centered as a group; SVG text has no server-side width measurement,
	// so this is a character-count estimate, checked visually afterward.
	approxWidth := float64(len(score.Label)) * 7.3
	groupStartX := gaugeCX - approxWidth/2 - 9
	fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="5" fill="%s"/>`, groupStartX, gaugeCY+72, score.Color)
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="start" class="gauge-label">%s</text>`,
		groupStartX+10, gaugeCY+76, score.Label)

	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}
