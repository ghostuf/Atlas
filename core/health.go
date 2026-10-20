package core

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

// SharedHTTPClient is reused across every network call this program makes
// (npm, GitHub, OSV, and the lockfile-fetch/issue-creation paths in other
// packages). A fresh *http.Client means a fresh, unpooled *http.Transport, so
// every single request was paying for a brand new TCP+TLS handshake even
// when hitting the same host (api.github.com, registry.npmjs.org, ...)
// repeatedly in the same run — that handshake overhead was the single
// largest contributor in the CPU/network profile. Reusing one client lets
// the default Transport keep connections alive and pool/reuse them per host.
var SharedHTTPClient = &http.Client{Timeout: 30 * time.Second}

// HttpGet executes a GET request and returns body, status code, and error.
func HttpGet(u string, headers map[string]string) ([]byte, int, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := SharedHTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return body, resp.StatusCode, err
}

// GithubHeaders returns default headers with GITHUB_TOKEN if available.
func GithubHeaders() map[string]string {
	h := map[string]string{"Accept": "application/vnd.github+json"}
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		h["Authorization"] = "Bearer " + tok
	}
	return h
}

// githubRateLimitErr is used where only the status code is available (no
// headers), so it cannot tell a rate limit from a permission failure and says so.
func githubRateLimitErr(status int) error {
	if os.Getenv("GITHUB_TOKEN") == "" {
		return fmt.Errorf("github API returned HTTP %d: rate limit or access denied (no GITHUB_TOKEN is set; setting one raises the limit)", status)
	}
	return fmt.Errorf("github API returned HTTP %d: rate limit or access denied (the response did not say which)", status)
}

// ---------- npm registry ----------

type npmPackument struct {
	Maintainers []struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"maintainers"`
	Time       map[string]string `json:"time"`
	Repository struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"repository"`
}

func fetchNpmPackument(name string) (*npmPackument, error) {
	u := "https://registry.npmjs.org/" + url.PathEscape(name)
	body, status, err := HttpGet(u, nil)
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

func fetchNpmDownloads(name string) (int, error) {
	u := "https://api.npmjs.org/downloads/point/last-week/" + url.PathEscape(name)
	body, status, err := HttpGet(u, nil)
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

// ---------- GitHub REST API ----------

type githubRepoResponse struct {
	Archived   bool   `json:"archived"`
	PushedAt   string `json:"pushed_at"`
	OpenIssues int    `json:"open_issues_count"`
}

func fetchGithubRepo(owner, repo string) (*githubRepoResponse, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s", owner, repo)
	body, status, err := HttpGet(u, GithubHeaders())
	if err != nil {
		return nil, err
	}
	if status == 403 || status == 429 {
		return nil, githubRateLimitErr(status)
	}
	if status != 200 {
		return nil, fmt.Errorf("github repo %s/%s: %s", owner, repo, http.StatusText(status))
	}
	var r githubRepoResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parse github repo: %w", err)
	}
	return &r, nil
}

func paginatedCount(u string) (int, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return 0, err
	}
	for k, v := range GithubHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := SharedHTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 403 || resp.StatusCode == 429 {
		return 0, githubRateLimitErr(resp.StatusCode)
	}
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("github count: %s", http.StatusText(resp.StatusCode))
	}
	link := resp.Header.Get("Link")
	if link == "" {
		body, _ := io.ReadAll(resp.Body)
		var arr []any
		if err := json.Unmarshal(body, &arr); err != nil {
			return 0, err
		}
		return len(arr), nil
	}
	return parseLastPage(link)
}

func parseLastPage(linkHeader string) (int, error) {
	for _, part := range strings.Split(linkHeader, ",") {
		section := strings.TrimSpace(part)
		if strings.Contains(section, `rel="last"`) {
			start := strings.Index(section, "<")
			end := strings.Index(section, ">")
			if start >= 0 && end > start {
				targetURL := section[start+1 : end]
				parsed, err := url.Parse(targetURL)
				if err != nil {
					return 0, err
				}
				p := parsed.Query().Get("page")
				if p == "" {
					return 0, fmt.Errorf("no page param in last link")
				}
				n, err := strconv.Atoi(p)
				if err != nil {
					return 0, err
				}
				return n, nil
			}
		}
	}
	return 0, fmt.Errorf("no rel=last found in link header")
}

