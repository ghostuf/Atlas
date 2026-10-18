package main

// GitHub Issue Agent — creates structured GitHub issues for dependency
// vulnerabilities and risks found by Atlas's health and usage pipeline.
//
// Design:
//   - Two-phase: suggest_github_issues (preview + model asks user) then
//     create_github_issues (creates only after explicit user confirmation).
//   - Issues are fully self-contained: problem description, affected files
//     (from the usage scan), blast-radius metrics (from the graph), and a
//     proposed remediation.
//   - Deduplication: before creating, Atlas searches the target repo's
//     existing issues for a matching atlas:pkg + atlas:advisory fingerprint
//     embedded as an HTML comment, so calling create twice is safe.
//   - build-tooling and unused-candidate packages are never filed — too noisy.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"text/template"
	"time"
)

// ---------- risk classification ----------

// IssueSeverity is the triage level Atlas assigns to a filing candidate,
// blending OSV severity with usage classification so a CRITICAL advisory in
// a test-only package doesn't outrank a MODERATE one wired into production.
type IssueSeverity string

const (
	SeverityCritical IssueSeverity = "CRITICAL"
	SeverityHigh     IssueSeverity = "HIGH"
	SeverityModerate IssueSeverity = "MODERATE"
	SeverityLow      IssueSeverity = "LOW"
)

// IssueableRisk is one actionable finding: either a specific advisory on a
// package, or a structural risk (archived, abandonment) without an advisory.
type IssueableRisk struct {
	Package        string
	Version        string
	Classification string       // one of the Class* constants from report.go
	Advisory       *Advisory    // nil for structural risks (archived, abandonment)
	Health         *Health      // full health record — bus factor, contributor count, etc.
	Usage          *Usage       // nil if usage scan didn't run
	UsedIn         string       // pre-rendered from usageSummary()
	BlastRadius    int          // Node.TransitiveDependents
	RiskType       string       // "advisory" | "archived" | "abandonment"
	EffectiveSev   IssueSeverity // blended: considers usage class
}

// ---------- triage ----------

// blendSeverity downgrades the raw OSV severity based on usage class —
// a CRITICAL advisory in a test-only package is a real finding but much less
// urgent than the same advisory in production code.
func blendSeverity(rawSev string, class string) IssueSeverity {
	sev := IssueSeverity(rawSev)
	if sev == "" {
		sev = SeverityLow // UNRATED treated as LOW for triage
	}
	switch class {
	case ClassRuntime, ClassConfigReferenced:
		return sev // no downgrade for production usage
	case ClassTestOnly:
		// downgrade one step for test-only
		switch sev {
		case SeverityCritical:
			return SeverityHigh
		case SeverityHigh:
			return SeverityModerate
		default:
			return SeverityLow
		}
	default:
		// build-tooling / unused-candidate: never filed, but return LOW for completeness
		return SeverityLow
	}
}

// shouldFile returns true if a risk is worth opening a GitHub issue for.
// Build-tooling and unused-candidate packages are excluded — too noisy and
// too likely to be false positives. Advisory threshold is MODERATE for
// runtime, HIGH for test-only (filing a MODERATE issue on a test package
// would drown out real signal).
func shouldFile(r IssueableRisk) bool {
	switch r.Classification {
	case ClassBuildTooling, ClassUnusedCandidate, ClassNotScanned:
		return false
	}

	switch r.RiskType {
	case "advisory":
		switch r.EffectiveSev {
		case SeverityCritical, SeverityHigh:
			return true
		case SeverityModerate:
			return r.Classification == ClassRuntime || r.Classification == ClassConfigReferenced
		default:
			return false
		}
	case "archived":
		// Archived repo used in production: always file.
		return r.Classification == ClassRuntime || r.Classification == ClassConfigReferenced
	case "abandonment":
		// Only file if blast radius is significant and it's production-facing.
		return r.BlastRadius >= 3 &&
			(r.Classification == ClassRuntime || r.Classification == ClassConfigReferenced)
	}
	return false
}

