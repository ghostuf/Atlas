package agents

import (
	"atlas/core"
	"atlas/core/python"
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var askForInputRe = regexp.MustCompile(`(?i)(please provide|provide either|once you provide|i need access to the project|in the format owner/name|not (been )?received|have not (been )?(received|provided)|tool output|cannot (provide|write|generate)|i require the output|provide the results)`)

const reportDir = "reports"

const systemPrompt = `You are Atlas, an orchestrating dependency risk analysis agent.

You have tools to analyze a project's npm or Python (poetry) dependency graph. For any request
about analyzing, auditing, or checking a repository's dependencies:

1. Always call analyze_repository first. It needs either a GitHub repo as
   "owner/name" (e.g. "cloudflare/mcp") or a local lockfile path. If the user
   hasn't given you one, ask them for it before calling any tool — don't guess.
   The interactive dependency graph opens in the browser immediately, and a
   parallel multi-agent risk evaluation (architectural coupling, vulnerability
   severity, maintenance sustainability, and remediation planning) runs
   automatically across all identified high-risk dependencies.
2. Call scan_usage/check_health again yourself only to go further than the
   default: scan_usage with a specific "package" to see its exact usage
   sites, or check_health with "top" to extend coverage beyond direct
   dependencies.
3. Once you're done calling tools and are ready to give your final answer, a
   combined HTML report (graph + usage scan + health check + multi-agent
   assessments + your own answer) is generated and opened automatically.
   Write your final answer as a clear assessment: what's load-bearing,
   what's risky, and concretely what to do about it. You can mention that
   issues could be filed for the worst findings, but don't worry about
   stating an exact count — the CLI automatically prints a separate,
   precisely-computed offer to the user right after your answer (something
   like "Atlas found N issue(s) worth filing... say the word"), so you don't
   need to carry that number yourself. Do not call suggest_github_issues
   yet — wait for the user's reply to that offer.
4. When judging abandonment/bus-factor risk, read bus factor together with
   contributor count and weekly downloads.
5. GitHub Issue Creation — three-step, always requires two explicit user
   confirmations before anything is written to GitHub:
   STEP 1 — the user says yes to your offer (or asks to file issues directly):
     Call suggest_github_issues. It triages health results into structured
     issue drafts and checks for duplicates concurrently. Relay its output
     verbatim-ish as your final answer: the exact count and list of issues
     it would create, then ask again: "Shall I go ahead and create these N
     issues in <repo>? (yes/no)" Do NOT call create_github_issues yet.
   STEP 2 — the user confirms that list:
     Call create_github_issues. It creates issues concurrently with
     rate-limiting and returns a per-issue created/skipped/failed result.
     If the user only wants a subset (e.g. "just the top 2", "only the
     critical ones"), you MUST pass that number as the count argument —
     the pending list from suggest_github_issues is already sorted
     severity-first, so count takes exactly the first N of it. Never just
     say in words that you'll only create some of them while leaving count
     unset — that creates ALL pending drafts regardless of what you said.
   STEP 3 — report the outcome in the chat, not the report:
     Your final answer after create_github_issues must restate the result
     directly to the user in plain language (how many created, with links;
     how many skipped as duplicates; how many failed and why) — this does
     NOT go into the HTML report, so say it plainly in the chat yourself.
     Each failure line already states what GitHub reported and the next
     step. Relay it faithfully and do not add causes it doesn't state:
     HTTP 401 means GitHub rejected the token itself (invalid/expired), HTTP
     403 means access was refused (a missing permission, or a rate limit if
     the line says so), and HTTP 404 means the repo wasn't found or isn't
     visible to the token. Never call create_github_issues again on the same
     drafts without a fresh explicit yes from the user.`

// Session tracks the active analysis state in a conversation.
type Session struct {
	Graph      *core.Graph
	Adapter    core.EcosystemAdapter // set once in AnalyzeRepository, reused by ScanUsage/CheckHealth
	SrcDir     string
	Repo       string
	GithubRepo string // owner/name; empty when analysis came from a local lockfile, not a GitHub repo
	Cleanup    func()

	Usage  core.UsageScan
	Health []*core.Health

	Profiles        []DeepRiskProfile
	IssueDrafts     []IssueDraft
	IssueTargetRepo string
}

func (s *Session) Reset() {
	if s.Cleanup != nil {
		s.Cleanup()
	}
	s.Graph, s.Adapter, s.SrcDir, s.Repo, s.GithubRepo, s.Cleanup = nil, nil, "", "", "", nil
	s.Usage, s.Health = core.UsageScan{}, nil
	s.Profiles, s.IssueDrafts, s.IssueTargetRepo = nil, nil, ""
}

// OrchestratorAgent coordinates the multi-agent pipeline and user conversation.
type OrchestratorAgent struct {
	model      string
	couplingAg *CouplingAgent
	vulnAg     *VulnerabilityAgent
	maintAg    *MaintenanceAgent
	remedAg    *RemediationAgent
	actionAg   *ActionAgent
	limiter    *ConcurrencyLimiter
	sess       *Session

	errOut io.Writer // where user-facing errors are shown (stderr)
	failed bool      // a tool reported a failure to the user this turn
}

// NewOrchestratorAgent creates a generalist Orchestrator with parallel specialist agents.
func NewOrchestratorAgent(model string) *OrchestratorAgent {
	limiter := NewConcurrencyLimiter(3)
	return &OrchestratorAgent{
		model:      model,
		couplingAg: NewCouplingAgent(model, limiter),
		vulnAg:     NewVulnerabilityAgent(model, limiter),
		maintAg:    NewMaintenanceAgent(model, limiter),
		remedAg:    NewRemediationAgent(model, limiter),
		actionAg:   NewActionAgent(limiter),
		limiter:    limiter,
		sess:       &Session{},
		errOut:     os.Stderr,
	}
}

// fail shows err to the user directly, exactly as written, and gives the
// model only a terse result. Routing the full text through the model let it
// paraphrase and speculate about causes Atlas had not established.
func (o *OrchestratorAgent) fail(err error) string {
	fmt.Fprintln(o.errOut, core.FormatError(err))
	o.failed = true
	return "FAILED: " + core.Redact(err.Error()) +
		". Atlas has already shown the user the full error and next steps. Do not add explanations or guesses about the cause."
}

func (o *OrchestratorAgent) Name() string {
	return "Orchestrator Agent"
}

func (o *OrchestratorAgent) Description() string {
	return "Orchestrates multi-agent dependency risk analysis, coordinating parallel specialist agents and action dispatch."
}

// AnalyzeRepository executes the deterministic baseline and dispatches parallel specialist agents.
func (o *OrchestratorAgent) AnalyzeRepository(repo, file, ref, dir string) string {
	if repo == "" && file == "" {
		return "error: specify either a GitHub repo as owner/name, or a local lockfile path"
	}

	var data []byte
	var lockName string
	fetchErr := core.WithSpinner("Fetching lockfile", func() error {
		var err error
		if file != "" {
			data, err = core.ReadLocalFile(file, "lockfile")
			lockName = filepath.Base(file)
		} else {
			data, lockName, err = core.FetchLockfile(repo, ref, dir)
		}
		return err
	})
	if fetchErr != nil {
		return o.fail(fetchErr)
	}
	o.sess.Reset()

	var g *core.Graph
	var adapter core.EcosystemAdapter = core.NpmAdapter{}
	buildErr := core.WithSpinner("Analyzing dependencies", func() error {
		var err error
		switch core.DetectEcosystem(lockName) {
		case core.EcosystemPoetry:
			adapter = python.Adapter{}
			g, err = python.BuildPoetryGraph(data, repo, ref, dir, file)
			return err
		default:
			var lf core.Lockfile
			if err := json.Unmarshal(data, &lf); err != nil {
				src := "package-lock.json from " + repo
				if file != "" {
					src = file
				}
				return core.LockfileParseError(err, src)
			}
			if len(lf.Packages) == 0 && len(lf.V1Deps) > 0 {
				pj, err := core.LoadPackageJSON(repo, ref, dir, file)
				if err != nil {
					return core.Annotate(err, "This is a version 1 lockfile, which does not list the project's direct dependencies, so Atlas also needs its package.json.")
				}
				core.ConvertV1(&lf, pj)
			}
			g, err = core.BuildGraph(&lf)
			return err
		}
	})
	if buildErr != nil {
		return o.fail(buildErr)
	}
	_, edges, direct := core.RankNodes(g)
	core.Detail("%d packages, %d edges, %d direct dependencies", len(g.Nodes), edges, direct)

	o.sess.Graph = g
	o.sess.Adapter = adapter
	o.sess.GithubRepo = repo
	if repo != "" {
		o.sess.Repo = repo
	} else {
		o.sess.Repo = file
	}

	vizPath := filepath.Join(reportDir, "graph.html")
	vizNote := ""
	_ = core.WithSpinner("Creating interactive graph", func() error {
		if err := core.WriteViz(g, vizPath, 150); err != nil {
			vizNote = "(interactive graph failed: " + err.Error() + ")"
			return err
		}
		vizNote = "Wrote an interactive graph to " + vizPath
		core.OpenInBrowser(vizPath)
		return nil
	})
	core.Detail("%s", vizPath)

	cloneErr := core.WithSpinner("Cloning repository", func() error {
		if file != "" {
			o.sess.SrcDir = filepath.Dir(file)
			return nil
		}
		d, cleanup, err := core.CloneRepo(repo, ref)
		if err != nil {
			return err
		}
		o.sess.SrcDir = filepath.Join(d, dir)
		o.sess.Cleanup = cleanup
		return nil
	})

	report := core.GraphReport(g, 15)
	var usageText string
	if cloneErr != nil {
		// Previously discarded: the user saw a bare ✗ and the usage scan
		// silently never ran. Show why, and say what is missing.
		fmt.Fprintln(o.errOut, core.FormatError(cloneErr))
		usageText = "usage scan skipped: the repository could not be cloned (the error was shown to the user); health results below have no usage context"
	} else {
		usageText = o.ScanUsage("")
	}
	healthJSON := o.CheckHealth(0)

	// ---- Step 4: Parallel Agentic Analysis ----
	agentSummary := o.RunParallelSpecialists()

	return report + "\n" + vizNote + "\n" + usageText + "\n" + healthJSON + "\n\n" + agentSummary
}

// RunParallelSpecialists identifies candidate packages and executes specialist agents in parallel.
func (o *OrchestratorAgent) RunParallelSpecialists() string {
	if o.sess.Graph == nil || o.sess.Health == nil {
		return ""
	}

	// 1. Identify candidates needing deep agent analysis
	clean, _ := core.SplitHealthProblems(o.sess.Health)
	byPkgUsage := map[string]*core.Usage{}
	for _, u := range o.sess.Usage.Rows {
		byPkgUsage[u.Package] = u
	}
	blastMap := map[string]int{}
	for _, n := range o.sess.Graph.Nodes {
		if n.ID != "" && n.TransitiveDependents > blastMap[n.Name] {
			blastMap[n.Name] = n.TransitiveDependents
		}
	}

	var candidates []RiskCandidate
	for _, h := range clean {
		u := byPkgUsage[h.Package]
		class := core.ClassifyUsage(o.sess.Usage.Rows != nil, h.Dev, u)
		blast := blastMap[h.Package]
		hasAdv := len(h.Advisories) > 0
		isAband := core.IsAbandonmentRisk(h)

		// Candidate if: has advisories, archived, abandonment risk, unused-candidate, or high blast radius
		if hasAdv || h.Archived || isAband || class == core.ClassUnusedCandidate || blast >= 5 {
			candidates = append(candidates, RiskCandidate{
				Package:          h.Package,
				Version:          h.Version,
				Ecosystem:        o.sess.Graph.Ecosystem,
				Dev:              h.Dev,
				Classification:   class,
				BlastRadius:      blast,
				UsedIn:           core.UsageSummary(u),
				Usage:            u,
				Health:           h,
				Archived:         h.Archived,
				BusFactor:        h.BusFactor,
				ContributorCount: h.ContributorCount,
				WeeklyDownloads:  h.WeeklyDownloads,
				OpenIssues:       h.OpenIssues,
				Advisories:       h.Advisories,
			})
		}
	}

	if len(candidates) == 0 {
		return "Multi-Agent Risk Analysis: No severe structural or vulnerability risks flagged across dependencies."
	}

	// Limit to top 8 candidates to keep analysis snappy
	if len(candidates) > 8 {
		sort.SliceStable(candidates, func(i, j int) bool {
			return candidates[i].BlastRadius > candidates[j].BlastRadius
		})
		candidates = candidates[:8]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var couplings []*CouplingAssessment
	var vulns []*VulnerabilityAssessment
	var maints []*MaintenanceAssessment

	_ = core.WithSpinner(fmt.Sprintf("Running parallel multi-agent analysis across %d high-risk dependencies", len(candidates)), func() error {
		// Execute Coupling, Vulnerability, and Maintenance specialists in parallel!
		RunConcurrentStages(ctx,
			func(c context.Context) error {
				couplings = o.couplingAg.AnalyzeAll(c, candidates)
				return nil
			},
			func(c context.Context) error {
				vulns = o.vulnAg.AnalyzeAll(c, candidates)
				return nil
			},
			func(c context.Context) error {
				maints = o.maintAg.AnalyzeAll(c, candidates)
				return nil
			},
		)
		return nil
	})

	couplingMap := map[string]*CouplingAssessment{}
	for _, c := range couplings {
		if c != nil {
			couplingMap[c.Package] = c
		}
	}
	vulnMap := map[string]*VulnerabilityAssessment{}
	for _, v := range vulns {
		if v != nil {
			vulnMap[v.Package] = v
		}
	}
	maintMap := map[string]*MaintenanceAssessment{}
	for _, m := range maints {
		if m != nil {
			maintMap[m.Package] = m
		}
	}

	// Now run Remediation Agent concurrently across candidates informed by the other agents
	var remediations []*RemediationPlan
	_ = core.WithSpinner("Synthesizing concrete remediation plans", func() error {
		remediations = o.remedAg.SynthesizeAll(ctx, candidates, couplingMap, vulnMap, maintMap)
		return nil
	})

	remedMap := map[string]*RemediationPlan{}
	for _, r := range remediations {
		if r != nil {
			remedMap[r.Package] = r
		}
	}

	// Assemble final DeepRiskProfiles
	profiles := make([]DeepRiskProfile, len(candidates))
	for i, cand := range candidates {
		profiles[i] = DeepRiskProfile{
			Candidate:     cand,
			Coupling:      couplingMap[cand.Package],
			Vulnerability: vulnMap[cand.Package],
			Maintenance:   maintMap[cand.Package],
			Remediation:   remedMap[cand.Package],
		}
	}
	o.sess.Profiles = profiles

	// Format text summary of multi-agent findings for model context
	var sb strings.Builder
	sb.WriteString("=== Multi-Agent Deep Risk & Remediation Profiles (Evaluated in Parallel) ===\n")
	for _, p := range profiles {
		sb.WriteString(fmt.Sprintf("\n• Package: %s@%s (Blast Radius: %d, Classification: %s)\n",
			p.Candidate.Package, p.Candidate.Version, p.Candidate.BlastRadius, p.Candidate.Classification))
		if p.Coupling != nil {
			sb.WriteString(fmt.Sprintf("  - Architectural Coupling: [%s] %s — %s\n",
				strings.ToUpper(p.Coupling.CouplingLevel), p.Coupling.ArchitecturalRole, p.Coupling.Reason))
		}
		if p.Vulnerability != nil && p.Vulnerability.Summary != "" {
			sb.WriteString(fmt.Sprintf("  - Security Vulnerability: Contextual Sev: %s, Exploitability: %s, Urgency: %s\n",
				p.Vulnerability.ContextualSeverity, p.Vulnerability.Exploitability, p.Vulnerability.Urgency))
		}
		if p.Maintenance != nil {
			sb.WriteString(fmt.Sprintf("  - Maintainer Health: Abandonment Risk: %s, Warning: %s\n",
				strings.ToUpper(p.Maintenance.AbandonmentRisk), p.Maintenance.BusFactorWarning))
		}
		if p.Remediation != nil {
			sb.WriteString(fmt.Sprintf("  - Recommended Action: %s (Effort: %s)\n    Fix: %s\n",
				p.Remediation.ActionRequired, p.Remediation.MigrationEffort, p.Remediation.SuggestedFix))
		}
	}

	core.Detail("%d deep risk profiles synthesized across 4 parallel specialist agents", len(profiles))
	return sb.String()
}

func (o *OrchestratorAgent) ScanUsage(pkg string) string {
	if o.sess.Graph == nil {
		return "error: call analyze_repository first"
	}
	if o.sess.SrcDir == "" {
		return "error: no cloned source available for usage scanning"
	}
	var text string
	_ = core.WithSpinner("Scanning usage", func() error {
		var result core.UsageScan
		text, result = core.RunScan(o.sess.Graph, o.sess.SrcDir, pkg, false, o.sess.Adapter)
		o.sess.Usage = result
		return nil
	})
	_, _, direct := core.RankNodes(o.sess.Graph)
	core.Detail("%d source files — %d/%d direct dependencies actually imported", o.sess.Usage.Scanned, len(o.sess.Usage.Rows), direct)
	return text
}

func (o *OrchestratorAgent) CheckHealth(top int) string {
	if o.sess.Graph == nil {
		return "error: call analyze_repository first"
	}
	var rows []*core.Health
	_ = core.WithSpinner(fmt.Sprintf("Checking dependency health (%s, GitHub, OSV.dev)", o.sess.Graph.Ecosystem.RegistryName()), func() error {
		rows = core.RunHealth(o.sess.Graph, top, o.sess.Adapter)
		return nil
	})
	o.sess.Health = rows
	clean, problems := core.SplitHealthProblems(rows)
	advisories := 0
	for _, h := range clean {
		advisories += len(h.Advisories)
	}
	score := core.ComputeRepoScore(clean, o.sess.Usage.Rows, o.sess.Usage.Rows != nil)
	core.Detail("%d packages checked — %d advisories found — %d could not be evaluated — score %d/100 (%s)",
		len(clean), advisories, len(problems), score.Value, score.Label)

	byPkg := map[string]*core.Usage{}
	for _, u := range o.sess.Usage.Rows {
		byPkg[u.Package] = u
	}

	type compactHealth struct {
		Package          string   `json:"package"`
		Version          string   `json:"version"`
		Dev              bool     `json:"dev,omitempty"`
		Classification   string   `json:"classification"`
		UsedInProject    string   `json:"usedInProject"`
		Archived         bool     `json:"archived,omitempty"`
		WeeklyDownloads  int      `json:"weeklyDownloads,omitempty"`
		ContributorCount int      `json:"contributorCount,omitempty"`
		BusFactorPct     int      `json:"busFactorPct,omitempty"`
		LastCommit       string   `json:"lastCommit,omitempty"`
		LastPublish      string   `json:"lastPublish,omitempty"`
		OpenIssues       int      `json:"openIssues,omitempty"`
		AdvisoryCount    int      `json:"advisoryCount,omitempty"`
		TopAdvisories    []string `json:"topAdvisories,omitempty"`
	}

	out := make([]compactHealth, len(clean))
	for i, h := range clean {
		u := byPkg[h.Package]
		usedIn := "usage not scanned"
		if o.sess.Usage.Rows != nil {
			usedIn = core.UsageSummary(u)
		}
		c := compactHealth{
			Package: h.Package, Version: h.Version, Dev: h.Dev,
			Classification: core.ClassifyUsage(o.sess.Usage.Rows != nil, h.Dev, u),
			UsedInProject:  usedIn, Archived: h.Archived,
			WeeklyDownloads: h.WeeklyDownloads, ContributorCount: h.ContributorCount,
			BusFactorPct: int(h.BusFactor*100 + 0.5), OpenIssues: h.OpenIssues,
			AdvisoryCount: len(h.Advisories),
		}
		if !h.LastCommit.IsZero() {
			c.LastCommit = core.DateOrDash(h.LastCommit)
		}
		if !h.LastPublish.IsZero() {
			c.LastPublish = core.DateOrDash(h.LastPublish)
		}
		advs := append([]core.Advisory(nil), h.Advisories...)
		sort.SliceStable(advs, func(i, j int) bool {
			return core.SeverityWeight(advs[i].Severity) > core.SeverityWeight(advs[j].Severity)
		})
		for k, a := range advs {
			if k >= 5 {
				break
			}
			sev := a.Severity
			if sev == "" {
				sev = "UNRATED"
			}
			line := a.ID + " [" + sev + "]"
			if len(a.FixedIn) > 0 {
				line += " fixed in " + a.FixedIn[0]
			} else {
				line += " no fix published"
			}
			c.TopAdvisories = append(c.TopAdvisories, line)
		}
		out[i] = c
	}

	b, _ := json.Marshal(out)
	result := fmt.Sprintf("Overall dependency health score: %d/100 (%s).\n\n", score.Value, score.Label) + string(b)
	return result
}

// SuggestIssues triages findings into drafts with concurrent duplicate checking.
func (o *OrchestratorAgent) SuggestIssues(targetRepo string) string {
	if o.sess.Graph == nil {
		return "error: call analyze_repository first"
	}
	if o.sess.Health == nil {
		return "error: health data not available — call check_health first"
	}
	if targetRepo == "" {
		return "error: target_repo is required (owner/name)"
	}

	o.sess.IssueTargetRepo = targetRepo
	clean, _ := core.SplitHealthProblems(o.sess.Health)
	risks := CollectRisks(clean, o.sess.Usage.Rows, o.sess.Graph)

	if len(risks) == 0 {
		o.sess.IssueDrafts = nil
		return "No fileable issues found. All identified risks are below the filing threshold or have no findings."
	}

	// Render drafts
	var rawDrafts []IssueDraft
	for _, r := range risks {
		draft, err := RenderIssueDraft(r)
		if err == nil {
			rawDrafts = append(rawDrafts, draft)
		}
	}

	// Run concurrent duplicate check across all drafts
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var existing map[string]int
	_ = core.WithSpinner("Checking for existing duplicate issues concurrently", func() error {
		existing = CheckDuplicatesConcurrently(ctx, targetRepo, rawDrafts, 3)
		return nil
	})

	var pendingDrafts []IssueDraft
	alreadyCount := 0
	for _, d := range rawDrafts {
		if _, ok := existing[d.Fingerprint]; ok {
			alreadyCount++
		} else {
			pendingDrafts = append(pendingDrafts, d)
		}
	}

	o.sess.IssueDrafts = pendingDrafts

	if len(pendingDrafts) == 0 {
		return fmt.Sprintf("All %d identified issue(s) already exist in %s. No new issues need to be filed.",
			alreadyCount, targetRepo)
	}

	var sb strings.Builder
	sb.WriteString(FormatDraftSummary(pendingDrafts))
	if alreadyCount > 0 {
		sb.WriteString(fmt.Sprintf("\n(Note: %d existing issue(s) were found and excluded from this list.)\n", alreadyCount))
	}
	sb.WriteString(fmt.Sprintf("\nTarget repo for issues: **%s**\n", targetRepo))
	sb.WriteString("\n**Ask the user if they want to proceed with creating these issues.**")
	return sb.String()
}

// CreateIssues creates stored drafts concurrently with rate-limiting. If count
// is > 0 and smaller than the number of pending drafts, only the first count
// (the list is already sorted severity-first) are created; the rest stay
// pending in the session so a later "create the rest" doesn't need a fresh
// suggest_github_issues call.
func (o *OrchestratorAgent) CreateIssues(count int) string {
	if len(o.sess.IssueDrafts) == 0 {
		return "error: no issue drafts to create — call suggest_github_issues first"
	}
	if o.sess.IssueTargetRepo == "" {
		return "error: target repo not set — call suggest_github_issues first"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	drafts := o.sess.IssueDrafts
	if count > 0 && count < len(drafts) {
		drafts = drafts[:count]
		o.sess.IssueDrafts = o.sess.IssueDrafts[count:] // remainder stays pending
	} else {
		o.sess.IssueDrafts = nil // Clear drafts so re-calling doesn't duplicate
	}

	results := o.actionAg.CreateIssuesConcurrently(ctx, o.sess.IssueTargetRepo, drafts)

	var sb strings.Builder
	created, skipped, failed := 0, 0, 0
	for _, r := range results {
		if r.Err != "" {
			failed++
			sb.WriteString(fmt.Sprintf("❌ FAILED  — %s\n   error: %s\n", r.Title, r.Err))
		} else if r.Skipped {
			skipped++
			sb.WriteString(fmt.Sprintf("⏭  SKIPPED — %s\n   already exists: %s/issues/%d\n",
				r.Title, "https://github.com/"+o.sess.IssueTargetRepo, r.SkipNum))
		} else {
			created++
			sb.WriteString(fmt.Sprintf("✅ CREATED — %s\n   %s\n", r.Title, r.URL))
		}
	}
	sb.WriteString(fmt.Sprintf("\nSummary: %d created · %d skipped · %d failed\n", created, skipped, failed))
	if remaining := len(o.sess.IssueDrafts); remaining > 0 {
		sb.WriteString(fmt.Sprintf("\n%d issue(s) from the original list were left out and are still pending — say the word if you want those created too.\n", remaining))
	}
	return sb.String()
}

func (o *OrchestratorAgent) WriteSessionReport(reasoning string) (string, error) {
	list, edges, direct := core.RankNodes(o.sess.Graph)
	data := core.ReportData{
		Title:       o.sess.Repo,
		GeneratedAt: time.Now().Format("2006-01-02 15:04:05"),
		GraphFile:   "graph.html",
		Vertices:    len(o.sess.Graph.Nodes),
		Edges:       edges,
		DirectCount: direct,
		GraphTotal:  len(list),
		GraphRows:   core.ToReportGraphRows(list, core.ReportGraphRows),
		Reasoning:   core.RenderReasoning(reasoning),
	}
	if o.sess.Usage.Rows != nil {
		data.UsageRoot = o.sess.Usage.Root
		data.UsageScanned = o.sess.Usage.Scanned
		data.UsageRows = core.ToReportUsageRows(o.sess.Usage.Rows)
		data.UnusedDeps = o.sess.Usage.Unused
	}
	if o.sess.Health != nil {
		clean, problems := core.SplitHealthProblems(o.sess.Health)
		data.HealthRows = core.ToReportHealthRows(clean, o.sess.Usage.Rows)
		data.ProblemRows = core.ToReportProblemRows(problems)
		data.Score = core.ComputeRepoScore(clean, o.sess.Usage.Rows, o.sess.Usage.Rows != nil)
		data.ScoreGauge = core.RenderGaugeSVG(data.Score)
	}

	path := filepath.Join(reportDir, "report.html")
	vizPath := filepath.Join(reportDir, "graph.html")
	if o.sess.Graph != nil {
		_ = core.WriteViz(o.sess.Graph, vizPath, 150)
	}
	if err := core.WriteReport(path, data); err != nil {
		return "", err
	}
	core.OpenInBrowser(path)
	return path, nil
}

// AgentTools returns tool definitions for the Orchestrator.
func (o *OrchestratorAgent) AgentTools() []core.OllamaTool {
	return []core.OllamaTool{
		core.NewTool("analyze_repository",
			"Clone a GitHub repo (or read a local lockfile), parse its lockfile (npm package-lock.json or Python poetry.lock), build the dependency graph, compute blast-radius metrics, and dispatch parallel specialist agents for coupling, security, maintenance, and remediation. Call this first for any repository analysis request.",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"repo": map[string]any{"type": "string", "description": "GitHub repo as owner/name, e.g. cloudflare/mcp"},
					"file": map[string]any{"type": "string", "description": "path to a local lockfile (package-lock.json or poetry.lock)"},
					"ref":  map[string]any{"type": "string", "description": "branch, tag, or commit SHA (optional)"},
					"dir":  map[string]any{"type": "string", "description": "subdirectory for monorepos (optional)"},
				},
			}),
		core.NewTool("scan_usage",
			"Find where direct dependencies are actually imported in the source code. Requires analyze_repository to have run.",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"package": map[string]any{"type": "string", "description": "optional package name to show sampled usage snippets for"},
				},
			}),
		core.NewTool("check_health",
			"Re-run or extend the dependency health check. Optional top to check additional transitive dependencies by blast radius.",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"top": map[string]any{"type": "integer", "description": "check this many extra packages ranked by blast radius"},
				},
			}),
		core.NewTool("suggest_github_issues",
			"Triage findings, check for duplicate issues concurrently on GitHub, and render issue drafts. ALWAYS call this and ask user confirmation before create_github_issues.",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target_repo": map[string]any{"type": "string", "description": "GitHub repo to file issues in, as owner/name"},
				},
				"required": []string{"target_repo"},
			}),
		core.NewTool("create_github_issues",
			"POST pre-rendered issue drafts to GitHub with concurrency rate-limiting. Call ONLY after explicit user confirmation.",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"count": map[string]any{"type": "integer", "description": "only create this many issues, taking the first N from the pending list (already sorted severity-first); omit or 0 to create all pending drafts"},
				},
			}),
	}
}

