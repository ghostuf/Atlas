package agents

import (
	"atlas/core"
	"context"
	"encoding/json"
	"fmt"
)

// MaintenanceAgent evaluates maintainer turnover, bus-factor, and repository abandonment risk.
type MaintenanceAgent struct {
	model   string
	limiter *ConcurrencyLimiter
}

// NewMaintenanceAgent creates a Maintenance & Abandonment Agent.
func NewMaintenanceAgent(model string, limiter *ConcurrencyLimiter) *MaintenanceAgent {
	return &MaintenanceAgent{
		model:   model,
		limiter: limiter,
	}
}

func (a *MaintenanceAgent) Name() string {
	return "Maintainer & Abandonment Agent"
}

func (a *MaintenanceAgent) Description() string {
	return "Evaluates contributor concentration (bus factor), commit velocity, and probability of maintainer abandonment."
}

// Analyze evaluates maintenance and abandonment risk for a candidate.
func (a *MaintenanceAgent) Analyze(ctx context.Context, c RiskCandidate) (*MaintenanceAssessment, error) {
	busPct := int(c.BusFactor*100 + 0.5)

	// Try LLM schema-constrained evaluation if model is configured
	if a.model != "" && (c.Archived || c.BusFactor >= 0.6 || c.ContributorCount <= 3) {
		schema := map[string]any{
			"type": "object",
			"properties": map[string]any{
				"abandonment_risk":   map[string]any{"type": "string", "enum": []string{"critical", "moderate", "low"}},
				"bus_factor_warning": map[string]any{"type": "string"},
				"maintainer_health":  map[string]any{"type": "string"},
				"recommendation":     map[string]any{"type": "string"},
			},
			"required": []string{"abandonment_risk", "bus_factor_warning", "maintainer_health", "recommendation"},
		}

		prompt := fmt.Sprintf(
			"Analyze the maintenance sustainability and abandonment risk for npm package %q:\n"+
				"Archived status: %v\n"+
				"Bus factor: %d%%\n"+
				"Top contributor: %s\n"+
				"Contributor count: %d\n"+
				"Weekly downloads: %d\n"+
				"Open issues: %d\n"+
				"Last commit: %s\n"+
				"Last publish: %s\n"+
				"Blast radius in this project: %d transitive dependents\n\n"+
				"Rule: Do NOT read bus factor alone — read it together with contributor count and weekly downloads. "+
				"A high bus factor on a package with few contributors and low downloads is a real risk. "+
				"The same bus factor on a widely downloaded package with an active community is much less concerning. "+
				"If the repo is archived, abandonment risk is critical.",
			c.Package, c.Archived, busPct, c.Health.TopContributor, c.ContributorCount, c.WeeklyDownloads,
			c.OpenIssues, core.DateOrDash(c.Health.LastCommit), core.DateOrDash(c.Health.LastPublish), c.BlastRadius,
		)

		var respText string
		var err error
		a.limiter.Run(func() {
			respText, err = core.ChatOllamaJSON(a.model, []core.OllamaMessage{
				{Role: "system", Content: "You are an open-source health and maintainer risk analyst."},
				{Role: "user", Content: prompt},
			}, schema)
		})

		if err == nil && respText != "" {
			var assessment MaintenanceAssessment
			if jsonErr := json.Unmarshal([]byte(respText), &assessment); jsonErr == nil {
				assessment.Package = c.Package
				return &assessment, nil
			}
		}
	}

	// Deterministic heuristic fallback
	assessment := &MaintenanceAssessment{
		Package: c.Package,
	}

	if c.Archived {
		assessment.AbandonmentRisk = "critical"
		assessment.BusFactorWarning = "Repository is officially archived upstream by maintainers."
		assessment.MaintainerHealth = "Archived repository; no future security patches or maintenance will be provided."
		assessment.Recommendation = "Plan immediate migration to an active fork or supported alternative."
		return assessment, nil
	}

	if c.Health != nil && core.IsAbandonmentRisk(c.Health) {
		assessment.AbandonmentRisk = "critical"
		assessment.BusFactorWarning = fmt.Sprintf("High bus factor (%d%%) with only %d contributor(s) and low weekly downloads (%s).",
			busPct, c.ContributorCount, core.FormatCount(c.WeeklyDownloads))
		assessment.MaintainerHealth = "Fragile solo-maintainer setup with low community adoption to absorb maintenance if maintainer departs."
		assessment.Recommendation = "Isolate behind an internal adapter or vendor in-house to protect against sudden upstream abandonment."
	} else if c.BusFactor >= 0.7 && c.ContributorCount <= 5 {
		assessment.AbandonmentRisk = "moderate"
		assessment.BusFactorWarning = fmt.Sprintf("Top contributor authored %d%% of recent commits across %d total contributors.", busPct, c.ContributorCount)
		assessment.MaintainerHealth = "Maintenance concentrated among very few individuals."
		assessment.Recommendation = "Monitor release cadence and consider evaluating alternative community-backed packages."
	} else {
		assessment.AbandonmentRisk = "low"
		assessment.BusFactorWarning = fmt.Sprintf("Healthy contributor distribution (bus factor %d%% across %d contributors).", busPct, c.ContributorCount)
		assessment.MaintainerHealth = fmt.Sprintf("Active maintenance with %s weekly downloads.", core.FormatCount(c.WeeklyDownloads))
		assessment.Recommendation = "No immediate maintenance action required; keep dependency updated."
	}

	return assessment, nil
}

// AnalyzeAll evaluates multiple candidates concurrently.
func (a *MaintenanceAgent) AnalyzeAll(ctx context.Context, candidates []RiskCandidate) []*MaintenanceAssessment {
	results, _ := RunParallel(ctx, candidates, 3, func(c context.Context, item RiskCandidate) (*MaintenanceAssessment, error) {
		return a.Analyze(c, item)
	})
	return results
}