type contributorWeek struct {
	Week    int `json:"w"`
	Commits int `json:"c"`
}

type contributorStat struct {
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	Total int               `json:"total"`
	Weeks []contributorWeek `json:"weeks"`
}

// fetchGithubContributorStats polls GitHub's stats/contributors endpoint,
// which returns 202 while it computes the stats in the background for a repo
// that hasn't been queried recently (can take a while for a large/active
// repo). Backs off exponentially (250ms, 500ms, 1s, 2s, 4s — ~7.75s worst
// case across 6 attempts) rather than a fixed 1s sleep: that gives slow repos
// a realistic chance to finish computing before we give up, while still
// failing fast on repos that return instantly.
func fetchGithubContributorStats(owner, repo string) ([]contributorStat, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/stats/contributors", owner, repo)
	const maxAttempts = 6
	backoff := 250 * time.Millisecond
	for attempt := 0; attempt < maxAttempts; attempt++ {
		body, status, err := HttpGet(u, GithubHeaders())
		if err != nil {
			return nil, err
		}
		if status == 202 {
			if attempt == maxAttempts-1 {
				break
			}
			time.Sleep(backoff)
			backoff *= 2
			continue
		}
		if status == 403 || status == 429 {
			return nil, githubRateLimitErr(status)
		}
		if status != 200 {
			return nil, fmt.Errorf("github stats/contributors: %s", http.StatusText(status))
		}
		var stats []contributorStat
		if err := json.Unmarshal(body, &stats); err != nil {
			return nil, fmt.Errorf("parse contributor stats: %w", err)
		}
		return stats, nil
	}
	return nil, fmt.Errorf("github stats/contributors still computing (202) after retries")
}

func computeBusFactor(stats []contributorStat, now time.Time) (topContributor string, share float64) {
	oneYearAgo := now.AddDate(-1, 0, 0).Unix()
	var totals []struct {
		login string
		c     int
	}
	grandTotal := 0
	for _, s := range stats {
		c := 0
		for _, w := range s.Weeks {
			if int64(w.Week) >= oneYearAgo {
				c += w.Commits
			}
		}
		if c > 0 {
			login := s.Author.Login
			if login == "" {
				login = "(anon)"
			}
			totals = append(totals, struct {
				login string
				c     int
			}{login, c})
			grandTotal += c
		}
	}
	if grandTotal == 0 {
		return "", 0
	}
	sort.Slice(totals, func(i, j int) bool { return totals[i].c > totals[j].c })
	return totals[0].login, float64(totals[0].c) / float64(grandTotal)
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
		Severity string `json:"severity"`
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
	body, status, err := HttpGet("https://api.osv.dev/v1/vulns/"+url.PathEscape(id), nil)
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
	resp, err := SharedHTTPClient.Do(req)
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

	// Dedup first (the same advisory ID commonly recurs across packages), then
	// fetch each unique advisory's detail concurrently instead of one at a
	// time — this was a serial HTTP chain, the single biggest contributor to
	// the OSV-side network wait in the profile.
	var uniqueIDs []string
	seenID := map[string]bool{}
	for _, ids := range idsByPkg {
		for _, id := range ids {
			if !seenID[id] {
				seenID[id] = true
				uniqueIDs = append(uniqueIDs, id)
			}
		}
	}

	details := make(map[string]*osvVulnDetail, len(uniqueIDs))
	var detailsMu sync.Mutex
	var wg sync.WaitGroup
	const osvDetailConcurrency = 10
	sem := make(chan struct{}, osvDetailConcurrency)
	for _, id := range uniqueIDs {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			d, err := fetchOsvVulnDetail(id)
			if err != nil {
				d = &osvVulnDetail{}
			}
			detailsMu.Lock()
			details[id] = d
			detailsMu.Unlock()
		}(id)
	}
	wg.Wait()

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

// Advisory represents an OSV security advisory against a package version.
type Advisory struct {
	ID        string
	Summary   string
	Severity  string // "LOW"/"MODERATE"/"HIGH"/"CRITICAL"
	Published time.Time
	FixedIn   []string
}