func (o *OrchestratorAgent) DispatchTool(tc core.OllamaToolCall) string {
	switch tc.Function.Name {
	case "analyze_repository":
		var args struct {
			Repo string `json:"repo"`
			File string `json:"file"`
			Ref  string `json:"ref"`
			Dir  string `json:"dir"`
		}
		_ = json.Unmarshal(tc.Function.Arguments, &args)
		return o.AnalyzeRepository(args.Repo, args.File, args.Ref, args.Dir)
	case "scan_usage":
		var args struct {
			Package string `json:"package"`
		}
		_ = json.Unmarshal(tc.Function.Arguments, &args)
		return o.ScanUsage(args.Package)
	case "check_health":
		var args struct {
			Top int `json:"top"`
		}
		_ = json.Unmarshal(tc.Function.Arguments, &args)
		return o.CheckHealth(args.Top)
	case "suggest_github_issues":
		var args struct {
			TargetRepo string `json:"target_repo"`
		}
		_ = json.Unmarshal(tc.Function.Arguments, &args)
		return o.SuggestIssues(args.TargetRepo)
	case "create_github_issues":
		var args struct {
			Count int `json:"count"`
		}
		_ = json.Unmarshal(tc.Function.Arguments, &args)
		return o.CreateIssues(args.Count)
	default:
		return "unknown tool: " + tc.Function.Name
	}
}