// collectRisks walks the health rows from a session and returns every
// IssueableRisk that passes triage. The graph is needed to look up blast
// radius (TransitiveDependents) per package.
func collectRisks(health []*Health, usageRows []*Usage, g *Graph) []IssueableRisk {
	// Build lookup maps.
	byPkg := map[string]*Usage{}
	for _, u := range usageRows {
		byPkg[u.Package] = u
	}
	blastByPkg := map[string]int{}
	for _, n := range g.Nodes {
		if n.ID == "" {
			continue
		}
		if cur, ok := blastByPkg[n.Name]; !ok || n.TransitiveDependents > cur {
			blastByPkg[n.Name] = n.TransitiveDependents
		}
	}

	scanned := usageRows != nil
	var risks []IssueableRisk

	for _, h := range health {
		if h.Material {
			continue // skip packages we couldn't evaluate
		}
		u := byPkg[h.Package]
		class := classifyUsage(scanned, h.Dev, u)
		used := usageSummary(u)
		blast := blastByPkg[h.Package]

		// --- advisory risks ---
		for i := range h.Advisories {
			adv := h.Advisories[i]
			eff := blendSeverity(adv.Severity, class)
			r := IssueableRisk{
				Package: h.Package, Version: h.Version,
				Classification: class, Advisory: &h.Advisories[i],
				Health: h, Usage: u, UsedIn: used,
				BlastRadius: blast, RiskType: "advisory", EffectiveSev: eff,
			}
			if shouldFile(r) {
				risks = append(risks, r)
			}
		}

		// --- archived risk ---
		if h.Archived {
			r := IssueableRisk{
				Package: h.Package, Version: h.Version,
				Classification: class, Health: h, Usage: u, UsedIn: used,
				BlastRadius: blast, RiskType: "archived", EffectiveSev: SeverityHigh,
			}
			if shouldFile(r) {
				risks = append(risks, r)
			}
		}

		// --- abandonment risk ---
		if isAbandonmentRisk(h) {
			r := IssueableRisk{
				Package: h.Package, Version: h.Version,
				Classification: class, Health: h, Usage: u, UsedIn: used,
				BlastRadius: blast, RiskType: "abandonment", EffectiveSev: SeverityModerate,
			}
			if shouldFile(r) {
				risks = append(risks, r)
			}
		}
	}

	// Sort: advisory > archived > abandonment, then by effective severity, then by blast radius.
	sevOrder := map[IssueSeverity]int{SeverityCritical: 0, SeverityHigh: 1, SeverityModerate: 2, SeverityLow: 3}
	typeOrder := map[string]int{"advisory": 0, "archived": 1, "abandonment": 2}
	sort.SliceStable(risks, func(i, j int) bool {
		a, b := risks[i], risks[j]
		if typeOrder[a.RiskType] != typeOrder[b.RiskType] {
			return typeOrder[a.RiskType] < typeOrder[b.RiskType]
		}
		if sevOrder[a.EffectiveSev] != sevOrder[b.EffectiveSev] {
			return sevOrder[a.EffectiveSev] < sevOrder[b.EffectiveSev]
		}
		return a.BlastRadius > b.BlastRadius
	})
	return risks
}

// ---------- issue body rendering ----------

// IssueDraft is a fully-rendered GitHub issue ready to POST (or preview).
type IssueDraft struct {
	Title       string
	Body        string
	Labels      []string
	Fingerprint string // "pkg::advisory" or "pkg::archived" — used for dedup search
	Risk        IssueableRisk
}

