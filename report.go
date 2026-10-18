package main

// Renders a human-readable HTML report bundling whatever analysis the
// interactive agent has gathered in a session (graph, usage scan, health
// check) plus the model's own final assessment, and opens generated files
// in the default browser.
//
// Uses html/template (not raw string formatting) because the embedded data
// includes text pulled from third-party sources — npm maintainers, GitHub
// repo fields, OSV advisory summaries, and the model's own free-form answer
// — which must be escaped before landing in a page a real browser renders.

import (
	"fmt"
	"html/template"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// how many graph rows the HTML report shows (more generous than the ~15-row
// summary fed to the chat model, since this is a scrollable table, not a
// terminal column budget).
const reportGraphRows = 50

type reportGraphRow struct {
	Name       string
	Version    string
	Direct     bool
	Depth      int
	Dependents int
	Transitive int
}

type reportUsageRow struct {
	Package         string
	ProdFiles       int
	ProdFileNames   string
	TestFiles       int
	TestFileNames   string
	ConfigFiles     int
	ConfigFileNames string
	Dirs            int
	Sites           int
	Tags            string
}

type reportHealthRow struct {
	Package        string
	Version        string
	Downloads      string
	LastPublish    string
	LastCommit     string
	Contributors   int
	BusFactor      string
	Archived       bool
	OpenIssues     int
	AdvisoryItems  []string
	HasAdvisory    bool
	UsedIn         string // cross-referenced from the usage scan; "" if usage wasn't scanned
	Classification string // one of ClassRuntime/ClassTestOnly/ClassConfigReferenced/ClassBuildTooling/ClassUnusedCandidate
}

// reportProblemRow is a package Atlas couldn't fully evaluate — a failed
// lookup, not a finding. Kept entirely separate from reportHealthRow so it
// can never get mixed into the health table or the model's evaluative input.
type reportProblemRow struct {
	Package string
	Notes   []string
}

type reportData struct {
	Title       string
	GeneratedAt string
	GraphFile   string

	Score      RepoScore
	ScoreGauge template.HTML // pre-rendered SVG — see renderGaugeSVG

	Vertices    int
	Edges       int
	DirectCount int
	GraphTotal  int // total ranked rows available, before truncating to reportGraphRows
	GraphRows   []reportGraphRow

	UsageRoot    string
	UsageScanned int
	UsageRows    []reportUsageRow
	UnusedDeps   []string

	HealthRows  []reportHealthRow
	ProblemRows []reportProblemRow

	Reasoning template.HTML // pre-rendered, pre-escaped — see renderReasoning
}

func toReportGraphRows(list []*Node, limit int) []reportGraphRow {
	if limit > len(list) {
		limit = len(list)
	}
	out := make([]reportGraphRow, 0, limit)
	for _, n := range list[:limit] {
		out = append(out, reportGraphRow{
			Name: n.Name, Version: n.Version, Direct: n.Direct,
			Depth: n.Depth, Dependents: len(n.In), Transitive: n.TransitiveDependents,
		})
	}
	return out
}

func toReportUsageRows(rows []*Usage) []reportUsageRow {
	out := make([]reportUsageRow, len(rows))
	for i, u := range rows {
		out[i] = reportUsageRow{
			Package: u.Package, ProdFiles: len(u.ProdFiles), TestFiles: len(u.TestFiles),
			ProdFileNames:   listFiles(sortedFileList(u.ProdFiles)),
			TestFileNames:   listFiles(sortedFileList(u.TestFiles)),
			ConfigFiles:     len(u.ConfigFiles),
			ConfigFileNames: listFiles(sortedFileList(u.ConfigFiles)),
			Dirs:            len(u.Dirs), Sites: len(u.Sites), Tags: sortedKeys(u.Tags),
		}
	}
	return out
}

// maxUsageFilesListed caps how many file names usageSummary spells out per
// category before collapsing the rest into "+N more" — enough to name every
// file for the common case (a package used in a handful of places) without
// an unreadable wall of paths for something imported everywhere.
const maxUsageFilesListed = 8

// listFiles renders up to maxUsageFilesListed file names, or the count only
// if there are more than that.
func listFiles(files []string) string {
	if len(files) > maxUsageFilesListed {
		return strconv.Itoa(len(files)) + " files"
	}
	return strings.Join(files, ", ")
}

// usageSummary renders how deeply a package is actually used in this
// project, naming the actual files — the context severity needs: an
// advisory in a package nobody imports in production code is a very
// different story than one wired into main.ts and calculate.ts.
func usageSummary(u *Usage) string {
	if u == nil {
		return "not imported in source"
	}
	var parts []string
	if len(u.ProdFiles) > 0 {
		parts = append(parts, "prod: "+listFiles(sortedFileList(u.ProdFiles)))
	}
	if len(u.TestFiles) > 0 {
		parts = append(parts, "test: "+listFiles(sortedFileList(u.TestFiles)))
	}
	if len(u.ConfigFiles) > 0 {
		// Weaker evidence than a parsed import — name found as a string in a
		// config file (package.json, .eslintrc, etc.), e.g. a build-tool
		// plugin loaded by convention rather than require()'d anywhere.
		parts = append(parts, "config-referenced: "+listFiles(sortedFileList(u.ConfigFiles)))
	}
	if len(parts) == 0 {
		return "not imported in source"
	}
	s := strings.Join(parts, "; ")
	if tags := sortedKeys(u.Tags); tags != "" {
		s += " (" + tags + ")"
	}
	return s
}

// classifyUsage buckets a direct dependency deterministically — in Go, not
// left to the model, which can't reliably combine the dev flag with usage
// evidence on its own. isDev comes straight from the lockfile's own "dev"
// flag, not a guess.
//
// The key distinction: a devDependency with zero usage evidence is very
// likely a build-tool plugin loaded by naming convention (Grunt, Gulp,
// Webpack, ESLint, and similar all do this) that static scanning simply
// can't trace — not a candidate for removal. A regular (non-dev) dependency
// with zero usage evidence has no such excuse; nothing "convention-loads" a
// production runtime dependency the way build tools load dev tooling, so
// that's real evidence of dead weight.
const (
	ClassRuntime          = "runtime"
	ClassTestOnly         = "test-only"
	ClassConfigReferenced = "config-referenced"
	ClassBuildTooling     = "build-tooling"
	ClassUnusedCandidate  = "unused-candidate"
	ClassNotScanned       = "not-scanned" // usage scan never ran at all — no claim can be made either way
)

// classifyUsage buckets a direct dependency. scanned must be false whenever
// the usage scan never ran at all (e.g. clone failed) — without it, every
// package would default to "unused-candidate" just because nobody looked,
// which is a very different claim from "we looked and found nothing."
func classifyUsage(scanned, isDev bool, u *Usage) string {
	if !scanned {
		return ClassNotScanned
	}
	switch {
	case u != nil && len(u.ProdFiles) > 0:
		return ClassRuntime
	case u != nil && len(u.TestFiles) > 0:
		return ClassTestOnly
	case u != nil && len(u.ConfigFiles) > 0:
		return ClassConfigReferenced
	case isDev:
		return ClassBuildTooling
	default:
		return ClassUnusedCandidate
	}
}

// toReportHealthRows cross-references each package's health signals against
// its usage-scan result (nil usageRows means the scan never ran, vs. an
// empty-but-non-nil slice meaning it ran and found nothing for this package).
func toReportHealthRows(rows []*Health, usageRows []*Usage) []reportHealthRow {
	byPkg := map[string]*Usage{}
	for _, u := range usageRows {
		byPkg[u.Package] = u
	}

	out := make([]reportHealthRow, len(rows))
	for i, h := range rows {
		u := byPkg[h.Package]
		usedIn := ""
		if usageRows != nil {
			usedIn = usageSummary(u)
		}
		out[i] = reportHealthRow{
			Package: h.Package, Version: h.Version, Downloads: formatCount(h.WeeklyDownloads),
			LastPublish: dateOrDash(h.LastPublish),
			LastCommit:  dateOrDash(h.LastCommit), Contributors: h.ContributorCount,
			BusFactor: busFactorStr(h), Archived: h.Archived, OpenIssues: h.OpenIssues,
			AdvisoryItems: advisoryItems(h), HasAdvisory: len(h.Advisories) > 0,
			UsedIn: usedIn, Classification: classifyUsage(usageRows != nil, h.Dev, u),
		}
	}
	return out
}

func toReportProblemRows(rows []*Health) []reportProblemRow {
	out := make([]reportProblemRow, len(rows))
	for i, h := range rows {
		out[i] = reportProblemRow{Package: h.Package, Notes: h.Notes}
	}
	return out
}

var (
	mdBold      = regexp.MustCompile(`\*\*(.+?)\*\*`)
	mdCode      = regexp.MustCompile("`([^`]+)`")
	mdOrdered   = regexp.MustCompile(`^(\d+)\.\s+(.*)$`)
	mdHeading   = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	mdFence     = regexp.MustCompile("^```")
	mdTableCell = regexp.MustCompile(`^:?-{2,}:?$`)
)

// mdInline applies inline markdown (bold, code spans) to an already-escaped
// line. Code is converted before bold so a bold marker inside a code span
// isn't mistaken for emphasis.
func mdInline(escaped string) string {
	escaped = mdCode.ReplaceAllString(escaped, "<code>$1</code>")
	return mdBold.ReplaceAllString(escaped, "<strong>$1</strong>")
}

// splitTableRow splits a markdown table row like "| a | b |" into ["a",
// "b"], trimming cells and dropping the empty strings produced by
// leading/trailing pipes. Returns nil if the line doesn't look like a row.
func splitTableRow(line string) []string {
	line = strings.TrimSpace(line)
	if !strings.Contains(line, "|") {
		return nil
	}
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	parts := strings.Split(line, "|")
	cells := make([]string, len(parts))
	for i, p := range parts {
		cells[i] = strings.TrimSpace(p)
	}
	return cells
}

func isTableSeparatorRow(cells []string) bool {
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		if !mdTableCell.MatchString(c) {
			return false
		}
	}
	return true
}