// promptGithubToken asks for a token when none is configured. It does not
// validate or exit: an empty answer is reported by core.Preflight together
// with any other startup problem.
func promptGithubToken() {
	if os.Getenv("GITHUB_TOKEN") != "" {
		return
	}
	fmt.Println("Atlas needs a GitHub token to read private repositories, create issues, and avoid GitHub's low unauthenticated rate limit.")
	fmt.Print("GITHUB_TOKEN: ")
	reader := bufio.NewReader(os.Stdin)
	tok, _ := reader.ReadString('\n')
	if tok = strings.TrimSpace(tok); tok != "" {
		os.Setenv("GITHUB_TOKEN", tok)
	}
}

// RunAgentMode starts the interactive REPL with the orchestrator. If repo is
// non-empty (e.g. from `-repo owner/name` on the command line), it is
// analyzed immediately, as if the user had typed "run dependency analysis on
// <repo>" as their first message, before the REPL takes over.
//
// Startup validation order is documented on core.Preflight: configuration,
// then GitHub, then Ollama. All independent failures are shown together and
// the REPL does not start if any is blocking.
func RunAgentMode(model, repo, ref, dir string) {
	promptGithubToken()

	pre := core.Preflight(core.PreflightOptions{
		RequireGithubToken: true,
		CheckGithub:        true,
		CheckOllama:        true,
		Model:              model,
	})
	for _, w := range pre.Warnings {
		fmt.Fprintln(os.Stderr, core.FormatWarning(w))
	}
	if len(pre.Blocking) > 0 {
		fmt.Fprintln(os.Stderr, core.FormatErrors(pre.Blocking, core.PreflightHeader))
		os.Exit(1)
	}

	fmt.Printf("Atlas — dependency risk analysis agent (model: %s)\n", model)
	fmt.Println(`Try: "run dependency analysis on cloudflare/mcp". Type "exit" to quit.`)

	orch := NewOrchestratorAgent(model)
	defer orch.sess.Reset()

	history := []core.OllamaMessage{{Role: "system", Content: systemPrompt}}
	tools := orch.AgentTools()

	if repo != "" {
		seed := "Run dependency analysis on " + repo
		if ref != "" {
			seed += " (ref: " + ref + ")"
		}
		if dir != "" {
			seed += " (dir: " + dir + ")"
		}
		fmt.Println("\nyou> " + seed)
		history = append(history, core.OllamaMessage{Role: "user", Content: seed})
		if err := orch.RunTurn(model, &history, tools); err != nil {
			fmt.Fprintln(os.Stderr, core.FormatError(err))
		}
	}

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("\nyou> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			break
		}

		history = append(history, core.OllamaMessage{Role: "user", Content: line})
		if err := orch.RunTurn(model, &history, tools); err != nil {
			fmt.Fprintln(os.Stderr, core.FormatError(err))
		}
	}
}

