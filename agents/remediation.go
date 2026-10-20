package agents

import (
	"atlas/core"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// RemediationAgent synthesizes concrete migration and fix plans for high-risk dependencies.
type RemediationAgent struct {
	model   string
	limiter *ConcurrencyLimiter
}

// NewRemediationAgent creates a Remediation Agent.
func NewRemediationAgent(model string, limiter *ConcurrencyLimiter) *RemediationAgent {
	return &RemediationAgent{
		model:   model,
		limiter: limiter,
	}
}

func (a *RemediationAgent) Name() string {
	return "Remediation Agent"
}

func (a *RemediationAgent) Description() string {
	return "Generates concrete migration paths, version upgrades, and mitigation plans for identified risks."
}

// Synthesize produces a tailored remediation plan for a candidate based on findings.
func (a *RemediationAgent) Synthesize(
	ctx context.Context,
	c RiskCandidate,
	coupling *CouplingAssessment,
	vuln *VulnerabilityAssessment,
	maint *MaintenanceAssessment,
) (*RemediationPlan, error) {
	// If it's an unused candidate, remediation is simple: remove it!
	if c.Classification == core.ClassUnusedCandidate {
		return &RemediationPlan{
			Package:         c.Package,
			SuggestedFix:    fmt.Sprintf("Remove %q from %s — zero source or config references found.", c.Package, c.Ecosystem.ManifestName()),
			AlternativeLibs: nil,
			MigrationEffort: "trivial",
			ActionRequired:  fmt.Sprintf("Run: %s", c.Ecosystem.RemoveCommand(c.Package)),
		}, nil
	}

	// Collect advisory fixes
	var fixedVersions []string
	for _, adv := range c.Advisories {
		if len(adv.FixedIn) > 0 {
			fixedVersions = append(fixedVersions, adv.FixedIn...)
		}
	}

	// Try LLM schema-constrained evaluation if model is configured
	if a.model != "" {
		schema := map[string]any{
			"type": "object",
			"properties": map[string]any{
				"suggested_fix":    map[string]any{"type": "string"},
				"alternative_libs": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"migration_effort": map[string]any{"type": "string", "enum": []string{"trivial", "moderate", "architectural_refactor"}},
				"action_required":  map[string]any{"type": "string"},
			},
			"required": []string{"suggested_fix", "alternative_libs", "migration_effort", "action_required"},
		}

		couplingInfo := "unknown"
		if coupling != nil {
			couplingInfo = fmt.Sprintf("%s (%s)", coupling.CouplingLevel, coupling.ArchitecturalRole)
		}
		vulnInfo := "none"
		if vuln != nil {
			vulnInfo = fmt.Sprintf("%s urgency, %s exploitability", vuln.Urgency, vuln.Exploitability)
		}
		maintInfo := "stable"
		if maint != nil {
			maintInfo = fmt.Sprintf("%s abandonment risk", maint.AbandonmentRisk)
		}

		prompt := fmt.Sprintf(
			"Synthesize a concrete remediation and migration plan for %s package %q:\n"+
				"Installed version: %s\n"+
				"Fixed versions published: %s\n"+
				"Architectural coupling: %s\n"+
				"Vulnerability profile: %s\n"+
				"Maintenance sustainability: %s\n"+
				"Where used in project: %s\n\n"+
				"Provide a concrete fix, recommend 1-3 modern alternatives or native APIs, determine migration effort, and state the exact action required.",
			c.Ecosystem.DisplayName(), c.Package, c.Version, strings.Join(fixedVersions, ", "), couplingInfo, vulnInfo, maintInfo, c.UsedIn,
		)

		var respText string
		var err error
		a.limiter.Run(func() {
			respText, err = core.ChatOllamaJSON(a.model, []core.OllamaMessage{
				{Role: "system", Content: "You are an expert dependency migration and security remediation engineer."},
				{Role: "user", Content: prompt},
			}, schema)
		})

		if err == nil && respText != "" {
			var plan RemediationPlan
			if jsonErr := json.Unmarshal([]byte(respText), &plan); jsonErr == nil {
				plan.Package = c.Package
				return &plan, nil
			}
		}
	}

	// Deterministic heuristic fallback
	plan := &RemediationPlan{
		Package: c.Package,
	}

	if len(fixedVersions) > 0 {
		plan.SuggestedFix = fmt.Sprintf("Upgrade %s to version %s to resolve open advisories.", c.Package, fixedVersions[len(fixedVersions)-1])
		plan.MigrationEffort = "trivial"
		plan.ActionRequired = fmt.Sprintf("Run: %s", c.Ecosystem.InstallCommand(c.Package, fixedVersions[len(fixedVersions)-1]))
	} else if c.Archived {
		plan.SuggestedFix = fmt.Sprintf("Package %s is archived upstream. Migrate to an actively maintained alternative.", c.Package)
		plan.MigrationEffort = "architectural_refactor"
		plan.ActionRequired = fmt.Sprintf("Audit usages across %s and replace with modern maintained alternatives.", c.UsedIn)
	} else if maint != nil && maint.AbandonmentRisk == "critical" {
		plan.SuggestedFix = fmt.Sprintf("Wrap %s behind an internal facade/adapter to decouple application logic from this fragile dependency.", c.Package)
		plan.MigrationEffort = "moderate"
		plan.ActionRequired = "Create an internal interface wrapper around direct import calls."
	} else {
		plan.SuggestedFix = fmt.Sprintf("Keep %s updated to the latest minor/patch release and monitor security channels.", c.Package)
		plan.MigrationEffort = "trivial"
		plan.ActionRequired = "Check for upstream updates periodically."
	}

	return plan, nil
}

// SynthesizeAll synthesizes remediation plans concurrently across candidates.
func (a *RemediationAgent) SynthesizeAll(
	ctx context.Context,
	candidates []RiskCandidate,
	couplings map[string]*CouplingAssessment,
	vulns map[string]*VulnerabilityAssessment,
	maints map[string]*MaintenanceAssessment,
) []*RemediationPlan {
	results, _ := RunParallel(ctx, candidates, 3, func(c context.Context, item RiskCandidate) (*RemediationPlan, error) {
		return a.Synthesize(c, item, couplings[item.Package], vulns[item.Package], maints[item.Package])
	})
	return results
}