var issueBodyTmpl = template.Must(template.New("issue").Parse(`## Atlas Dependency Risk Report

<!-- atlas:fingerprint={{.Fingerprint}} -->

> **Generated by [Atlas](https://github.com/your-org/atlas)** — automated dependency risk analysis  
> **Scanned at:** {{.Date}}

---

### 📦 Package

| Field | Value |
|-------|-------|
| **Name** | ` + "`{{.Package}}`" + ` |
| **Installed Version** | ` + "`{{.Version}}`" + ` |
| **Classification** | {{.Classification}} |
| **GitHub Repo** | {{if .GithubRepo}}[{{.GithubRepo}}](https://github.com/{{.GithubRepo}}){{else}}—{{end}} |

---

### ⚠️ Finding — {{.RiskHeading}}

{{.FindingBody}}

---

### 📍 Where It's Used in This Project

{{if .UsedIn}}
{{.UsedIn}}
{{else}}
_Usage scan was not available for this analysis run._
{{end}}

---

### 💥 Blast Radius

**{{.BlastRadius}} transitive dependent(s)** in the dependency graph will be affected if this package breaks or is removed.

{{if .BlastDetail}}{{.BlastDetail}}{{end}}

---

### 🩹 Proposed Remediation

{{.Remediation}}

---

### 📚 References

{{range .References}}- {{.}}
{{end}}

---

<details>
<summary>Atlas health signals</summary>

| Signal | Value |
|--------|-------|
| Weekly Downloads | {{.WeeklyDownloads}} |
| Last Publish | {{.LastPublish}} |
| Last Commit | {{.LastCommit}} |
| Contributors | {{.ContributorCount}} |
| Bus Factor | {{.BusFactor}} |
| Open Issues | {{.OpenIssues}} |
| Archived | {{.Archived}} |

</details>
`))

type issueTemplateData struct {
	Fingerprint    string
	Date           string
	Package        string
	Version        string
	Classification string
	GithubRepo     string
	RiskHeading    string
	FindingBody    string
	UsedIn         string
	BlastRadius    int
	BlastDetail    string
	Remediation    string
	References     []string
	WeeklyDownloads string
	LastPublish    string
	LastCommit     string
	ContributorCount int
	BusFactor      string
	OpenIssues     int
	Archived       bool
}

func renderIssueTitle(r IssueableRisk) string {
	switch r.RiskType {
	case "advisory":
		sev := string(r.EffectiveSev)
		if r.Advisory.Severity == "" {
			sev = "UNRATED"
		}
		return fmt.Sprintf("[atlas] %s advisory in %s@%s (%s)", sev, r.Package, r.Version, r.Advisory.ID)
	case "archived":
		return fmt.Sprintf("[atlas] Archived dependency: %s@%s", r.Package, r.Version)
	case "abandonment":
		return fmt.Sprintf("[atlas] Abandonment risk: %s@%s (bus factor %d%%)", r.Package, r.Version, int(r.Health.BusFactor*100))
	}
	return fmt.Sprintf("[atlas] Dependency risk: %s@%s", r.Package, r.Version)
}

func renderFingerprint(r IssueableRisk) string {
	if r.Advisory != nil {
		return r.Package + "::" + r.Advisory.ID
	}
	return r.Package + "::" + r.RiskType
}

