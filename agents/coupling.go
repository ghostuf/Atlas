package agents

import (
	"atlas/core"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// CouplingAgent analyzes how deeply a dependency is wired into the system architecture.
type CouplingAgent struct {
	model   string
	limiter *ConcurrencyLimiter
}

// NewCouplingAgent creates an Architectural Coupling Agent.
func NewCouplingAgent(model string, limiter *ConcurrencyLimiter) *CouplingAgent {
	return &CouplingAgent{
		model:   model,
		limiter: limiter,
	}
}

func (a *CouplingAgent) Name() string {
	return "Architectural Coupling Agent"
}

func (a *CouplingAgent) Description() string {
	return "Evaluates how architecturally embedded a package is (surface-level utility vs load-bearing infrastructure)."
}

// Analyze evaluates a single candidate dependency's architectural coupling.
func (a *CouplingAgent) Analyze(ctx context.Context, c RiskCandidate) (*CouplingAssessment, error) {
	// 1. Gather structural signals
	prodCount := 0
	testCount := 0
	dirsCount := 0
	sitesCount := 0
	var tags []string

	if c.Usage != nil {
		prodCount = len(c.Usage.ProdFiles)
		testCount = len(c.Usage.TestFiles)
		dirsCount = len(c.Usage.Dirs)
		sitesCount = len(c.Usage.Sites)
		for t := range c.Usage.Tags {
			tags = append(tags, t)
		}
	}

	// 2. Try LLM schema-constrained evaluation if model is configured
	if a.model != "" && prodCount > 0 {
		schema := map[string]any{
			"type": "object",
			"properties": map[string]any{
				"coupling_level":     map[string]any{"type": "string", "enum": []string{"low", "medium", "high"}},
				"architectural_role": map[string]any{"type": "string"},
				"reason":             map[string]any{"type": "string"},
				"is_load_bearing":    map[string]any{"type": "boolean"},
			},
			"required": []string{"coupling_level", "architectural_role", "reason", "is_load_bearing"},
		}

		prompt := fmt.Sprintf(
			"Analyze the architectural coupling for npm package %q in this project.\n"+
				"Production files importing it: %d\n"+
				"Test files importing it: %d\n"+
				"Top-level directories touched: %d\n"+
				"Architectural domain tags: %s\n"+
				"Direct/Transitive dependents affected if it breaks: %d\n"+
				"Sample usage: %s\n\n"+
				"Classify whether this is low, medium, or high coupling, its role, and whether it is load-bearing infrastructure.",
			c.Package, prodCount, testCount, dirsCount, strings.Join(tags, ", "), c.BlastRadius, c.UsedIn,
		)

		var respText string
		var err error
		a.limiter.Run(func() {
			respText, err = core.ChatOllamaJSON(a.model, []core.OllamaMessage{
				{Role: "system", Content: "You are an expert software architect evaluating dependency coupling depth."},
				{Role: "user", Content: prompt},
			}, schema)
		})

		if err == nil && respText != "" {
			var assessment CouplingAssessment
			if jsonErr := json.Unmarshal([]byte(respText), &assessment); jsonErr == nil {
				assessment.Package = c.Package
				return &assessment, nil
			}
		}
	}

	// 3. Deterministic heuristic fallback
	assessment := &CouplingAssessment{
		Package: c.Package,
	}

	isCriticalTag := len(tags) > 0
	if prodCount >= 8 || (prodCount >= 3 && isCriticalTag) || c.BlastRadius >= 15 {
		assessment.CouplingLevel = "high"
		assessment.IsLoadBearing = true
		assessment.ArchitecturalRole = "Core Load-Bearing Infrastructure"
		assessment.Reason = fmt.Sprintf("Imported across %d production files spanning %d directories with %s tags; %d transitive dependents.",
			prodCount, dirsCount, strings.Join(tags, "/"), c.BlastRadius)
	} else if prodCount >= 2 || (prodCount == 1 && isCriticalTag) || c.BlastRadius >= 5 {
		assessment.CouplingLevel = "medium"
		assessment.IsLoadBearing = false
		assessment.ArchitecturalRole = "Moderate Integration Component"
		assessment.Reason = fmt.Sprintf("Imported in %d production files with %d call sites.", prodCount, sitesCount)
	} else if testCount > 0 && prodCount == 0 {
		assessment.CouplingLevel = "low"
		assessment.IsLoadBearing = false
		assessment.ArchitecturalRole = "Test-Only Utility"
		assessment.Reason = fmt.Sprintf("Imported only in %d test files, zero production usage.", testCount)
	} else {
		assessment.CouplingLevel = "low"
		assessment.IsLoadBearing = false
		assessment.ArchitecturalRole = "Surface Utility or Peripheral Tooling"
		assessment.Reason = "Minimal direct architectural embedding found in source code."
	}

	return assessment, nil
}

// AnalyzeAll evaluates multiple candidates concurrently.
func (a *CouplingAgent) AnalyzeAll(ctx context.Context, candidates []RiskCandidate) []*CouplingAssessment {
	results, _ := RunParallel(ctx, candidates, 3, func(c context.Context, item RiskCandidate) (*CouplingAssessment, error) {
		return a.Analyze(c, item)
	})
	return results
}
