package main

// Step 3: dependency health signals, no AI — just three free APIs plus one
// metric we compute ourselves (bus factor).
//
//   npm registry   -> maintainers, last publish, release cadence
//   GitHub REST API -> last commit, contributor count, archived flag, open issues
//   OSV.dev         -> known advisory history
//
// Bus factor = share of commits from the single most active contributor,
// over the trailing 12 months, derived from GitHub's per-contributor weekly
// commit stats.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- shared HTTP ----------

func httpGet(u string, headers map[string]string) ([]byte, int, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return body, resp.StatusCode, err
}

func githubHeaders() map[string]string {
	h := map[string]string{"Accept": "application/vnd.github+json"}
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		h["Authorization"] = "Bearer " + tok
	}
	return h
}

func githubRateLimitErr(status int) error {
	return fmt.Errorf("github API returned %s (set GITHUB_TOKEN to raise the rate limit)", http.StatusText(status))
}

// ---------- npm registry ----------

type npmPackument struct {
	// Maintainers is npm's publish-access list for this package — accounts
	// allowed to push new versions — NOT a measure of who writes code. A
	// solo maintainer with a huge, active GitHub contributor base (see
	// ContributorCount) is a very different risk than a solo maintainer with
	// neither contributors nor adoption; this field alone can't tell them
	// apart, which is why it's paired with ContributorCount and
	// WeeklyDownloads rather than read on its own.
	Maintainers []struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"maintainers"`
	Time       map[string]string `json:"time"` // version -> ISO8601; also "created"/"modified"
	Repository struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"repository"`
}

func fetchNpmPackument(name string) (*npmPackument, error) {
	u := "https://registry.npmjs.org/" + url.PathEscape(name)
	body, status, err := httpGet(u, nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("npm registry: %s", http.StatusText(status))
	}
	var pj npmPackument
	if err := json.Unmarshal(body, &pj); err != nil {
		return nil, fmt.Errorf("parse npm packument: %w", err)
	}
	return &pj, nil
}