// parseMarkdownTable checks whether lines[i] begins a markdown table — a row
// immediately followed by a "| :--- | :--- |"-style separator row — and if
// so returns the header cells, every data row, and how many lines (header +
// separator + data rows) make up the table.
func parseMarkdownTable(lines []string, i int) (header []string, rows [][]string, consumed int, ok bool) {
	if i+1 >= len(lines) {
		return nil, nil, 0, false
	}
	header = splitTableRow(lines[i])
	sep := splitTableRow(lines[i+1])
	if header == nil || !isTableSeparatorRow(sep) || len(sep) != len(header) {
		return nil, nil, 0, false
	}
	consumed = 2
	for i+consumed < len(lines) {
		row := splitTableRow(lines[i+consumed])
		if row == nil {
			break
		}
		rows = append(rows, row)
		consumed++
	}
	return header, rows, consumed, true
}

// renderReasoning turns the model's markdown-ish free-form answer into safe,
// structured HTML: paragraphs, "- "/"* " bullets, "1. " numbered lists, "#"
// headings, **bold**, and `code` spans. Escaping happens FIRST, on the raw
// text, before any HTML tags are introduced — so nothing from the model's
// own text can inject markup; the only unescaped HTML in the output is what
// this function inserts itself.
func renderReasoning(text string) template.HTML {
	escaped := template.HTMLEscapeString(strings.TrimSpace(text))
	lines := strings.Split(escaped, "\n")

	var b strings.Builder
	listTag := "" // "", "ul", or "ol" — tracks which list (if any) is open
	closeList := func() {
		if listTag != "" {
			b.WriteString("</" + listTag + ">\n")
			listTag = ""
		}
	}
	openList := func(tag string) {
		if listTag != tag {
			closeList()
			b.WriteString("<" + tag + ">\n")
			listTag = tag
		}
	}
	inCode := false
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		t := strings.TrimSpace(line)
		if !inCode {
			if header, rows, consumed, ok := parseMarkdownTable(lines, i); ok {
				closeList()
				b.WriteString(`<div class="table-wrap"><table><thead><tr>`)
				for _, c := range header {
					b.WriteString("<th>" + mdInline(c) + "</th>")
				}
				b.WriteString("</tr></thead><tbody>\n")
				for _, row := range rows {
					b.WriteString("<tr>")
					for _, c := range row {
						b.WriteString("<td>" + mdInline(c) + "</td>")
					}
					b.WriteString("</tr>\n")
				}
				b.WriteString("</tbody></table></div>\n")
				i += consumed - 1
				continue
			}
		}
		if mdFence.MatchString(t) {
			if inCode {
				b.WriteString("</code></pre>\n")
			} else {
				closeList()
				b.WriteString("<pre><code>")
			}
			inCode = !inCode
			continue
		}
		if inCode {
			b.WriteString(line + "\n")
			continue
		}
		switch {
		case t == "":
			closeList()
		case strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* "):
			openList("ul")
			b.WriteString("<li>" + mdInline(t[2:]) + "</li>\n")
		case mdOrdered.MatchString(t):
			m := mdOrdered.FindStringSubmatch(t)
			openList("ol")
			b.WriteString("<li>" + mdInline(m[2]) + "</li>\n")
		case mdHeading.MatchString(t):
			closeList()
			m := mdHeading.FindStringSubmatch(t)
			level := min(len(m[1])+2, 6) // keep headings subordinate to the section's own <h2>
			fmt.Fprintf(&b, "<h%d>%s</h%d>\n", level, mdInline(m[2]), level)
		default:
			closeList()
			b.WriteString("<p>" + mdInline(t) + "</p>\n")
		}
	}
	if inCode {
		b.WriteString("</code></pre>\n") // unterminated fence — close it anyway
	}
	closeList()
	return template.HTML(b.String())
}