func renderFindingBody(r IssueableRisk) string {
	switch r.RiskType {
	case "advisory":
		adv := r.Advisory
		sev := adv.Severity
		if sev == "" {
			sev = "UNRATED"
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("**Advisory ID:** `%s`  \n", adv.ID))
		sb.WriteString(fmt.Sprintf("**Severity:** %s (effective for this project: %s)  \n", sev, string(r.EffectiveSev)))
		if adv.Summary != "" {
			sb.WriteString(fmt.Sprintf("**Summary:** %s  \n", adv.Summary))
		}
		if !adv.Published.IsZero() {
			sb.WriteString(fmt.Sprintf("**Published:** %s  \n", adv.Published.Format("2006-01-02")))
		}
		if len(adv.FixedIn) > 0 {
			sb.WriteString(fmt.Sprintf("**Fixed In:** %s  \n", strings.Join(adv.FixedIn, ", ")))
		} else {
			sb.WriteString("**Fixed In:** _No fix published yet_  \n")
		}
		return sb.String()

	case "archived":
		return fmt.Sprintf(
			"The GitHub repository for `%s` (**%s**) has been **archived** by its maintainer. "+
				"Archived repos receive no further updates, security patches, or bug fixes. "+
				"Continued use of an archived package is a long-term maintenance liability.",
			r.Package, r.Health.GithubRepo)

	case "abandonment":
		h := r.Health
		return fmt.Sprintf(
			"`%s` shows signs of **maintainer abandonment**: the top contributor (**%s**) "+
				"accounts for **%d%%** of all commits in the last 12 months, "+
				"there are only **%d contributor(s)** total, and the package sees only **%s downloads/week**. "+
				"If this maintainer steps away, there is little community capacity to pick it up.",
			r.Package, h.TopContributor, int(h.BusFactor*100), h.ContributorCount, formatCount(h.WeeklyDownloads))
	}
	return ""
}

func renderRemediation(r IssueableRisk) string {
	switch r.RiskType {
	case "advisory":
		adv := r.Advisory
		if len(adv.FixedIn) > 0 {
			return fmt.Sprintf(
				"1. **Upgrade `%s`** to version `%s` or later — this is the version OSV lists as patched.\n"+
					"2. Run `npm audit` after upgrading to confirm no remaining advisories.\n"+
					"3. Review all usage sites listed above and verify the new version's API is compatible.",
				r.Package, adv.FixedIn[0])
		}
		return fmt.Sprintf(
			"1. **No patch is currently available** for `%s` (%s).\n"+
				"2. Evaluate whether the vulnerable code path is reachable from your usage (see usage sites above).\n"+
				"3. Consider a runtime mitigation (input validation, sandboxing) or a fork/alternative package.\n"+
				"4. Subscribe to the advisory at https://osv.dev/vulnerability/%s for patch notifications.",
			r.Package, adv.ID, adv.ID)

	case "archived":
		return fmt.Sprintf(
			"1. **Plan a migration away from `%s`** — the repository is archived and will receive no further updates.\n"+
				"2. Identify an actively maintained alternative (check npm's \"similar packages\" or search GitHub).\n"+
				"3. Prioritize migration based on how deeply the package is used (see blast radius above).\n"+
				"4. In the meantime, pin the current version and monitor for security advisories via `npm audit`.",
			r.Package)

	case "abandonment":
		return fmt.Sprintf(
			"1. **Audit your dependency on `%s`** — with a single dominant contributor and few others, "+
				"a maintainer departure would leave the package unpatched.\n"+
				"2. If `%s` is load-bearing (see blast radius above), consider whether a fork or alternative exists.\n"+
				"3. Pin the version in your lockfile and set up automated security scanning (GitHub Dependabot, Snyk) "+
				"to catch any advisories early.\n"+
				"4. Consider contributing to the project or reaching out to the maintainer about succession plans.",
			r.Package, r.Package)
	}
	return "Review and remediate based on the findings above."
}

func renderReferences(r IssueableRisk) []string {
	var refs []string
	if r.Advisory != nil {
		refs = append(refs, fmt.Sprintf("OSV advisory: https://osv.dev/vulnerability/%s", r.Advisory.ID))
		refs = append(refs, fmt.Sprintf("GitHub advisory search: https://github.com/advisories?query=%s", url.QueryEscape(r.Package)))
	}
	if r.Health.GithubRepo != "" {
		refs = append(refs, fmt.Sprintf("Package repo: https://github.com/%s", r.Health.GithubRepo))
	}
	refs = append(refs, fmt.Sprintf("npm registry: https://www.npmjs.com/package/%s", url.PathEscape(r.Package)))
	return refs
}