// fetchNpmDownloads returns weekly download count from npm's public download
// counts API — a rough proxy for "how much replacement pressure exists if
// the maintainer disappears." A widely-downloaded package under-resourced on
// maintenance tends to get picked up by someone else (forks, new maintainers
// stepping in); an unpopular one usually doesn't.
func fetchNpmDownloads(name string) (int, error) {
	u := "https://api.npmjs.org/downloads/point/last-week/" + url.PathEscape(name)
	body, status, err := httpGet(u, nil)
	if err != nil {
		return 0, err
	}
	if status != 200 {
		return 0, fmt.Errorf("npm downloads: %s", http.StatusText(status))
	}
	var d struct {
		Downloads int `json:"downloads"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return 0, fmt.Errorf("parse npm downloads: %w", err)
	}
	return d.Downloads, nil
}

// npmPublishStats derives last-publish time, mean days between the most
// recent releases, and maintainer count from a packument.
func npmPublishStats(pj *npmPackument) (last time.Time, cadenceDays float64, maintainers int) {
	maintainers = len(pj.Maintainers)

	var versions []time.Time
	for k, v := range pj.Time {
		if k == "created" || k == "modified" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			versions = append(versions, t)
		}
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].Before(versions[j]) })

	if t, err := time.Parse(time.RFC3339, pj.Time["modified"]); err == nil {
		last = t
	} else if len(versions) > 0 {
		last = versions[len(versions)-1]
	}

	const window = 10
	if n := len(versions); n >= 2 {
		start := n - window
		if start < 0 {
			start = 0
		}
		recent := versions[start:]
		span := recent[len(recent)-1].Sub(recent[0])
		cadenceDays = span.Hours() / 24 / float64(len(recent)-1)
	}
	return
}

// resolveGithubRepo pulls owner/repo out of a packument's repository.url,
// handling the common forms npm packages use.
func resolveGithubRepo(pj *npmPackument) (owner, repo string, ok bool) {
	raw := pj.Repository.URL
	if raw == "" {
		return "", "", false
	}
	s := strings.TrimSuffix(strings.TrimPrefix(raw, "git+"), ".git")
	switch {
	case strings.HasPrefix(s, "github:"):
		s = "https://github.com/" + strings.TrimPrefix(s, "github:")
	case strings.HasPrefix(s, "git://github.com/"):
		s = "https://" + strings.TrimPrefix(s, "git://")
	case strings.HasPrefix(s, "git@github.com:"):
		s = "https://github.com/" + strings.TrimPrefix(s, "git@github.com:")
	}
	i := strings.Index(s, "github.com/")
	if i < 0 {
		return "", "", false
	}
	path := strings.Trim(s[i+len("github.com/"):], "/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// ---------- GitHub ----------

type ghRepo struct {
	Archived   bool   `json:"archived"`
	OpenIssues int    `json:"open_issues_count"`
	PushedAt   string `json:"pushed_at"`
}

func fetchGithubRepo(owner, repo string) (*ghRepo, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s", owner, repo)
	body, status, err := httpGet(u, githubHeaders())
	if err != nil {
		return nil, err
	}
	if status == 403 || status == 429 {
		return nil, githubRateLimitErr(status)
	}
	if status != 200 {
		return nil, fmt.Errorf("github repo: %s", http.StatusText(status))
	}
	var r ghRepo
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parse github repo: %w", err)
	}
	return &r, nil
}

type ghContributorStats struct {
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	Weeks []struct {
		W int64 `json:"w"` // unix timestamp, start of week
		C int   `json:"c"` // commits that week
	} `json:"weeks"`
}

// paginatedCount returns the true total item count for a paginated GitHub
// list endpoint by requesting one item per page and reading the page number
// out of the "last" rel in the Link header — avoids walking every page just
// to count them. If there's no Link header, everything fit on one page.
//
// This matters because several GitHub endpoints (notably stats/contributors)
// silently cap out around 100 results with no further pages exposed at all;
// the real count has to come from a genuinely paginated endpoint instead.
func paginatedCount(u string) (int, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return 0, err
	}
	for k, v := range githubHeaders() {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode == 403 || resp.StatusCode == 429 {
		return 0, githubRateLimitErr(resp.StatusCode)
	}
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("%s: %s", u, http.StatusText(resp.StatusCode))
	}
	if last := lastPageFromLink(resp.Header.Get("Link")); last > 0 {
		return last, nil // per_page=1, so the last page number equals the total count
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(body, &arr); err != nil {
		return 0, err
	}
	return len(arr), nil
}

func lastPageFromLink(link string) int {
	for _, part := range strings.Split(link, ",") {
		if !strings.Contains(part, `rel="last"`) {
			continue
		}
		start, end := strings.Index(part, "<"), strings.Index(part, ">")
		if start < 0 || end < 0 || end <= start {
			continue
		}
		parsed, err := url.Parse(part[start+1 : end])
		if err != nil {
			continue
		}
		if n, err := strconv.Atoi(parsed.Query().Get("page")); err == nil {
			return n
		}
	}
	return 0
}

// fetchGithubContributorStats can 202 while GitHub computes stats in the
// background; we retry once and otherwise give up rather than block the run.
// Note: this endpoint is used only for the weekly commit breakdown bus
// factor needs — it is NOT a reliable total contributor count (see
// paginatedCount above), since it caps out well short of the real number on
// busy repos.
func fetchGithubContributorStats(owner, repo string) ([]ghContributorStats, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/stats/contributors", owner, repo)
	for attempt := 0; attempt < 2; attempt++ {
		body, status, err := httpGet(u, githubHeaders())
		if err != nil {
			return nil, err
		}
		switch status {
		case 200:
			var stats []ghContributorStats
			if err := json.Unmarshal(body, &stats); err != nil {
				return nil, fmt.Errorf("parse contributor stats: %w", err)
			}
			return stats, nil
		case 202:
			time.Sleep(2 * time.Second)
			continue
		case 403, 429:
			return nil, githubRateLimitErr(status)
		default:
			return nil, fmt.Errorf("github contributor stats: %s", http.StatusText(status))
		}
	}
	return nil, fmt.Errorf("contributor stats not ready yet, try again shortly")
}

// computeBusFactor sums each contributor's commits over the trailing 12
// months and returns the top contributor's share of that total.
func computeBusFactor(stats []ghContributorStats, now time.Time) (topLogin string, share float64) {
	cutoff := now.AddDate(-1, 0, 0).Unix()
	totals := map[string]int{}
	all := 0
	for _, s := range stats {
		if s.Author.Login == "" {
			continue
		}
		sum := 0
		for _, w := range s.Weeks {
			if w.W >= cutoff {
				sum += w.C
			}
		}
		totals[s.Author.Login] += sum
		all += sum
	}
	for login, c := range totals {
		if c > totals[topLogin] {
			topLogin = login
		}
	}
	if all > 0 {
		share = float64(totals[topLogin]) / float64(all)
	}
	return
}

// ---------- OSV.dev ----------

type osvBatchResponse struct {
	Results []struct {
		Vulns []struct {
			ID string `json:"id"`
		} `json:"vulns"`
	} `json:"results"`
}

type osvVulnDetail struct {
	Summary          string `json:"summary"`
	Published        string `json:"published"`
	DatabaseSpecific struct {
		Severity string `json:"severity"` // "LOW"/"MODERATE"/"HIGH"/"CRITICAL", empty if OSV hasn't classified it
	} `json:"database_specific"`
	Affected []struct {
		Ranges []struct {
			Events []struct {
				Introduced string `json:"introduced,omitempty"`
				Fixed      string `json:"fixed,omitempty"`
			} `json:"events"`
		} `json:"ranges"`
	} `json:"affected"`
}

// fixedVersions pulls every "fixed" version OSV lists across this vuln's
// affected ranges (there can be more than one, e.g. a backport on an older
// major line as well as the current one).
func fixedVersions(d *osvVulnDetail) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range d.Affected {
		for _, r := range a.Ranges {
			for _, e := range r.Events {
				if e.Fixed != "" && !seen[e.Fixed] {
					seen[e.Fixed] = true
					out = append(out, e.Fixed)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func fetchOsvVulnDetail(id string) (*osvVulnDetail, error) {
	body, status, err := httpGet("https://api.osv.dev/v1/vulns/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("osv vuln %s: %s", id, http.StatusText(status))
	}
	var d osvVulnDetail
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// osvBatchQueryChunk asks OSV's batch endpoint which packages have known
// vulnerabilities. Critically, it sends the exact installed version for each
// package — without it, OSV returns every vulnerability ever filed against
// that package name, including ones for versions you don't have and ones
// already patched in the version you do have. With a version, OSV itself
// filters to only the advisories whose affected ranges actually cover it.
// The batch endpoint intentionally returns only IDs (no summary/published)
// to keep responses small.
func osvBatchQueryChunk(targets []*Node) (map[string][]string, error) {
	type query struct {
		Package struct {
			Name      string `json:"name"`
			Ecosystem string `json:"ecosystem"`
		} `json:"package"`
		Version string `json:"version,omitempty"`
	}
	var reqBody struct {
		Queries []query `json:"queries"`
	}
	for _, n := range targets {
		var q query
		q.Package.Name = n.Name
		q.Package.Ecosystem = "npm"
		q.Version = n.Version
		reqBody.Queries = append(reqBody.Queries, q)
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", "https://api.osv.dev/v1/querybatch", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("osv.dev: %s", http.StatusText(resp.StatusCode))
	}
	var res osvBatchResponse
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("parse osv response: %w", err)
	}
	out := map[string][]string{}
	for i, r := range res.Results {
		for _, v := range r.Vulns {
			out[targets[i].Name] = append(out[targets[i].Name], v.ID)
		}
	}
	return out, nil
}

// osvBatchQuery resolves advisory IDs — filtered to the installed version of
// each package — in one (or a few, chunked) batch calls, then fetches full
// details (including fixed-in versions) only for the distinct vuln IDs that
// actually came back; most packages have none.
func osvBatchQuery(targets []*Node) (map[string][]Advisory, error) {
	idsByPkg := map[string][]string{}
	const chunkSize = 1000
	for start := 0; start < len(targets); start += chunkSize {
		end := start + chunkSize
		if end > len(targets) {
			end = len(targets)
		}
		ids, err := osvBatchQueryChunk(targets[start:end])
		if err != nil {
			return nil, err
		}
		for pkg, pkgIDs := range ids {
			idsByPkg[pkg] = pkgIDs
		}
	}

	details := map[string]*osvVulnDetail{}
	for _, ids := range idsByPkg {
		for _, id := range ids {
			if _, done := details[id]; done {
				continue
			}
			d, err := fetchOsvVulnDetail(id)
			if err != nil {
				d = &osvVulnDetail{} // keep the ID even if detail fetch fails
			}
			details[id] = d
		}
	}

	out := map[string][]Advisory{}
	for pkg, ids := range idsByPkg {
		for _, id := range ids {
			d := details[id]
			adv := Advisory{ID: id, Summary: d.Summary, Severity: d.DatabaseSpecific.Severity, FixedIn: fixedVersions(d)}
			if t, err := time.Parse(time.RFC3339, d.Published); err == nil {
				adv.Published = t
			}
			out[pkg] = append(out[pkg], adv)
		}
	}
	return out, nil
}

// ---------- orchestration ----------

type Advisory struct {
	ID        string
	Summary   string
	Severity  string // "LOW"/"MODERATE"/"HIGH"/"CRITICAL" per OSV, empty if unclassified
	Published time.Time
	FixedIn   []string // versions OSV lists as fixed; empty if no fix is published yet
}

type Health struct {
	Package            string
	Version            string
	Dev                bool // from the lockfile's own "dev" flag — ground truth, not inferred
	MaintainerCount    int  // npm publish-access count, not code contributors — see npmPackument.Maintainers
	LastPublish        time.Time
	PublishCadenceDays float64 // mean days between the last ~10 releases
	WeeklyDownloads    int     // npm weekly downloads — proxy for replacement pressure if maintenance stalls

	GithubRepo       string // "owner/repo", empty if unresolved
	LastCommit       time.Time
	ContributorCount int // via anon=1 (see fetchHealth); can overcount contributors who used multiple unlinked emails
	Archived         bool
	OpenIssues       int // issues only; GitHub's own open_issues_count also counts open PRs
	TopContributor   string
	BusFactor        float64 // top contributor's share of commits, trailing 12mo, [0,1]

	Advisories []Advisory
	Notes      []string // partial-failure notes, e.g. "no GitHub repo found"
}

// healthTargets picks direct dependencies (one Node per distinct package
// name) plus, if topN > 0, the next topN packages by blast radius that
// aren't already covered.
func healthTargets(g *Graph, topN int) []*Node {
	seen := map[string]bool{}
	var targets []*Node
	var rest []*Node
	for id, n := range g.Nodes {
		if id == "" || n.Depth < 0 {
			continue
		}
		if n.Direct {
			if !seen[n.Name] {
				seen[n.Name] = true
				targets = append(targets, n)
			}
		} else {
			rest = append(rest, n)
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })

	if topN > 0 {
		sort.Slice(rest, func(i, j int) bool {
			if rest[i].TransitiveDependents != rest[j].TransitiveDependents {
				return rest[i].TransitiveDependents > rest[j].TransitiveDependents
			}
			return rest[i].ID < rest[j].ID
		})
		for _, n := range rest {
			if topN == 0 {
				break
			}
			if seen[n.Name] {
				continue
			}
			seen[n.Name] = true
			targets = append(targets, n)
			topN--
		}
	}
	return targets
}

func fetchHealth(n *Node, advisories []Advisory) *Health {
	h := &Health{Package: n.Name, Version: n.Version, Dev: n.Dev, Advisories: advisories}

	pj, err := fetchNpmPackument(n.Name)
	if err != nil {
		h.Notes = append(h.Notes, err.Error()) // fetchNpmPackument's error already says "npm registry: ..."
		return h
	}
	h.LastPublish, h.PublishCadenceDays, h.MaintainerCount = npmPublishStats(pj)

	if downloads, err := fetchNpmDownloads(n.Name); err != nil {
		h.Notes = append(h.Notes, "npm downloads: "+err.Error())
	} else {
		h.WeeklyDownloads = downloads
	}

	owner, repo, ok := resolveGithubRepo(pj)
	if !ok {
		h.Notes = append(h.Notes, "no GitHub repository found")
		return h
	}
	h.GithubRepo = owner + "/" + repo

	if r, err := fetchGithubRepo(owner, repo); err != nil {
		h.Notes = append(h.Notes, "github repo: "+err.Error())
	} else {
		h.Archived = r.Archived
		if t, err := time.Parse(time.RFC3339, r.PushedAt); err == nil {
			h.LastCommit = t
		}

		// open_issues_count counts open PRs as issues (a long-standing
		// GitHub API quirk) — subtract the real open-PR total to get an
		// issues-only number that matches what the Issues tab shows.
		openPRs, err := paginatedCount(fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls?state=open&per_page=1", owner, repo))
		if err != nil {
			h.Notes = append(h.Notes, "open PR count: "+err.Error())
			h.OpenIssues = r.OpenIssues // fall back to the combined issues+PRs figure
		} else {
			h.OpenIssues = r.OpenIssues - openPRs
			if h.OpenIssues < 0 {
				h.OpenIssues = 0 // counts can drift between the two live calls
			}
		}
	}

	// anon=1 includes commits from emails not linked to a GitHub account.
	// This can overcount (GitHub dedupes anonymous entries by commit email,
	// not by person, so one contributor using several unlinked emails over
	// the years shows up more than once) but it never undercounts the way
	// the non-anon list does for repos with lots of legacy/unlinked commits,
	// and it isn't subject to the 500-entry cap stats/contributors hits.
	if count, err := paginatedCount(fmt.Sprintf("https://api.github.com/repos/%s/%s/contributors?per_page=1&anon=1", owner, repo)); err != nil {
		h.Notes = append(h.Notes, "contributor count: "+err.Error())
	} else {
		h.ContributorCount = count
	}

	if stats, err := fetchGithubContributorStats(owner, repo); err != nil {
		h.Notes = append(h.Notes, "bus factor: "+err.Error())
	} else {
		h.TopContributor, h.BusFactor = computeBusFactor(stats, time.Now())
	}
	return h
}

// runHealth fetches health signals for direct deps (plus optional top-N by
// blast radius) concurrently, bounded so we don't hammer GitHub's rate limit.
func runHealth(g *Graph, topN int) []*Health {
	targets := healthTargets(g, topN)

	advisories, err := osvBatchQuery(targets)
	if err != nil {
		fmt.Fprintln(os.Stderr, "osv.dev error:", err)
		advisories = map[string][]Advisory{}
	}

	rows := make([]*Health, len(targets))
	const maxConcurrency = 5
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	for i, n := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, n *Node) {
			defer wg.Done()
			defer func() { <-sem }()
			rows[i] = fetchHealth(n, advisories[n.Name])
		}(i, n)
	}
	wg.Wait()
	return rows
}

// ---------- output ----------

func dateOrDash(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02")
}

func busFactorStr(h *Health) string {
	if h.TopContributor == "" {
		return "-"
	}
	return strconv.Itoa(int(h.BusFactor*100)) + "%"
}

// formatCount abbreviates large counts (downloads, etc.) for table display.
// The underlying Health field always keeps the exact int — this is a
// display-only helper.
func formatCount(n int) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1_000_000, 'f', 1, 64) + "M"
	case n >= 1_000:
		return strconv.FormatFloat(float64(n)/1_000, 'f', 1, 64) + "K"
	default:
		return strconv.Itoa(n)
	}
}

// advisoryStr reports each advisory alongside OSV's "fixed in" version(s) —
// since the query was scoped to the installed version (osvBatchQueryChunk),
// every advisory listed here already applies to what's actually installed.
func advisoryStr(h *Health) string {
	if len(h.Advisories) == 0 {
		return "-"
	}
	parts := make([]string, len(h.Advisories))
	for i, a := range h.Advisories {
		sev := a.Severity
		if sev == "" {
			sev = "UNRATED"
		}
		if len(a.FixedIn) == 0 {
			parts[i] = a.ID + " [" + sev + "] (no fix published yet)"
		} else {
			parts[i] = a.ID + " [" + sev + "] (fixed in " + strings.Join(a.FixedIn, "/") + ")"
		}
	}
	return strings.Join(parts, ", ")
}

// splitHealthProblems separates packages Atlas could actually evaluate from
// ones where something failed along the way (npm registry 404, GitHub rate
// limited, etc.) — the latter have no reliable signal behind them and
// shouldn't be mixed into health findings or judged as if they were.
func splitHealthProblems(rows []*Health) (clean, problems []*Health) {
	for _, h := range rows {
		if len(h.Notes) > 0 {
			problems = append(problems, h)
		} else {
			clean = append(clean, h)
		}
	}
	return clean, problems
}

// printHealth builds the report text, prints it, and returns it (so callers
// like the interactive agent can also fold it into a saved report).
func printHealth(rows []*Health) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n%-24s %-10s %-8s %-12s %-12s %-6s %-9s %-5s %-7s %s\n",
		"PACKAGE", "VERSION", "DL/WK", "LAST PUBLISH", "LAST COMMIT", "CONTR", "BUSFACTOR", "ARCH", "ISSUES", "ADVISORIES")
	for _, h := range rows {
		fmt.Fprintf(&b, "%-24s %-10s %-8s %-12s %-12s %-6d %-9s %-5v %-7d %s\n",
			trunc(h.Package, 24), trunc(h.Version, 10), formatCount(h.WeeklyDownloads),
			dateOrDash(h.LastPublish),
			dateOrDash(h.LastCommit), h.ContributorCount,
			busFactorStr(h), h.Archived, h.OpenIssues, advisoryStr(h))
	}
	for _, h := range rows {
		for _, note := range h.Notes {
			fmt.Fprintf(&b, "  note: %s: %s\n", h.Package, note)
		}
	}
	fmt.Fprintln(&b, "\nAdvisories above are scoped to your installed version via OSV.dev's version filter,"+
		" but OSV can lag a freshly-published GHSA by a few hours. For anything you're about to act on,"+
		" cross-check with `npm audit` or https://github.com/advisories before treating it as confirmed.")
	fmt.Fprintln(&b, "A high bus factor alone isn't a verdict: a package with few contributors and low downloads"+
		" has nobody to pick it up if the maintainer stops, but the same bus factor on a package with a broad"+
		" contributor base and heavy downloads usually gets a replacement maintainer or fork. Read bus factor,"+
		" contributor count, and downloads together.")
	fmt.Print(b.String())
	return b.String()
}

func writeHealthJSON(rows []*Health, path string) error {
	b, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
