package agents

import (
	"atlas/core"
	"context"
)

// Agent is the common interface implemented by all agents in Atlas.
type Agent interface {
	Name() string
	Description() string
}

// RiskCandidate represents a dependency identified as needing deep analysis.
type RiskCandidate struct {
	Package          string
	Version          string
	Ecosystem        core.Ecosystem
	Dev              bool
	Classification   string
	BlastRadius      int
	Dependents       int
	Depth            int
	UsedIn           string
	Usage            *core.Usage
	Health           *core.Health
	Archived         bool
	BusFactor        float64
	ContributorCount int
	WeeklyDownloads  int
	OpenIssues       int
	Advisories       []core.Advisory
}

// CouplingAssessment is the structured output from the Coupling Agent.
type CouplingAssessment struct {
	Package           string `json:"package"`
	CouplingLevel     string `json:"coupling_level"`     // "low", "medium", "high"
	ArchitecturalRole string `json:"architectural_role"` // e.g. "core infrastructure", "surface utility", "middleware"
	Reason            string `json:"reason"`
	IsLoadBearing     bool   `json:"is_load_bearing"`
}

// VulnerabilityAssessment is the structured output from the Vulnerability Agent.
type VulnerabilityAssessment struct {
	Package            string `json:"package"`
	ContextualSeverity string `json:"contextual_severity"` // "CRITICAL", "HIGH", "MODERATE", "LOW"
	Exploitability     string `json:"exploitability"`      // "reachable_in_prod", "test_only", "isolated", "unreachable"
	Summary            string `json:"summary"`
	Urgency            string `json:"urgency"` // "immediate", "next_sprint", "backlog", "monitor"
}

// MaintenanceAssessment is the structured output from the Maintenance Agent.
type MaintenanceAssessment struct {
	Package          string `json:"package"`
	AbandonmentRisk  string `json:"abandonment_risk"` // "critical", "moderate", "low"
	BusFactorWarning string `json:"bus_factor_warning"`
	MaintainerHealth string `json:"maintainer_health"`
	Recommendation   string `json:"recommendation"`
}

// RemediationPlan is the structured output from the Remediation Agent.
type RemediationPlan struct {
	Package         string   `json:"package"`
	SuggestedFix    string   `json:"suggested_fix"`
	AlternativeLibs []string `json:"alternative_libs"`
	MigrationEffort string   `json:"migration_effort"` // "trivial", "moderate", "architectural_refactor"
	ActionRequired  string   `json:"action_required"`
}

// DeepRiskProfile merges the findings of all specialist agents for a candidate package.
type DeepRiskProfile struct {
	Candidate     RiskCandidate            `json:"candidate"`
	Coupling      *CouplingAssessment      `json:"coupling,omitempty"`
	Vulnerability *VulnerabilityAssessment `json:"vulnerability,omitempty"`
	Maintenance   *MaintenanceAssessment   `json:"maintenance,omitempty"`
	Remediation   *RemediationPlan         `json:"remediation,omitempty"`
}

// IssueSeverity is the triage level Atlas assigns to a filing candidate.
type IssueSeverity string

const (
	SeverityCritical IssueSeverity = "CRITICAL"
	SeverityHigh     IssueSeverity = "HIGH"
	SeverityModerate IssueSeverity = "MODERATE"
	SeverityLow      IssueSeverity = "LOW"
)

// IssueableRisk is one actionable finding ready for issue triaging.
type IssueableRisk struct {
	Package        string
	Version        string
	Ecosystem      core.Ecosystem
	Classification string
	Advisory       *core.Advisory
	Health         *core.Health
	Usage          *core.Usage
	UsedIn         string
	BlastRadius    int
	RiskType       string
	EffectiveSev   IssueSeverity
}

// IssueDraft is a fully-rendered GitHub issue ready to POST (or preview).
type IssueDraft struct {
	Title       string
	Body        string
	Labels      []string
	Fingerprint string
	Risk        IssueableRisk
}

// ParallelAgentTask defines an executable work unit for the parallel executor.
type ParallelAgentTask[T any, R any] struct {
	Ctx    context.Context
	Input  T
	Worker func(ctx context.Context, input T) (R, error)
}