func renderBlastDetail(r IssueableRisk) string {
	if r.BlastRadius == 0 {
		return "This package has no known transitive dependents in the graph — impact is limited to its direct callers."
	}
	if r.BlastRadius >= 10 {
		return fmt.Sprintf("⚠️ This package is **highly load-bearing**: %d packages transitively depend on it. "+
			"A breaking change or security incident would have a wide blast radius across the codebase.", r.BlastRadius)
	}
	return ""
}

func renderIssueDraft(r IssueableRisk) (IssueDraft, error) {
	h := r.Health
	data := issueTemplateData{
		Fingerprint:      renderFingerprint(r),
		Date:             time.Now().Format("2006-01-02 15:04 MST"),
		Package:          r.Package,
		Version:          r.Version,
		Classification:   r.Classification,
		GithubRepo:       h.GithubRepo,
		RiskHeading:      riskHeading(r),
		FindingBody:      renderFindingBody(r),
		UsedIn:           r.UsedIn,
		BlastRadius:      r.BlastRadius,
		BlastDetail:      renderBlastDetail(r),
		Remediation:      renderRemediation(r),
		References:       renderReferences(r),
		WeeklyDownloads:  formatCount(h.WeeklyDownloads),
		LastPublish:      dateOrDash(h.LastPublish),
		LastCommit:       dateOrDash(h.LastCommit),
		ContributorCount: h.ContributorCount,
		BusFactor:        busFactorStr(h),
		OpenIssues:       h.OpenIssues,
		Archived:         h.Archived,
	}

	var buf strings.Builder
	if err := issueBodyTmpl.Execute(&buf, data); err != nil {
		return IssueDraft{}, fmt.Errorf("render issue body for %s: %w", r.Package, err)
	}

	labels := []string{"atlas-security", "dependencies"}
	switch r.RiskType {
	case "advisory":
		labels = append(labels, "vulnerability")
		if r.EffectiveSev == SeverityCritical || r.EffectiveSev == SeverityHigh {
			labels = append(labels, "priority:high")
		}
	case "archived":
		labels = append(labels, "maintenance")
	case "abandonment":
		labels = append(labels, "maintenance", "bus-factor")
	}

	return IssueDraft{
		Title:       renderIssueTitle(r),
		Body:        buf.String(),
		Labels:      labels,
		Fingerprint: data.Fingerprint,
		Risk:        r,
	}, nil
}

func riskHeading(r IssueableRisk) string {
	switch r.RiskType {
	case "advisory":
		sev := string(r.EffectiveSev)
		if r.Advisory.Severity == "" {
			sev = "UNRATED"
		}
		return fmt.Sprintf("%s Security Advisory (%s)", sev, r.Advisory.ID)
	case "archived":
		return "Archived Repository — No Further Updates"
	case "abandonment":
		return fmt.Sprintf("Maintainer Abandonment Risk (bus factor %d%%)", int(r.Health.BusFactor*100))
	}
	return "Dependency Risk"
}

// ---------- GitHub Issues API ----------