var reportTmpl = template.Must(template.New("report").Parse(reportTemplate))

const reportTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Atlas report — {{.Title}}</title>
<style>
:root{
  --bg:#ffffff;--fg:#1f2328;--muted:#6b7280;--panel:#f6f8fa;--line:#d0d7de;
  --link:#2563eb;--danger:#b42318;--danger-bg:#fee2e2;--row-alt:#f9fafb;
}
@media (prefers-color-scheme:dark){
  :root{
    --bg:#0d1117;--fg:#e6edf3;--muted:#8b949e;--panel:#161b22;--line:#30363d;
    --link:#60a5fa;--danger:#f87171;--danger-bg:#3f1d1d;--row-alt:#11161c;
  }
}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.6 system-ui,-apple-system,"Segoe UI",sans-serif}
.wrap{max-width:1100px;margin:0 auto;padding:32px 24px 80px}
h1{font-size:24px;margin:0 0 4px;letter-spacing:-0.01em}
.meta{color:var(--muted);font-size:13px;margin-bottom:8px}
.meta a{color:var(--link);text-decoration:none;font-weight:600}
.meta a:hover{text-decoration:underline}
.stats{display:flex;gap:16px;margin:20px 0 8px;flex-wrap:wrap}
.stat{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:12px 20px;min-width:110px}
.stat .n{font-size:22px;font-weight:700}
.stat .l{font-size:11px;color:var(--muted);text-transform:uppercase;letter-spacing:.05em}
.score-section{text-align:center}
.gauge-wrap{display:flex;justify-content:center}
.gauge{width:100%;max-width:320px;height:auto}
.gauge-endlabel{font:600 11px system-ui,sans-serif;fill:var(--muted);text-transform:uppercase;letter-spacing:.04em}
.gauge-value{font:700 40px system-ui,sans-serif;fill:var(--fg)}
.gauge-label{font:600 14px system-ui,sans-serif;fill:var(--muted)}
.gauge-needle{fill:var(--fg)}
.gauge-pivot{fill:var(--fg);stroke:var(--bg);stroke-width:3}
section{margin-top:40px}
h2{font-size:16px;margin:0 0 14px;padding-bottom:8px;border-bottom:2px solid var(--line)}
.table-wrap{overflow-x:auto;border:1px solid var(--line);border-radius:10px}
table{width:100%;border-collapse:collapse;font-size:13px;white-space:nowrap}
th{text-align:left;background:var(--panel);padding:10px 14px;font-size:11px;text-transform:uppercase;
   letter-spacing:.04em;color:var(--muted);border-bottom:1px solid var(--line)}
