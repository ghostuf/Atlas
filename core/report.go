package core

// Renders a human-readable HTML report bundling whatever analysis the
// interactive agent has gathered in a session plus the model's own final assessment.

import (
	"fmt"
	"html/template"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const ReportGraphRows = 50

type ReportGraphRow struct {
	Name       string
	Version    string
	Direct     bool
	Depth      int
	Dependents int
	Transitive int
}

type ReportUsageRow struct {
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

type ReportHealthRow struct {
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
	UsedIn         string
	Classification string
}

type ReportProblemRow struct {
	Package string
	Notes   []string
}

type ReportData struct {
	Title       string
	GeneratedAt string
	GraphFile   string

	Score      RepoScore
	ScoreGauge template.HTML

	Vertices    int
	Edges       int
	DirectCount int
	GraphTotal  int
	GraphRows   []ReportGraphRow

	UsageRoot    string
	UsageScanned int
	UsageRows    []ReportUsageRow
	UnusedDeps   []string

	HealthRows  []ReportHealthRow
	ProblemRows []ReportProblemRow

	Reasoning template.HTML
}

func ToReportGraphRows(list []*Node, limit int) []ReportGraphRow {
	if limit > len(list) {
		limit = len(list)
	}
	out := make([]ReportGraphRow, 0, limit)
	for _, n := range list[:limit] {
		out = append(out, ReportGraphRow{
			Name: n.Name, Version: n.Version, Direct: n.Direct,
			Depth: n.Depth, Dependents: len(n.In), Transitive: n.TransitiveDependents,
		})
	}
	return out
}

func ToReportUsageRows(rows []*Usage) []ReportUsageRow {
	out := make([]ReportUsageRow, len(rows))
	for i, u := range rows {
		out[i] = ReportUsageRow{
			Package: u.Package, ProdFiles: len(u.ProdFiles), TestFiles: len(u.TestFiles),
			ProdFileNames:   ListFiles(SortedFileList(u.ProdFiles)),
			TestFileNames:   ListFiles(SortedFileList(u.TestFiles)),
			ConfigFiles:     len(u.ConfigFiles),
			ConfigFileNames: ListFiles(SortedFileList(u.ConfigFiles)),
			Dirs:            len(u.Dirs), Sites: len(u.Sites), Tags: SortedKeys(u.Tags),
		}
	}
	return out
}

func ToReportHealthRows(rows []*Health, usageRows []*Usage) []ReportHealthRow {
	byPkg := map[string]*Usage{}
	for _, u := range usageRows {
		byPkg[u.Package] = u
	}

	out := make([]ReportHealthRow, len(rows))
	for i, h := range rows {
		u := byPkg[h.Package]
		usedIn := ""
		if usageRows != nil {
			usedIn = UsageSummary(u)
		}
		out[i] = ReportHealthRow{
			Package: h.Package, Version: h.Version, Downloads: FormatCount(h.WeeklyDownloads),
			LastPublish: DateOrDash(h.LastPublish),
			LastCommit:  DateOrDash(h.LastCommit), Contributors: h.ContributorCount,
			BusFactor: BusFactorStr(h), Archived: h.Archived, OpenIssues: h.OpenIssues,
			AdvisoryItems: AdvisoryItems(h), HasAdvisory: len(h.Advisories) > 0,
			UsedIn: usedIn, Classification: ClassifyUsage(usageRows != nil, h.Dev, u),
		}
	}
	return out
}

func ToReportProblemRows(rows []*Health) []ReportProblemRow {
	out := make([]ReportProblemRow, len(rows))
	for i, h := range rows {
		out[i] = ReportProblemRow{Package: h.Package, Notes: h.Notes}
	}
	return out
}

var (
	MdBold      = regexp.MustCompile(`\*\*(.+?)\*\*`)
	MdCode      = regexp.MustCompile("`([^`]+)`")
	MdOrdered   = regexp.MustCompile(`^(\d+)\.\s+(.*)$`)
	MdHeading   = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	MdFence     = regexp.MustCompile("^```")
	MdTableCell = regexp.MustCompile(`^:?-{2,}:?$`)
)

func MdInline(escaped string) string {
	escaped = MdCode.ReplaceAllString(escaped, "<code>$1</code>")
	return MdBold.ReplaceAllString(escaped, "<strong>$1</strong>")
}

func SplitTableRow(line string) []string {
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

func IsTableSeparatorRow(cells []string) bool {
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		if !MdTableCell.MatchString(c) {
			return false
		}
	}
	return true
}

func ParseMarkdownTable(lines []string, i int) (header []string, rows [][]string, consumed int, ok bool) {
	if i+1 >= len(lines) {
		return nil, nil, 0, false
	}
	header = SplitTableRow(lines[i])
	sep := SplitTableRow(lines[i+1])
	if header == nil || !IsTableSeparatorRow(sep) || len(sep) != len(header) {
		return nil, nil, 0, false
	}
	consumed = 2
	for i+consumed < len(lines) {
		row := SplitTableRow(lines[i+consumed])
		if row == nil {
			break
		}
		rows = append(rows, row)
		consumed++
	}
	return header, rows, consumed, true
}

// RenderReasoning converts markdown-like text into safe HTML.
func RenderReasoning(text string) template.HTML {
	escaped := template.HTMLEscapeString(strings.TrimSpace(text))
	lines := strings.Split(escaped, "\n")

	var b strings.Builder
	listTag := ""
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
			if header, rows, consumed, ok := ParseMarkdownTable(lines, i); ok {
				closeList()
				b.WriteString(`<div class="table-wrap"><table><thead><tr>`)
				for _, c := range header {
					b.WriteString("<th>" + MdInline(c) + "</th>")
				}
				b.WriteString("</tr></thead><tbody>\n")
				for _, row := range rows {
					b.WriteString("<tr>")
					for _, c := range row {
						b.WriteString("<td>" + MdInline(c) + "</td>")
					}
					b.WriteString("</tr>\n")
				}
				b.WriteString("</tbody></table></div>\n")
				i += consumed - 1
				continue
			}
		}
		if MdFence.MatchString(t) {
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
			b.WriteString("<li>" + MdInline(t[2:]) + "</li>\n")
		case MdOrdered.MatchString(t):
			m := MdOrdered.FindStringSubmatch(t)
			openList("ol")
			b.WriteString("<li>" + MdInline(m[2]) + "</li>\n")
		case MdHeading.MatchString(t):
			m := MdHeading.FindStringSubmatch(t)
			level := len(m[1]) + 2
			if level > 6 {
				level = 6
			}
			closeList()
			fmt.Fprintf(&b, "<h%d>%s</h%d>\n", level, MdInline(m[2]), level)
		default:
			closeList()
			b.WriteString("<p>" + MdInline(t) + "</p>\n")
		}
	}
	if inCode {
		b.WriteString("</code></pre>\n")
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
<title>Atlas report • {{.Title}}</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Lexend+Deca:wght@300;400;500;600&family=Roboto+Mono:wght@400;500;700&display=swap" rel="stylesheet">
<style>
:root {
  --bg: #0c0c0c;
  --sub-alt: #121212;
  --sub: #404040;
  --text: #d3c2c3;
  --caret: #bebebe;
  --main: #b7976a;
  --main-hover: #c1a174;
  --error: #ff4d4f;
  --error-extra: #ff7875;
  --warning: #b7976a;

  --text-muted: rgba(211, 194, 195, 0.65);

  --radius: 8px;
  --radius-sm: 4px;
  --border: 1px solid var(--sub);
  --transition: 150ms ease;
}
body[data-theme="light"] {
  --bg: #a0a0a0;
  --sub-alt: #bebebe;
  --sub: #353535;
  --text: #3b2d3b;
  --caret: #8d8d8d;
  --main: #573e40;
  --main-hover: #6a4d50;
  --error: #ff4d4f;
  --error-extra: #ff7875;
  --text-muted: #bfbfbf;
}
* { box-sizing: border-box; }
body {
  margin: 0;
  background: var(--bg);
  color: var(--text);
  font-family: "Roboto Mono", ui-monospace, SFMono-Regular, Menlo, monospace;
  line-height: 1.6;
  font-size: 14px;
  scrollbar-color: var(--sub) var(--bg);
}
::selection { background: var(--main); color: var(--bg); }
:focus-visible { outline: 2px solid var(--main); outline-offset: 2px; }

.wrap { max-width: 1200px; margin: 0 auto; padding: 2.5rem 2rem 5rem; }
h1, h2, h3, h4, nav, button, label, .ui {
  font-family: "Lexend Deca", system-ui, sans-serif;
  letter-spacing: -0.01em;
  color: var(--text);
}
h1 { font-size: 2.25rem; font-weight: 500; line-height: 1.2; margin: 0 0 6px; }
.meta { color: var(--text-muted); font-size: 13px; margin-bottom: 12px; }
.meta a { color: var(--main); text-decoration: none; transition: color var(--transition); }
.meta a:hover { color: var(--main-hover); }

.stats { display: flex; gap: 16px; margin: 24px 0 16px; flex-wrap: wrap; }
.stat { background: var(--sub-alt); border: 1px solid var(--sub); border-radius: var(--radius); padding: 16px 24px; min-width: 130px; }
.stat .n { font-family: "Roboto Mono", monospace; font-size: 2.25rem; font-weight: 500; color: var(--main); line-height: 1; margin-bottom: 4px; }
.stat .l { font-family: "Lexend Deca", sans-serif; font-size: 11.5px; color: var(--text-muted); text-transform: lowercase; }

.score-section { text-align: center; margin-top: 36px; }
.gauge-wrap { display: flex; justify-content: center; }
.gauge { width: 100%; max-width: 320px; height: auto; }
.gauge-endlabel { font: 500 11px "Lexend Deca", sans-serif; fill: var(--sub); text-transform: lowercase; }
.gauge-value { font: 500 44px "Roboto Mono", monospace; fill: var(--main); }
.gauge-label { font: 500 14px "Lexend Deca", sans-serif; fill: var(--text); }
.gauge-needle { fill: var(--caret); }
.gauge-pivot { fill: var(--main); stroke: var(--bg); stroke-width: 3; }

section { margin-top: 44px; }
h2 { font-size: 1.5rem; font-weight: 500; margin: 0 0 16px; padding-bottom: 10px; border-bottom: 1px solid var(--sub); }

.constellation-link {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  background: var(--sub-alt);
  color: var(--text);
  border: 1px solid var(--sub);
  border-radius: var(--radius);
  padding: 0.5rem 1rem;
  font-family: "Lexend Deca", sans-serif;
  font-size: 13px;
  text-decoration: none;
  transition: background var(--transition), color var(--transition), border-color var(--transition);
}
.constellation-link:hover {
  background: var(--main);
  color: var(--bg);
  border-color: var(--main);
}
.constellation-viewport-wrap {
  margin: 18px 0 24px;
  border-radius: var(--radius);
  overflow: hidden;
  border: 1px solid var(--sub);
  background: var(--bg);
}
.constellation-viewport-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  padding: 12px 18px;
  background: var(--sub-alt);
  border-bottom: 1px solid var(--sub);
}
.constellation-viewport-title {
  font-family: "Lexend Deca", sans-serif;
  font-size: 13.5px;
  font-weight: 500;
  color: var(--text);
  display: flex;
  align-items: center;
  gap: 10px;
}
.constellation-viewport-badge {
  font-family: "Roboto Mono", monospace;
  font-size: 11px;
  background: rgba(183, 151, 106, 0.15);
  color: var(--main);
  border: 1px solid rgba(183, 151, 106, 0.35);
  padding: 2px 7px;
  border-radius: var(--radius-sm);
}
.constellation-viewport-frame {
  width: 100%;
  height: 560px;
  border: none;
  display: block;
  background: var(--bg);
}

.table-wrap { overflow-x: auto; border: 1px solid var(--sub); border-radius: var(--radius); background: var(--sub-alt); }
table { width: 100%; border-collapse: collapse; font-size: 13px; white-space: nowrap; font-family: "Roboto Mono", monospace; }
th {
  text-align: left;
  background: #161616;
  padding: 11px 16px;
  font-size: 12px;
  font-family: "Lexend Deca", sans-serif;
  text-transform: lowercase;
  color: var(--main);
  border-bottom: 1px solid var(--sub);
  font-weight: 500;
  letter-spacing: 0.02em;
}
td { padding: 9px 16px; border-bottom: 1px solid #1a1a1a; vertical-align: top; color: var(--text); }
tr:last-child td { border-bottom: none; }
tbody tr:nth-child(even) { background: #0f0f0f; }
tr.flagged td { background: rgba(255, 77, 79, 0.18); color: #ffece8; }
.pkg { font-weight: 500; color: var(--main); }
.health-table { min-width: 1200px; }
.advisories-cell { white-space: normal; min-width: 240px; }
.advisory-item { margin: 0 0 6px; }
.advisory-item:last-child { margin-bottom: 0; }
.note { display: block; font-weight: 400; font-size: 11px; color: var(--text-muted); margin-top: 2px; white-space: normal; }
.muted { color: var(--sub); }
.yes-bad { color: var(--error); font-weight: 500; }
.caption { margin: 10px 4px 0; font-size: 12px; color: var(--text-muted); font-family: "Roboto Mono", monospace; }

.reasoning {
  background: var(--sub-alt);
  border: 1px solid var(--sub);
  border-radius: var(--radius);
  padding: 24px 28px;
  font-size: 14px;
  color: var(--text);
  line-height: 1.7;
}
.reasoning p { margin: 0 0 14px; }
.reasoning ul, .reasoning ol { margin: 0 0 14px; padding-left: 24px; }
.reasoning li { margin: 5px 0; }
.reasoning strong { color: var(--main); font-weight: 500; }
.reasoning h3, .reasoning h4, .reasoning h5, .reasoning h6 {
  font-family: "Lexend Deca", sans-serif;
  color: var(--text);
  margin: 20px 0 8px;
  font-weight: 500;
}
.reasoning h3 { font-size: 21px; }
.reasoning h4 { font-size: 18px; }
.reasoning h5, .reasoning h6 { font-size: 16px; }
.reasoning h3:first-child, .reasoning h4:first-child { margin-top: 0; }
.status-dot {
  display: inline-block;
  width: 8px;
  height: 8px;
  border-radius: 50%;
  margin-right: 7px;
  vertical-align: middle;
}
.reasoning code {
  background: var(--bg);
  border: 1px solid var(--sub);
  border-radius: var(--radius-sm);
  padding: 2px 6px;
  font-family: "Roboto Mono", monospace;
  font-size: 12px;
  color: var(--main);
}
.reasoning pre {
  background: var(--bg);
  border: 1px solid var(--sub);
  border-radius: var(--radius);
  padding: 14px 18px;
  overflow-x: auto;
  margin: 0 0 16px;
}
.reasoning pre code { background: none; border: none; padding: 0; color: var(--text); }
.reasoning table { white-space: normal; margin-bottom: 16px; }
.empty { color: var(--text-muted); font-style: italic; font-size: 13px; padding: 14px; }
</style>
</head>
<body>
<div class="wrap">

<h1>Atlas dependency report</h1>
<p class="meta">{{.Title}} &middot; generated {{.GeneratedAt}} &middot; <a class="constellation-link" href="{{.GraphFile}}">✧ open interactive constellation map &rarr;</a></p>

<div class="stats">
  <div class="stat"><div class="n">{{.Vertices}}</div><div class="l">packages</div></div>
  <div class="stat"><div class="n">{{.Edges}}</div><div class="l">edges</div></div>
  <div class="stat"><div class="n">{{.DirectCount}}</div><div class="l">direct deps</div></div>
</div>

{{if .ScoreGauge}}
<section class="score-section">
<h2>Dependency health score</h2>
<div class="gauge-wrap">{{.ScoreGauge}}</div>
<p class="caption">Computed from advisory severity, abandonment risk, archived repos, and unused-candidate bloat — weighted by how each package is actually used. Not an AI guess — reproducible from the tables below.</p>
</section>
{{end}}

<section>
<h2>Dependency graph — ranked by blast radius</h2>
{{if .GraphFile}}
<div class="constellation-viewport-wrap">
  <div class="constellation-viewport-header">
    <div class="constellation-viewport-title">
      <span>✦ Constellation Dependency Observatory</span>
      <span class="constellation-viewport-badge">Live Interactive Map</span>
    </div>
    <a class="constellation-link" href="{{.GraphFile}}" target="_blank">Open Fullscreen Observatory &nearr;</a>
  </div>
  <iframe class="constellation-viewport-frame" src="{{.GraphFile}}" title="Constellation Map" loading="lazy"></iframe>
</div>
{{end}}
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
<p class="caption">Config refs: the package name was found as a plain string in a config file, not a parsed import — weaker evidence than prod/test files, but still real for build-tool plugins loaded by convention.</p>
{{if .UnusedDeps}}<p class="caption">No usage evidence found at all: {{range $i, $d := .UnusedDeps}}{{if $i}}, {{end}}{{$d}}{{end}}</p>{{end}}
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
<td>{{if eq .Classification "unused-candidate"}}<span class="status-dot" style="background:var(--error)"></span><span class="yes-bad">Unused &mdash; consider removing</span>
{{- else if eq .Classification "build-tooling"}}Build tooling (dev dep)
{{- else if eq .Classification "config-referenced"}}Config-referenced
{{- else if eq .Classification "test-only"}}Test-only
{{- else if eq .Classification "not-scanned"}}Not scanned
{{- else}}Runtime{{end}}</td>
<td>{{if .UsedIn}}{{.UsedIn}}{{else}}<span class="muted">not scanned</span>{{end}}</td>
<td class="advisories-cell">{{if .AdvisoryItems}}{{range .AdvisoryItems}}<div class="advisory-item">{{.}}</div>{{end}}{{else}}<span class="muted">&mdash;</span>{{end}}</td>
</tr>
{{end}}
</tbody>
</table>
</div>
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

// WriteReport outputs the complete HTML report to a file.
func WriteReport(path string, data ReportData) error {
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

// OpenInBrowser opens a file in the default browser.
func OpenInBrowser(path string) {
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

// StripInlineMarkdown removes bold and code syntax.
func StripInlineMarkdown(s string) string {
	s = MdCode.ReplaceAllString(s, "$1")
	return MdBold.ReplaceAllString(s, "$1")
}

// RenderTableForTerminal renders markdown table cells as ASCII table.
func RenderTableForTerminal(header []string, rows [][]string) string {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, c := range row {
			if i < len(widths) && len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	var b strings.Builder
	writeRow := func(cells []string) {
		for i, c := range cells {
			if i >= len(widths) {
				break
			}
			if i > 0 {
				b.WriteString("  ")
			}
			b.WriteString(c)
			if pad := widths[i] - len(c); pad > 0 {
				b.WriteString(strings.Repeat(" ", pad))
			}
		}
		b.WriteString("\n")
	}
	writeRow(header)
	for i, w := range widths {
		if i > 0 {
			b.WriteString("  ")
		}
		b.WriteString(strings.Repeat("-", w))
	}
	b.WriteString("\n")
	for _, row := range rows {
		writeRow(row)
	}
	return b.String()
}