// Health represents the health signals retrieved for a dependency.
type Health struct {
	Package            string
	Version            string
	Dev                bool
	MaintainerCount    int
	LastPublish        time.Time
	PublishCadenceDays float64
	WeeklyDownloads    int

	GithubRepo       string
	LastCommit       time.Time
	ContributorCount int
	Archived         bool
	OpenIssues       int
	TopContributor   string
	BusFactor        float64

	Advisories []Advisory
	Notes      []string
	Material   bool
}

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
		h.Notes = append(h.Notes, err.Error())
		h.Material = true
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
		h.Material = true
	} else {
		h.Archived = r.Archived
		if t, err := time.Parse(time.RFC3339, r.PushedAt); err == nil {
			h.LastCommit = t
		}

		openPRs, err := paginatedCount(fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls?state=open&per_page=1", owner, repo))
		if err != nil {
			h.Notes = append(h.Notes, "open PR count: "+err.Error())
			h.OpenIssues = r.OpenIssues
		} else {
			h.OpenIssues = r.OpenIssues - openPRs
			if h.OpenIssues < 0 {
				h.OpenIssues = 0
			}
		}
	}

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

// RunHealth fetches health signals for dependencies concurrently.
func RunHealth(g *Graph, topN int) []*Health {
	targets := healthTargets(g, topN)

	advisories, err := osvBatchQuery(targets)
	if err != nil {
		fmt.Fprintln(os.Stderr, "osv.dev error:", err)
		advisories = map[string][]Advisory{}
	}

	rows := make([]*Health, len(targets))
	// Each target makes up to ~5 sequential HTTP calls to npm/GitHub/OSV, so
	// this bounds how many packages are "in flight" at once, not total
	// requests. 5 was conservative leftover from before requests shared a
	// pooled client/connection; 12 keeps us comfortably under GitHub's
	// documented secondary-rate-limit guidance (avoid large concurrent
	// bursts) while meaningfully cutting wall-clock time now that connection
	// reuse and OSV detail fetching no longer force extra serialization.
	const maxConcurrency = 12
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

// DateOrDash returns formatted date or dash if zero time.
func DateOrDash(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02")
}

// BusFactorStr returns formatted bus factor percentage.
func BusFactorStr(h *Health) string {
	if h.TopContributor == "" {
		return "-"
	}
	return strconv.Itoa(int(h.BusFactor*100)) + "%"
}

// FormatCount abbreviates numbers for display (e.g. 1.2M, 50K).
func FormatCount(n int) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1_000_000, 'f', 1, 64) + "M"
	case n >= 1_000:
		return strconv.FormatFloat(float64(n)/1_000, 'f', 1, 64) + "K"
	default:
		return strconv.Itoa(n)
	}
}

// AdvisoryItems formats advisories into display strings.
func AdvisoryItems(h *Health) []string {
	if len(h.Advisories) == 0 {
		return nil
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
	return parts
}

// AdvisoryStr joins advisory items into a single string.
func AdvisoryStr(h *Health) string {
	items := AdvisoryItems(h)
	if len(items) == 0 {
		return "-"
	}
	return strings.Join(items, ", ")
}

// SplitHealthProblems separates clean evaluated records from material failures.
func SplitHealthProblems(rows []*Health) (clean, problems []*Health) {
	for _, h := range rows {
		if h.Material {
			problems = append(problems, h)
		} else {
			clean = append(clean, h)
		}
	}
	return clean, problems
}

// PrintHealth renders the table of health signals to stdout and returns the string.
func PrintHealth(rows []*Health) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n%-24s %-10s %-8s %-12s %-12s %-6s %-9s %-5s %-7s %s\n",
		"PACKAGE", "VERSION", "DL/WK", "LAST PUBLISH", "LAST COMMIT", "CONTR", "BUSFACTOR", "ARCH", "ISSUES", "ADVISORIES")
	for _, h := range rows {
		fmt.Fprintf(&b, "%-24s %-10s %-8s %-12s %-12s %-6d %-9s %-5v %-7d %s\n",
			Trunc(h.Package, 24), Trunc(h.Version, 10), FormatCount(h.WeeklyDownloads),
			DateOrDash(h.LastPublish),
			DateOrDash(h.LastCommit), h.ContributorCount,
			BusFactorStr(h), h.Archived, h.OpenIssues, AdvisoryStr(h))
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

// WriteHealthJSON saves health records as JSON.
func WriteHealthJSON(rows []*Health, path string) error {
	b, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
