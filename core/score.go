package core

// A single 0-100 health score summarizing a repo's dependency risk —
// computed deterministically from signals Atlas already gathers (advisory
// severity, abandonment risk, archived repos, unused-candidate bloat), not
// an AI judgment call.

// RepoScore is the computed score plus its matching status label/color.
type RepoScore struct {
	Value int
	Label string
	Color string
}

const (
	severityWeightCritical = 22.0
	severityWeightHigh     = 13.0
	severityWeightModerate = 6.0
	severityWeightLow      = 2.0
	severityWeightUnrated  = 5.0

	abandonmentPenalty     = 9.0  // high bus factor + few contributors + low downloads
	archivedPenalty        = 11.0 // repo is archived upstream
	unusedCandidatePenalty = 3.0  // flat codebase-hygiene/bloat deduction, separate from any advisory
)

// SeverityWeight returns the numeric weight for an advisory severity string.
func SeverityWeight(sev string) float64 {
	switch sev {
	case "CRITICAL":
		return severityWeightCritical
	case "HIGH":
		return severityWeightHigh
	case "MODERATE":
		return severityWeightModerate
	case "LOW":
		return severityWeightLow
	default:
		return severityWeightUnrated
	}
}

// UsageWeight scales how much an issue on this package counts toward the score.
func UsageWeight(class string) float64 {
	switch class {
	case ClassRuntime:
		return 1.0
	case ClassConfigReferenced:
		return 0.7
	case ClassTestOnly, ClassBuildTooling:
		return 0.4
	case ClassUnusedCandidate:
		return 0.2
	default: // not-scanned
		return 0.7
	}
}

// IsAbandonmentRisk checks if a dependency is at severe risk of abandonment.
func IsAbandonmentRisk(h *Health) bool {
	return h.TopContributor != "" && h.BusFactor >= 0.7 && h.ContributorCount <= 3 && h.WeeklyDownloads < 10000
}

// ScoreLabel returns the text tier for a score.
func ScoreLabel(v int) string {
	switch {
	case v >= 80:
		return "Good"
	case v >= 60:
		return "Fair"
	case v >= 40:
		return "Needs attention"
	default:
		return "Poor"
	}
}

// ScoreColor returns the hex color for a score matching the iv_spade palette.
func ScoreColor(v int) string {
	switch {
	case v >= 80:
		return "#b7976a" // --main (accent gold)
	case v >= 60:
		return "#d3c2c3" // --text (primary off-white)
	case v >= 40:
		return "#ff7875" // --error-extra (bright coral red)
	default:
		return "#ff4d4f" // --error (high contrast bright red)
	}
}

// ComputeRepoScore calculates the overall repository health score.
func ComputeRepoScore(clean []*Health, usageRows []*Usage, scanned bool) RepoScore {
	byPkg := map[string]*Usage{}
	for _, u := range usageRows {
		byPkg[u.Package] = u
	}

	penalty := 0.0
	for _, h := range clean {
		class := ClassifyUsage(scanned, h.Dev, byPkg[h.Package])
		w := UsageWeight(class)
		for _, a := range h.Advisories {
			penalty += SeverityWeight(a.Severity) * w
		}
		if IsAbandonmentRisk(h) {
			penalty += abandonmentPenalty * w
		}
		if h.Archived {
			penalty += archivedPenalty * w
		}
		if class == ClassUnusedCandidate {
			penalty += unusedCandidatePenalty
		}
	}

	value := 100 - int(penalty+0.5) // round to nearest int
	if value < 0 {
		value = 0
	}
	if value > 100 {
		value = 100
	}
	return RepoScore{Value: value, Label: ScoreLabel(value), Color: ScoreColor(value)}
}
