package core

// Renders the repo health score as a self-contained semicircular gauge SVG.

import (
	"fmt"
	"html/template"
	"math"
	"strings"
)

const (
	gaugeCX     = 150.0
	gaugeCY     = 140.0
	gaugeOuterR = 128.0
	gaugeInnerR = 90.0
	gaugeGapDeg = 2.0
)

var gaugeBands = []struct {
	Label      string
	Color      string
	Start, End float64
}{
	{"Poor", "#ff4d4f", 0, 40},            // --error (high contrast bright red)
	{"Needs attention", "#ff7875", 40, 60}, // --error-extra (bright coral red)
	{"Fair", "#404040", 60, 80},            // --sub
	{"Good", "#b7976a", 80, 100},           // --main (gold accent)
}

func polar(cx, cy, r, deg float64) (float64, float64) {
	rad := deg * math.Pi / 180
	return cx + r*math.Cos(rad), cy - r*math.Sin(rad)
}

func scoreToAngle(score float64) float64 {
	return 180 - (score/100)*180
}

func arcSegmentPath(outerR, innerR, startDeg, endDeg float64) string {
	const steps = 16
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

// RenderGaugeSVG renders a semicircular gauge SVG for the given repo health score.
func RenderGaugeSVG(score RepoScore) template.HTML {
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

	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="start" class="gauge-endlabel">Poor</text>`,
		gaugeCX-gaugeOuterR+2, gaugeCY+18)
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="end" class="gauge-endlabel">Good</text>`,
		gaugeCX+gaugeOuterR-2, gaugeCY+18)

	needleAngle := scoreToAngle(float64(score.Value))
	tipX, tipY := polar(gaugeCX, gaugeCY, gaugeInnerR-12, needleAngle)
	leftX, leftY := polar(gaugeCX, gaugeCY, 7, needleAngle+90)
	rightX, rightY := polar(gaugeCX, gaugeCY, 7, needleAngle-90)
	fmt.Fprintf(&b, `<path d="M %.2f %.2f L %.2f %.2f L %.2f %.2f Z" class="gauge-needle"/>`,
		leftX, leftY, tipX, tipY, rightX, rightY)
	fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="8" class="gauge-pivot"/>`, gaugeCX, gaugeCY)

	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle" class="gauge-value">%d</text>`,
		gaugeCX, gaugeCY+48, score.Value)

	approxWidth := float64(len(score.Label)) * 7.3
	groupStartX := gaugeCX - approxWidth/2 - 9
	fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="5" fill="%s"/>`, groupStartX, gaugeCY+72, score.Color)
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="start" class="gauge-label">%s</text>`,
		groupStartX+10, gaugeCY+76, score.Label)

	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}