const maxToolHops = 6

// analysisTools are the tools whose output belongs in the regenerated HTML
// report. Action tools (issue suggest/create) are deliberately excluded: their
// status is conversational and must be read directly in the chat, not
// discovered by re-opening a silently-rewritten report.
var analysisTools = map[string]bool{
	"analyze_repository": true,
	"scan_usage":         true,
	"check_health":       true,
}

func (o *OrchestratorAgent) RunTurn(model string, history *[]core.OllamaMessage, tools []core.OllamaTool) error {
	toolsRan := false
	analysisRan := false
	retried := false
	for hop := 0; hop < maxToolHops; hop++ {
		think := core.StartSpinner("Thinking")
		msg, err := core.ChatOllama(model, *history, tools)
		think.StopSilent()
		if err != nil {
			return err
		}
		*history = append(*history, msg)

		if len(msg.ToolCalls) == 0 {
			answer := strings.TrimSpace(msg.Content)
			if toolsRan && o.sess.Graph != nil && askForInputRe.MatchString(answer) {
				if !retried {
					retried = true
					*history = append(*history, core.OllamaMessage{
						Role:    "user",
						Content: "The analysis tools have already run and their results are above. Write the final assessment now, based on that tool output.",
					})
					continue
				}
				answer = "(the model did not produce an assessment; see the tables above)"
			}

			if analysisRan && o.sess.Graph != nil {
				o.announceReport(answer)
			} else {
				fmt.Println("\natlas> " + core.StripInlineMarkdown(answer))
			}
			return nil
		}

		toolsRan = true
		calls := append([]core.OllamaToolCall(nil), msg.ToolCalls...)
		sort.SliceStable(calls, func(i, j int) bool {
			return calls[i].Function.Name == "analyze_repository" && calls[j].Function.Name != "analyze_repository"
		})
		for _, tc := range calls {
			if analysisTools[tc.Function.Name] {
				analysisRan = true
			}
			result := o.DispatchTool(tc)
			*history = append(*history, core.OllamaMessage{Role: "tool", Content: result})
		}
		if o.failed {
			// The user has already been shown the failure verbatim; don't
			// give the model another turn to restate or speculate about it.
			o.failed = false
			return nil
		}
	}

	fmt.Println("\natlas> (stopped after several tool calls without a final answer — try a more specific question)")
	if analysisRan && o.sess.Graph != nil {
		o.announceReport("(stopped after several tool calls without a final answer)")
	}
	return nil
}

