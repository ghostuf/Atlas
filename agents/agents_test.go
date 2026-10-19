package agents

import (
	"atlas/core"
	"context"
	"testing"
	"time"
)

func TestSpecialistAgents(t *testing.T) {
	limiter := NewConcurrencyLimiter(2)
	ctx := context.Background()

	couplingAg := NewCouplingAgent("", limiter)
	vulnAg := NewVulnerabilityAgent("", limiter)
	maintAg := NewMaintenanceAgent("", limiter)
	remedAg := NewRemediationAgent("", limiter)

	candidate := RiskCandidate{
		Package:        "express",
		Version:        "4.18.2",
		Dev:            false,
		Classification: core.ClassRuntime,
		BlastRadius:    12,
		UsedIn:         "prod: src/app.ts, src/server.ts",
		Usage: &core.Usage{
			Package:   "express",
			ProdFiles: map[string]bool{"src/app.ts": true, "src/server.ts": true},
			TestFiles: map[string]bool{},
			Dirs:      map[string]bool{"src": true},
			Tags:      map[string]bool{"routing": true},
		},
		Health: &core.Health{
			Package:          "express",
			Version:          "4.18.2",
			TopContributor:   "dougwilson",
			BusFactor:        0.35,
			ContributorCount: 150,
			WeeklyDownloads:  25000000,
			LastPublish:      time.Now().AddDate(0, -2, 0),
			LastCommit:       time.Now().AddDate(0, -1, 0),
		},
		Advisories: []core.Advisory{
			{
				ID:       "GHSA-1234",
				Severity: "HIGH",
				FixedIn:  []string{"4.19.0"},
				Summary:  "Open redirect vulnerability",
			},
		},
	}

	// 1. Test Coupling Agent
	coupling, err := couplingAg.Analyze(ctx, candidate)
	if err != nil {
		t.Fatalf("coupling agent failed: %v", err)
	}
	if coupling.Package != "express" {
		t.Fatalf("expected package express, got %s", coupling.Package)
	}
	if coupling.CouplingLevel != "medium" && coupling.CouplingLevel != "high" {
		t.Fatalf("expected medium/high coupling, got %s", coupling.CouplingLevel)
	}

	// 2. Test Vulnerability Agent
	vuln, err := vulnAg.Analyze(ctx, candidate)
	if err != nil {
		t.Fatalf("vulnerability agent failed: %v", err)
	}
	if vuln.ContextualSeverity != "HIGH" {
		t.Fatalf("expected contextual severity HIGH in runtime context, got %s", vuln.ContextualSeverity)
	}
	if vuln.Exploitability != "reachable_in_prod" {
		t.Fatalf("expected reachable_in_prod, got %s", vuln.Exploitability)
	}

	// 3. Test Maintenance Agent
	maint, err := maintAg.Analyze(ctx, candidate)
	if err != nil {
		t.Fatalf("maintenance agent failed: %v", err)
	}
	if maint.AbandonmentRisk != "low" {
		t.Fatalf("expected low abandonment risk for express, got %s", maint.AbandonmentRisk)
	}

	// 4. Test Remediation Agent
	remed, err := remedAg.Synthesize(ctx, candidate, coupling, vuln, maint)
	if err != nil {
		t.Fatalf("remediation agent failed: %v", err)
	}
	if remed.Package != "express" {
		t.Fatalf("expected remediation for express, got %s", remed.Package)
	}
	if remed.MigrationEffort == "" || remed.SuggestedFix == "" {
		t.Fatalf("expected populated remediation plan, got %+v", remed)
	}
}

func TestActionAgentTriageAndDraft(t *testing.T) {
	risk := IssueableRisk{
		Package:        "lodash",
		Version:        "4.17.20",
		Classification: core.ClassRuntime,
		RiskType:       "advisory",
		EffectiveSev:   SeverityHigh,
		Advisory: &core.Advisory{
			ID:       "GHSA-p6mc-m468-83gw",
			Severity: "HIGH",
			FixedIn:  []string{"4.17.21"},
			Summary:  "Prototype Pollution in lodash",
		},
		Health: &core.Health{
			Package:          "lodash",
			WeeklyDownloads:  50000000,
			ContributorCount: 80,
		},
		BlastRadius: 5,
		UsedIn:      "prod: src/utils.ts",
	}

	draft, err := RenderIssueDraft(risk)
	if err != nil {
		t.Fatalf("RenderIssueDraft failed: %v", err)
	}

	if draft.Title == "" || draft.Body == "" || draft.Fingerprint == "" {
		t.Fatalf("draft missing fields: %+v", draft)
	}

	if len(draft.Labels) == 0 {
		t.Fatalf("draft should have labels")
	}
}