td{padding:9px 14px;border-bottom:1px solid var(--line);vertical-align:top}
tr:last-child td{border-bottom:none}
tbody tr:nth-child(even){background:var(--row-alt)}
tr.flagged td{background:var(--danger-bg)}
.pkg{font-weight:600}
.health-table{min-width:1200px}
.advisories-cell{white-space:normal;min-width:240px}
.advisory-item{margin:0 0 6px}
.advisory-item:last-child{margin-bottom:0}
.note{display:block;font-weight:400;font-size:11px;color:var(--muted);margin-top:2px;white-space:normal}
.muted{color:var(--muted)}
.yes-bad{color:var(--danger);font-weight:600}
.caption{margin:10px 2px 0;font-size:12px;color:var(--muted)}
.reasoning{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:22px 26px;font-size:14px}
.reasoning p{margin:0 0 12px;line-height:1.7}
.reasoning ul,.reasoning ol{margin:0 0 14px;padding-left:22px}
.reasoning li{margin:4px 0;line-height:1.6}
.reasoning li>ul,.reasoning li>ol{margin-top:6px}
.reasoning strong{color:var(--fg)}
.reasoning h3,.reasoning h4,.reasoning h5,.reasoning h6{margin:18px 0 8px;font-size:14px;color:var(--fg)}
.reasoning h3:first-child,.reasoning h4:first-child{margin-top:0}
.reasoning code{background:var(--bg);border:1px solid var(--line);border-radius:4px;padding:1px 6px;
  font:12px ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
.reasoning pre{background:var(--bg);border:1px solid var(--line);border-radius:6px;padding:12px 14px;
  overflow-x:auto;margin:0 0 14px}
.reasoning pre code{background:none;border:none;padding:0;white-space:pre}
.reasoning table{white-space:normal;margin-bottom:14px}
.empty{color:var(--muted);font-style:italic;font-size:13px;padding:14px}
</style>
</head>
<body>
<div class="wrap">

<h1>Atlas dependency report</h1>
<p class="meta">{{.Title}} &middot; generated {{.GeneratedAt}} &middot; <a href="{{.GraphFile}}">open the interactive graph &rarr;</a></p>

<div class="stats">
  <div class="stat"><div class="n">{{.Vertices}}</div><div class="l">packages</div></div>
  <div class="stat"><div class="n">{{.Edges}}</div><div class="l">edges</div></div>
  <div class="stat"><div class="n">{{.DirectCount}}</div><div class="l">direct deps</div></div>
</div>

{{if .ScoreGauge}}
<section class="score-section">
<h2>Dependency health score</h2>
<div class="gauge-wrap">{{.ScoreGauge}}</div>
<p class="caption">Computed from advisory severity, abandonment risk, archived repos, and unused-candidate bloat — weighted by how each package is actually used (a vulnerability wired into production code counts far more than the same one on a test-only or build-tooling package). Not an AI guess — reproducible from the tables below.</p>
</section>
{{end}}

<section>
<h2>Dependency graph — ranked by blast radius</h2>
{{if .GraphRows}}
<div class="table-wrap">
<table>
<thead><tr><th>Package</th><th>Version</th><th>Direct</th><th>Depth</th><th>Dependents</th><th>Transitive</th></tr></thead>
<tbody>
{{range .GraphRows}}
<tr><td class="pkg">{{.Name}}</td><td>{{.Version}}</td><td>{{if .Direct}}yes{{else}}<span class="muted">no</span>{{end}}</td>
<td>{{.Depth}}</td><td>{{.Dependents}}</td><td>{{.Transitive}}</td></tr>
{{end}}
</tbody>
</table>
</div>
{{if gt .GraphTotal (len .GraphRows)}}<p class="caption">Showing top {{len .GraphRows}} of {{.GraphTotal}} packages by blast radius.</p>{{end}}
{{else}}<div class="empty">No packages found.</div>{{end}}
</section>

{{if .UsageRows}}
<section>
<h2>Usage scan — {{.UsageScanned}} source files under {{.UsageRoot}}</h2>
<div class="table-wrap">
<table>
<thead><tr><th>Package</th><th>Prod files</th><th>Test files</th><th>Config refs</th><th>Dirs</th><th>Sites</th><th>Tags</th></tr></thead>
<tbody>
{{range .UsageRows}}
<tr><td class="pkg">{{.Package}}</td>
<td>{{.ProdFiles}}{{if .ProdFileNames}}<span class="note">{{.ProdFileNames}}</span>{{end}}</td>
<td>{{.TestFiles}}{{if .TestFileNames}}<span class="note">{{.TestFileNames}}</span>{{end}}</td>
<td>{{.ConfigFiles}}{{if .ConfigFileNames}}<span class="note">{{.ConfigFileNames}}</span>{{end}}</td>
<td>{{.Dirs}}</td><td>{{.Sites}}</td>
<td>{{if .Tags}}{{.Tags}}{{else}}<span class="muted">&mdash;</span>{{end}}</td></tr>
{{end}}
</tbody>
</table>
</div>
<p class="caption">Config refs: the package name was found as a plain string in a config file (package.json, .eslintrc, etc.), not a parsed import — weaker evidence than prod/test files, but still real for build-tool plugins loaded by convention (Grunt, ESLint, Babel, and similar).</p>
{{if .UnusedDeps}}<p class="caption">No usage evidence found at all (no import, no config reference) — genuinely unused, or loaded via a convention this scan can't detect: {{range $i, $d := .UnusedDeps}}{{if $i}}, {{end}}{{$d}}{{end}}</p>{{end}}
</section>
{{end}}

{{if .HealthRows}}
<section>
<h2>Dependency health</h2>
<div class="table-wrap">
<table class="health-table">
<thead><tr><th>Package</th><th>Version</th><th>Downloads/wk</th><th>Last publish</th><th>Last commit</th>
<th>Contributors</th><th>Bus factor</th><th>Archived</th><th>Open issues</th><th>Classification</th><th>Used in this project</th><th>Advisories</th></tr></thead>
<tbody>
{{range .HealthRows}}
<tr{{if .HasAdvisory}} class="flagged"{{end}}>
<td class="pkg">{{.Package}}</td>
<td>{{.Version}}</td><td>{{.Downloads}}</td><td>{{.LastPublish}}</td><td>{{.LastCommit}}</td>
<td>{{.Contributors}}</td><td>{{.BusFactor}}</td>
<td>{{if .Archived}}<span class="yes-bad">yes</span>{{else}}<span class="muted">no</span>{{end}}</td>
<td>{{.OpenIssues}}</td>
<td>{{if eq .Classification "unused-candidate"}}<span class="yes-bad">Unused &mdash; consider removing</span>
{{- else if eq .Classification "build-tooling"}}<span class="muted">Build tooling (dev dep)</span>
{{- else if eq .Classification "config-referenced"}}<span class="muted">Config-referenced</span>
{{- else if eq .Classification "test-only"}}<span class="muted">Test-only</span>
{{- else if eq .Classification "not-scanned"}}<span class="muted">Not scanned</span>
{{- else}}<span class="muted">Runtime</span>{{end}}</td>
<td>{{if .UsedIn}}{{.UsedIn}}{{else}}<span class="muted">not scanned</span>{{end}}</td>
<td class="advisories-cell">{{if .AdvisoryItems}}{{range .AdvisoryItems}}<div class="advisory-item">{{.}}</div>{{end}}{{else}}<span class="muted">&mdash;</span>{{end}}</td>
</tr>
{{end}}
</tbody>
</table>
</div>
<p class="caption">Classification is computed from the lockfile's own dev-dependency flag plus the usage scan, not guessed: "Build tooling" means a devDependency with no detected usage — very likely loaded by a build-tool convention (Grunt, Gulp, Webpack, ESLint, etc.) that can't be traced by static scanning, not a removal candidate. "Unused" means a regular (non-dev) dependency with zero usage evidence anywhere — that has no such excuse and is a real candidate for removal.
Advisories are scoped to each package's installed version via OSV.dev's version filter, but OSV can lag a freshly-published GHSA by a few hours — cross-check with <code>npm audit</code> or github.com/advisories before acting on anything high-severity.
Read bus factor alongside contributors and downloads: high concentration with few contributors and low downloads means nobody picks up the project if the maintainer stops; the same concentration on a widely-downloaded, broadly-contributed-to package usually finds a replacement.
Severity is relative to this project: an advisory in a package only used in test files (or not imported at all) is far less urgent here than the same advisory in a package wired into production code. "config-referenced" means the name was found in a config file, not a parsed import — weaker evidence than prod/test, but not nothing (build-tool plugins are often loaded this way).</p>
</section>
{{end}}

{{if .Reasoning}}
<section>
<h2>Assessment &amp; recommendations</h2>
<div class="reasoning">{{.Reasoning}}</div>
</section>
{{end}}

{{if .ProblemRows}}
<section>
<h2>Problems</h2>
<p class="caption">Atlas wasn't able to find the registry entry (or hit some other error) for these packages, so there's no reliable data to evaluate them on. They're excluded from the health table above and from the assessment — check them manually if they matter to you.</p>
<div class="table-wrap">
<table>
<thead><tr><th>Package</th><th>What went wrong</th></tr></thead>
<tbody>
{{range .ProblemRows}}
<tr><td class="pkg">{{.Package}}</td><td>{{range $i, $n := .Notes}}{{if $i}}<br>{{end}}{{$n}}{{end}}</td></tr>
{{end}}
</tbody>
</table>
</div>
</section>
{{end}}

</div>
</body>
</html>
`

func writeReport(path string, data reportData) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return reportTmpl.Execute(f, data)
}

// openInBrowser best-effort opens a local file with the OS default
// application. It never fails the caller — in a headless environment
// nothing visible happens, which is fine.
// openInBrowser best-effort opens a local file with the OS default
// application. It never fails the caller — in a headless environment
// nothing visible happens, which is fine.
//
// Every analysis writes to the same path (reports/graph.html,
// reports/report.html), so re-"opening" it after regenerating just refocuses
// whatever tab already has that exact file:// URL open rather than forcing a
// real reload — the browser has no reason to think anything changed. A
// cache-busting query string makes each open a distinct URL, so it's treated
// as a fresh navigation instead of a no-op focus of stale content.
func openInBrowser(path string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return
	}
	p := filepath.ToSlash(abs)
	if runtime.GOOS == "windows" && !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	url := fmt.Sprintf("file://%s?t=%d", p, time.Now().UnixNano())

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