// issueAlreadyExists searches the target repo's open AND closed issues for
// any that contain the Atlas fingerprint comment. This makes deduplication
// resilient to title changes and to issues that were closed and re-opened.
func issueAlreadyExists(targetRepo, fingerprint string) (exists bool, issueNumber int, err error) {
	// GitHub issue search: look for the fingerprint string in issue bodies.
	q := fmt.Sprintf(`repo:%s is:issue "atlas:fingerprint=%s"`, targetRepo, fingerprint)
	u := "https://api.github.com/search/issues?q=" + url.QueryEscape(q) + "&per_page=5"

	body, status, err := httpGet(u, githubHeaders())
	if err != nil {
		return false, 0, fmt.Errorf("GitHub issue search: %w", err)
	}
	if status == 403 || status == 429 {
		return false, 0, githubRateLimitErr(status)
	}
	if status != 200 {
		return false, 0, fmt.Errorf("GitHub issue search: %s", http.StatusText(status))
	}

	var result struct {
		TotalCount int `json:"total_count"`
		Items      []struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
			State  string `json:"state"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return false, 0, fmt.Errorf("parse GitHub issue search response: %w", err)
	}
	if result.TotalCount > 0 {
		return true, result.Items[0].Number, nil
	}
	return false, 0, nil
}

type githubCreateIssueRequest struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels"`
}

type githubCreatedIssue struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

// createGithubIssue POSTs a new issue to the target repo and returns its URL.
func createGithubIssue(targetRepo string, draft IssueDraft) (githubCreatedIssue, error) {
	payload := githubCreateIssueRequest{
		Title:  draft.Title,
		Body:   draft.Body,
		Labels: draft.Labels,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return githubCreatedIssue{}, err
	}
	u := fmt.Sprintf("https://api.github.com/repos/%s/issues", targetRepo)
	req, err := http.NewRequest("POST", u, bytes.NewReader(b))
	if err != nil {
		return githubCreatedIssue{}, err
	}
	for k, v := range githubHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return githubCreatedIssue{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 403 || resp.StatusCode == 429 {
		return githubCreatedIssue{}, githubRateLimitErr(resp.StatusCode)
	}
	if resp.StatusCode == 401 {
		return githubCreatedIssue{}, fmt.Errorf("GitHub returned 401 Unauthorized — your GITHUB_TOKEN may lack 'repo' scope (needed to create issues). Update it under Settings > Developer settings > Personal access tokens")
	}
	if resp.StatusCode == 404 {
		return githubCreatedIssue{}, fmt.Errorf("repo %q not found or not accessible with your GITHUB_TOKEN", targetRepo)
	}
	if resp.StatusCode != 201 {
		return githubCreatedIssue{}, fmt.Errorf("GitHub create issue: %s", http.StatusText(resp.StatusCode))
	}

	var created githubCreatedIssue
	bodyBytes, _ := readAll(resp)
	if err := json.Unmarshal(bodyBytes, &created); err != nil {
		return githubCreatedIssue{}, fmt.Errorf("parse create issue response: %w", err)
	}
	return created, nil
}

// readAll reads from http.Response body into a []byte (avoids importing io
// again at the top — we already pull it in health.go in the same package).
func readAll(resp *http.Response) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(resp.Body)
	return buf.Bytes(), err
}

// ---------- summary formatting ----------

// FormatDraftSummary returns a human-readable preview of all planned issues,
// used by the agent tool to show the user what will be filed before asking
// for confirmation.
func FormatDraftSummary(drafts []IssueDraft) string {
	if len(drafts) == 0 {
		return "No fiileable issues found. All risks are either below the severity threshold, limited to build/test dependencies, or already filed."
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Atlas identified **%d issue(s)** to file:\n\n", len(drafts)))
	for i, d := range drafts {
		r := d.Risk
		sb.WriteString(fmt.Sprintf("%d. **%s** — %s\n", i+1, d.Title, riskSummaryLine(r)))
	}
	return sb.String()
}

func riskSummaryLine(r IssueableRisk) string {
	var parts []string
	if r.Advisory != nil {
		sev := r.Advisory.Severity
		if sev == "" {
			sev = "UNRATED"
		}
		parts = append(parts, fmt.Sprintf("Advisory: %s [%s]", r.Advisory.ID, sev))
		if len(r.Advisory.FixedIn) > 0 {
			parts = append(parts, "fix available: "+r.Advisory.FixedIn[0])
		} else {
			parts = append(parts, "no fix yet")
		}
	} else {
		parts = append(parts, r.RiskType)
	}
	parts = append(parts, fmt.Sprintf("blast radius %d", r.BlastRadius))
	parts = append(parts, r.Classification)
	if r.UsedIn != "" {
		parts = append(parts, "used in: "+r.UsedIn)
	}
	return strings.Join(parts, " · ")
}
