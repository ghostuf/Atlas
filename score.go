package main

// A single 0-100 health score summarizing a repo's dependency risk —
// computed deterministically from signals Atlas already gathers (advisory
// severity, abandonment risk, archived repos, unused-candidate bloat), not
// an AI judgment call. Reproducible and auditable, same philosophy as bus
// factor or the usage classification.

// RepoScore is the computed score plus its matching status label/color from
// the fixed four-step status scale (good/warning/serious/critical) — never
// an eyeballed rainbow.
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

func severityWeight(sev string) float64 {
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

// usageWeight scales how much an issue on this package should count toward
// the overall score — a vulnerability wired into production code is a much
// bigger deal than the same vulnerability on a test-only or
// convention-loaded build tool.
func usageWeight(class string) float64 {
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

func isAbandonmentRisk(h *Health) bool {
	return h.TopContributor != "" && h.BusFactor >= 0.7 && h.ContributorCount <= 3 && h.WeeklyDownloads < 10000
}

func scoreLabel(v int) string {
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

// scoreColor uses the dataviz skill's fixed, validated status palette
// (good/warning/serious/critical) — never an eyeballed gradient.
func scoreColor(v int) string {
	switch {
	case v >= 80:
		return "#0ca30c" // status good
	case v >= 60:
		return "#fab219" // status warning
	case v >= 40:
		return "#ec835a" // status serious
	default:
		return "#d03b3b" // status critical
	}
}

// computeRepoScore scores only cleanly-evaluated packages (clean, from
// splitHealthProblems) — rows in the "Problems" bucket have no reliable
// signal to score them on, so they're excluded entirely rather than guessed at.
func computeRepoScore(clean []*Health, usageRows []*Usage, scanned bool) RepoScore {
	byPkg := map[string]*Usage{}
	for _, u := range usageRows {
		byPkg[u.Package] = u
	}

	penalty := 0.0
	for _, h := range clean {
		class := classifyUsage(scanned, h.Dev, byPkg[h.Package])
		w := usageWeight(class)
		for _, a := range h.Advisories {
			penalty += severityWeight(a.Severity) * w
		}
		if isAbandonmentRisk(h) {
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
	return RepoScore{Value: value, Label: scoreLabel(value), Color: scoreColor(value)}
}