func (o *OrchestratorAgent) announceReport(reasoning string) {
	path, err := o.WriteSessionReport(reasoning)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: the HTML report could not be written:", core.Redact(err.Error()))
		return
	}
	fmt.Printf("\n✓ Prepared report: %s — open in your browser to inspect interactive graphs and multi-agent risk profiles.\n", path)
	o.printIssueOffer()
}

// printIssueOffer prints a deterministic, code-computed nudge to the chat
// (never the report) offering to file GitHub issues, when there's anything
// fileable. This doesn't depend on the model remembering to mention it in
// its free-form answer — that text only ever ends up in the report, not
// the terminal, so the offer would otherwise go unseen.
func (o *OrchestratorAgent) printIssueOffer() {
	if o.sess.GithubRepo == "" || o.sess.Health == nil || len(o.sess.IssueDrafts) > 0 {
		return
	}
	clean, _ := core.SplitHealthProblems(o.sess.Health)
	risks := CollectRisks(clean, o.sess.Usage.Rows, o.sess.Graph)
	if len(risks) == 0 {
		return
	}
	counts := map[IssueSeverity]int{}
	for _, r := range risks {
		counts[r.EffectiveSev]++
	}
	var parts []string
	for _, sev := range []IssueSeverity{SeverityCritical, SeverityHigh, SeverityModerate, SeverityLow} {
		if counts[sev] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[sev], strings.ToLower(string(sev))))
		}
	}
	fmt.Printf("\n💡 Atlas found %d issue(s) worth filing in %s (%s). Say the word and I'll draft and create them as GitHub issues.\n",
		len(risks), o.sess.GithubRepo, strings.Join(parts, ", "))
}
