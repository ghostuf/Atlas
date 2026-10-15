package main

// Interactive tool-calling agent: a local Ollama model drives the existing
// Atlas pipeline (graph + viz + usage scan + health check) through tools,
// instead of you driving it through flags.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// reportDir is where the agent writes graph.html and report.html, always,
// so a session's output lives in one predictable place instead of the repo
// root getting cluttered with regenerated graphs on every run.
const reportDir = "reports"

const systemPrompt = `You are Atlas, a dependency risk analysis agent.

You have tools to analyze a project's npm dependency graph. For any request
about analyzing, auditing, or checking a repository's dependencies:

1. Always call analyze_repository first. It needs either a GitHub repo as
   "owner/name" (e.g. "cloudflare/mcp") or a local lockfile path. If the user
   hasn't given you one, ask them for it before calling any tool — don't guess.
   The interactive dependency graph opens in the browser immediately, and a
   usage scan (where each direct dependency is actually imported — prod vs.
   test files, directories, architectural tags) plus a health check (npm
   registry, GitHub, OSV.dev) both run automatically as part of this same
   call — you do NOT need to call scan_usage or check_health separately for
   a normal analysis; their results are already in the analyze_repository
   tool output.
2. Call scan_usage/check_health again yourself only to go further than the
   default: scan_usage with a specific "package" to see its exact usage
   sites, or check_health with "top" to extend coverage beyond direct
   dependencies.
3. Once you're done calling tools and are ready to give your final answer, a
   combined HTML report (graph + usage scan + health check + your own answer)
   is generated and opened automatically — you don't need a tool for that.
   Just write your final answer as a clear assessment: what's load-bearing,
   what's risky, and concretely what to do and what not to do about it. Base
   it only on the tool output you actually got back — cite real package
   names and numbers, don't invent them. This answer becomes the report's
   "Assessment & recommendations" section, so make it a real conclusion, not
   just a summary of numbers already in the tables.
4. When judging abandonment/bus-factor risk, don't read bus factor alone —
   read it together with contributor count and weekly downloads. A high bus
   factor (one person dominates recent commits) on a package with few
   contributors and low downloads is a real risk: if that person stops,
   nobody is positioned to pick it up. The same bus factor on a package with
   a broad contributor base and heavy weekly downloads is much less
   concerning — popular, actively-contributed-to projects reliably attract a
   replacement maintainer or fork if the original one disappears. Say this
   explicitly in your assessment rather than just quoting the percentage.
   Do NOT use "maintainers" as a sign of how alive or healthy a package is —
   it's npm's publish-access list (who is allowed to push a release), which
   goes stale for years and tells you nothing about current activity. For
   "is this actually being maintained," use lastCommit, lastPublish, and bus
   factor instead; only mention maintainer count in passing, if at all, not
   as a leading indicator.
5. Severity is project-specific, not just whatever OSV says in the abstract.
   Each advisory now carries an OSV severity tier (LOW/MODERATE/HIGH/
   CRITICAL) — but a CRITICAL advisory in a package this project only
   imports in one test file is low real-world urgency here; a MODERATE
   advisory in a package imported across many production files — especially
   ones tagged auth/db/routing — is a bigger deal for THIS project than the
   abstract severity alone suggests. Always cross-reference each flagged
   package's advisory severity against its usage ("usedInProject" on each
   health entry) before ranking urgency, and say explicitly why something is
   or isn't urgent for this specific codebase — don't just restate the OSV
   severity tier as if it were the final word. A package with zero prod-file
   usage (test-only, or never imported at all) should never be your top
   priority regardless of advisory severity.
6. "usedInProject" names the actual file(s), e.g. "prod: src/app.ts" or
   "test: test/lodash.test.js" — always cite those real file names directly
   in your answer (e.g. "lodash is only imported in test/lodash.test.js")
   instead of vaguely saying "a test file." If a package touches many files,
   it may be summarized as a count instead of a full list — don't invent
   specific file names in that case, just say how many. A "config-referenced:"
   entry means the package name was found as a plain string in a config file
   (package.json, .eslintrc, etc.) rather than a parsed import — this is real
   but weaker evidence, typically a build-tool plugin loaded by convention
   (Grunt, ESLint, Babel, and similar all do this). Treat it as meaningfully
   more likely to be in active use than "not imported in source," but don't
   claim it's definitely wired into application logic the way a "prod:" hit
   is — say it's probably in use via tooling, not proven.
7. check_health's tool output only ever contains packages it could fully
   evaluate. If it tells you some packages were excluded (registry lookup
   failed, or some other error), that's a tooling gap, not a finding — never
   discuss those packages in your assessment or recommendations. They
   already get their own "Problems" section in the report; don't duplicate
   or reference them in yours.
8. Every health entry carries a "classification" field, computed
   deterministically from the lockfile's own dev-dependency flag plus the
   usage scan — trust it as-is, don't re-derive your own judgment about
   whether something is "really" used:
   - "runtime" / "test-only" / "config-referenced": real or plausible usage
     evidence exists. Reason about it per the severity rules above.
   - "build-tooling": a devDependency with no usage evidence found anywhere.
     This is very likely a build-tool plugin loaded by naming convention
     (Grunt, Gulp, Webpack, ESLint, and similar all resolve plugins this way
     with no literal reference anywhere in the repo) — static scanning
     cannot trace it. Do NOT recommend removing it. At most, suggest a
     manual check (e.g. "verify it's referenced by task name in your build
     config before touching it").
   - "unused-candidate": NOT a devDependency, and no usage evidence anywhere.
     A regular (production) dependency has no legitimate reason to be
     invisible to scanning the way build tooling does — this is the one
     classification where you should affirmatively recommend removing the
     package from the codebase.
   - "not-scanned": the usage scan didn't run (e.g. source wasn't
     available) — make no claim about usage either way for these.`

type session struct {
	graph   *Graph
	srcDir  string
	repo    string
	cleanup func()

	usage  UsageScan // zero value (Rows == nil) until scan_usage has run
	health []*Health // nil until check_health has run
}

func (s *session) reset() {
	if s.cleanup != nil {
		s.cleanup()
	}
	s.graph, s.srcDir, s.repo, s.cleanup = nil, "", "", nil
	s.usage, s.health = UsageScan{}, nil
}

// writeSessionReport builds reports/report.html from everything gathered so
// far in the session plus the model's own final answer, and opens it. It
// doesn't print anything itself — the caller decides what the terminal
// should say (never the report content itself, just a pointer to it).
func (s *session) writeSessionReport(reasoning string) (string, error) {
	list, edges, direct := rankNodes(s.graph)
	data := reportData{
		Title:       s.repo,
		GeneratedAt: time.Now().Format("2006-01-02 15:04:05"),
		GraphFile:   "graph.html",
		Vertices:    len(s.graph.Nodes),
		Edges:       edges,
		DirectCount: direct,
		GraphTotal:  len(list),
		GraphRows:   toReportGraphRows(list, reportGraphRows),
		Reasoning:   renderReasoning(reasoning),
	}
	if s.usage.Rows != nil {
		data.UsageRoot = s.usage.Root
		data.UsageScanned = s.usage.Scanned
		data.UsageRows = toReportUsageRows(s.usage.Rows)
		data.UnusedDeps = s.usage.Unused
	}
	if s.health != nil {
		clean, problems := splitHealthProblems(s.health)
		data.HealthRows = toReportHealthRows(clean, s.usage.Rows)
		data.ProblemRows = toReportProblemRows(problems)
		data.Score = computeRepoScore(clean, s.usage.Rows, s.usage.Rows != nil)
		data.ScoreGauge = renderGaugeSVG(data.Score)
	}

	path := filepath.Join(reportDir, "report.html")
	if err := writeReport(path, data); err != nil {
		return "", err
	}
	openInBrowser(path)
	return path, nil
}

// analyzeRepository fetches the lockfile, builds the graph, always writes
// and opens an interactive visualization, and clones the repo for later
// usage scanning. The combined report is NOT written here — only once, at
// the end of the turn, after the model has given its final answer.
func (s *session) analyzeRepository(repo, file, ref, dir string) string {
	if repo == "" && file == "" {
		return "error: specify either a GitHub repo as owner/name, or a local lockfile path"
	}

	var data []byte
	fetchErr := withSpinner("Fetching lockfile", func() error {
		var err error
		if file != "" {
			data, err = os.ReadFile(file)
		} else {
			data, err = fetchFile(repo, ref, dir, "package-lock.json")
		}
		return err
	})
	if fetchErr != nil {
		// Deliberately not resetting the session here: if a previous
		// analyze_repository already succeeded this conversation, a failed
		// retry (bad path, typo) shouldn't throw that away.
		return "error fetching lockfile: " + fetchErr.Error()
	}
	s.reset()

	var g *Graph
	buildErr := withSpinner("Analyzing dependencies", func() error {
		var lf lockfile
		if err := json.Unmarshal(data, &lf); err != nil {
			return err
		}
		if len(lf.Packages) == 0 && len(lf.V1Deps) > 0 {
			pj, err := loadPackageJSON(repo, ref, dir, file)
			if err != nil {
				return fmt.Errorf("v1 lockfile needs package.json: %w", err)
			}
			convertV1(&lf, pj)
		}
		var err error
		g, err = buildGraph(&lf)
		return err
	})
	if buildErr != nil {
		return "error: " + buildErr.Error()
	}
	_, edges, direct := rankNodes(g)
	detail("%d packages, %d edges, %d direct dependencies", len(g.Nodes), edges, direct)

	s.graph = g
	if repo != "" {
		s.repo = repo
	} else {
		s.repo = file
	}

	vizPath := filepath.Join(reportDir, "graph.html")
	vizNote := ""
	_ = withSpinner("Creating a graph", func() error {
		if err := writeViz(g, vizPath, 150); err != nil {
			vizNote = "(interactive graph failed: " + err.Error() + ")"
			return err
		}
		vizNote = "Wrote an interactive graph to " + vizPath
		openInBrowser(vizPath)
		return nil
	})
	detail("%s", vizPath)

	_ = withSpinner("Cloning repo", func() error {
		if file != "" {
			s.srcDir = filepath.Dir(file)
			return nil
		}
		d, cleanup, err := cloneRepo(repo, ref)
		if err != nil {
			return err // usage scan below reports "no cloned source available" on its own
		}
		s.srcDir = filepath.Join(d, dir)
		s.cleanup = cleanup
		return nil
	})

	report := graphReport(g, 15)

	// Usage scan and health check both run automatically here rather than
	// waiting for the model to decide to call them separately — they're now
	// a default part of "analyze this repo." Usage runs first so health's
	// advisories can be judged against how deeply each package is actually
	// used in this project, not just its abstract severity.
	usageText := s.scanUsage("")
	healthJSON := s.checkHealth(0)

	return report + "\n" + vizNote + "\n" + usageText + "\n" + healthJSON
}

func (s *session) scanUsage(pkg string) string {
	if s.graph == nil {
		return "error: call analyze_repository first"
	}
	if s.srcDir == "" {
		return "error: no cloned source available for usage scanning"
	}
	var text string
	_ = withSpinner("Scanning usage", func() error {
		var result UsageScan
		text, result = runScan(s.graph, s.srcDir, pkg, false)
		s.usage = result
		return nil
	})
	_, _, direct := rankNodes(s.graph)
	detail("%d source files — %d/%d direct dependencies actually imported", s.usage.Scanned, len(s.usage.Rows), direct)
	return text
}

// healthWithUsage merges a Health record with its usage-scan cross-reference
// into one JSON object, so the model sees "this package has a HIGH severity
// advisory AND is only used in a test file" as a single fact instead of
// having to join two separate tool outputs itself — a small local model
// reliably uses an inline field but can't be trusted to cross-reference two
// tables on its own.
type healthWithUsage struct {
	*Health
	UsedInProject  string `json:"usedInProject"`
	Classification string `json:"classification"` // computed deterministically — see classifyUsage; don't second-guess it
}

// checkHealth is called automatically once per analyze_repository (covering
// direct dependencies), and remains available as a tool the model can call
// again with a non-zero top to extend coverage to more packages by blast
// radius.
func (s *session) checkHealth(top int) string {
	if s.graph == nil {
		return "error: call analyze_repository first"
	}
	var rows []*Health
	_ = withSpinner("Checking dependency health (npm, GitHub, OSV.dev)", func() error {
		rows = runHealth(s.graph, top)
		return nil
	})
	s.health = rows
	clean, problems := splitHealthProblems(rows)
	advisories := 0
	for _, h := range clean {
		advisories += len(h.Advisories)
	}
	score := computeRepoScore(clean, s.usage.Rows, s.usage.Rows != nil)
	detail("%d packages checked — %d advisories found — %d could not be evaluated — score %d/100 (%s)",
		len(clean), advisories, len(problems), score.Value, score.Label)

	byPkg := map[string]*Usage{}
	for _, u := range s.usage.Rows {
		byPkg[u.Package] = u
	}
	// Only clean (fully-evaluated) packages go to the model — packages where
	// the registry lookup 404'd or some other check errored out have no
	// reliable signal behind them and must never be treated as a finding.
	out := make([]healthWithUsage, len(clean))
	for i, h := range clean {
		u := byPkg[h.Package]
		usedIn := "usage not scanned"
		if s.usage.Rows != nil {
			usedIn = usageSummary(u)
		}
		out[i] = healthWithUsage{Health: h, UsedInProject: usedIn, Classification: classifyUsage(s.usage.Rows != nil, h.Dev, u)}
	}

	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "error formatting health report: " + err.Error()
	}
	result := fmt.Sprintf("Overall dependency health score: %d/100 (%s) — computed deterministically from advisory severity, abandonment risk, archived repos, and unused-candidate bloat, weighted by how each package is actually used. Mention this score in your assessment.\n\n", score.Value, score.Label) + string(b)
	if len(problems) > 0 {
		names := make([]string, len(problems))
		for i, h := range problems {
			names[i] = h.Package
		}
		result += fmt.Sprintf("\n\n(%d package(s) could not be evaluated — registry lookup or other errors — and are intentionally excluded above: %s. They go in the report's Problems section, not your assessment. Do not discuss them as findings.)",
			len(problems), strings.Join(names, ", "))
	}
	return result
}

// ---------- tool schemas + dispatch ----------

func agentTools() []ollamaTool {
	return []ollamaTool{
		newTool("analyze_repository",
			"Clone a GitHub repo (or read a local lockfile), parse its npm lockfile, build the dependency graph, compute blast-radius metrics, and immediately open an interactive HTML visualization. Call this first for any repository analysis request.",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"repo": map[string]any{"type": "string", "description": "GitHub repo as owner/name, e.g. cloudflare/mcp"},
					"file": map[string]any{"type": "string", "description": "path to a local package-lock.json, as an alternative to repo"},
					"ref":  map[string]any{"type": "string", "description": "branch, tag, or commit SHA (optional, defaults to the default branch)"},
					"dir":  map[string]any{"type": "string", "description": "subdirectory containing package-lock.json, for monorepos (optional)"},
				},
			}),
		newTool("scan_usage",
			"Find where direct dependencies are actually imported in the cloned source code, separating production from test usage. Requires analyze_repository to have been called first in this conversation.",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"package": map[string]any{"type": "string", "description": "optional package name to show sampled usage snippets for"},
				},
			}),
		newTool("check_health",
			"Re-run or extend the dependency health check (npm registry metadata, GitHub repo/contributor stats, OSV.dev advisories, bus factor). analyze_repository already runs this once automatically for direct dependencies — only call this yourself if you need to extend coverage to more packages via top.",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"top": map[string]any{"type": "integer", "description": "additionally check this many extra packages ranked by blast radius, beyond the direct dependencies (optional, default 0)"},
				},
			}),
	}
}

func dispatchTool(tc ollamaToolCall, sess *session) string {
	switch tc.Function.Name {
	case "analyze_repository":
		var args struct {
			Repo string `json:"repo"`
			File string `json:"file"`
			Ref  string `json:"ref"`
			Dir  string `json:"dir"`
		}
		_ = json.Unmarshal(tc.Function.Arguments, &args)
		return sess.analyzeRepository(args.Repo, args.File, args.Ref, args.Dir)
	case "scan_usage":
		var args struct {
			Package string `json:"package"`
		}
		_ = json.Unmarshal(tc.Function.Arguments, &args)
		return sess.scanUsage(args.Package)
	case "check_health":
		var args struct {
			Top int `json:"top"`
		}
		_ = json.Unmarshal(tc.Function.Arguments, &args)
		return sess.checkHealth(args.Top)
	default:
		return "unknown tool: " + tc.Function.Name
	}
}

// ---------- GitHub token gate ----------

func ensureGithubToken() {
	if os.Getenv("GITHUB_TOKEN") != "" {
		return
	}
	fmt.Println("Atlas needs a GitHub token to clone repos and check dependency health.")
	fmt.Println("(it isn't in your environment or .env — this won't be saved anywhere, just used for this run)")
	fmt.Print("GITHUB_TOKEN: ")
	reader := bufio.NewReader(os.Stdin)
	tok, _ := reader.ReadString('\n')
	tok = strings.TrimSpace(tok)
	if tok == "" {
		fmt.Fprintln(os.Stderr, "no token provided, exiting")
		os.Exit(1)
	}
	os.Setenv("GITHUB_TOKEN", tok)
}

// ---------- REPL ----------

const maxToolHops = 6

func runAgentMode(model string) {
	ensureGithubToken()

	if err := checkOllamaModel(model); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	fmt.Printf("Atlas — dependency risk analysis agent (model: %s)\n", model)
	fmt.Println(`Try: "run dependency analysis on cloudflare/mcp". Type "exit" to quit.`)

	sess := &session{}
	defer sess.reset()

	history := []ollamaMessage{{Role: "system", Content: systemPrompt}}
	tools := agentTools()

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("\nyou> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			break // EOF (ctrl-D)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			break
		}

		history = append(history, ollamaMessage{Role: "user", Content: line})
		if err := runAgentTurn(model, &history, tools, sess); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
	}
}

// runAgentTurn drives the model until it produces a final answer with no
// further tool calls, executing each requested tool in between. If any
// tools ran this turn, the combined report (graph + usage + health + the
// model's final answer) is written and opened exactly once, at the end.
func runAgentTurn(model string, history *[]ollamaMessage, tools []ollamaTool, sess *session) error {
	toolsRan := false
	for hop := 0; hop < maxToolHops; hop++ {
		think := startSpinner("Thinking")
		msg, err := chatOllama(model, *history, tools)
		think.stopSilent()
		if err != nil {
			return err
		}
		*history = append(*history, msg)

		if len(msg.ToolCalls) == 0 {
			answer := strings.TrimSpace(msg.Content)
			// Only the HTML report gets the model's actual assessment text —
			// the terminal just points at it, so it doesn't end up re-reading
			// a wall of text it can get in the browser instead.
			if toolsRan && sess.graph != nil {
				announceReport(sess, answer)
			} else {
				fmt.Println("\natlas> " + plainText(answer))
			}
			return nil
		}

		toolsRan = true
		// Sort tool calls deterministically by name before executing, so
		// analyze_repository (if present) always runs before dependents like
		// scan_usage/check_health that rely on its session state.
		calls := append([]ollamaToolCall(nil), msg.ToolCalls...)
		sort.SliceStable(calls, func(i, j int) bool {
			return calls[i].Function.Name == "analyze_repository" && calls[j].Function.Name != "analyze_repository"
		})
		for _, tc := range calls {
			result := dispatchTool(tc, sess)
			*history = append(*history, ollamaMessage{Role: "tool", Content: result})
		}
	}
	fmt.Println("\natlas> (stopped after several tool calls without a final answer — try a more specific question)")
	if sess.graph != nil {
		announceReport(sess, "(stopped after several tool calls without a final answer)")
	}
	return nil
}

// announceReport writes the session report and tells the user where to find
// it — never the report's actual content, just a pointer to it.
// plainText strips the markdown syntax the model might produce (bold,
// headings, code spans) before printing straight to the terminal, which
// doesn't render any of it — unlike the HTML report, which converts the same
// markdown into real HTML via renderReasoning. Reuses the regexes already
// defined in report.go rather than duplicating the patterns.
func plainText(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if header, rows, consumed, ok := parseMarkdownTable(lines, i); ok {
			plainHeader := make([]string, len(header))
			for j, c := range header {
				plainHeader[j] = stripInlineMarkdown(c)
			}
			plainRows := make([][]string, len(rows))
			for j, row := range rows {
				plainRow := make([]string, len(row))
				for k, c := range row {
					plainRow[k] = stripInlineMarkdown(c)
				}
				plainRows[j] = plainRow
			}
			out = append(out, strings.Split(strings.TrimRight(renderTableForTerminal(plainHeader, plainRows), "\n"), "\n")...)
			i += consumed - 1
			continue
		}
		if mdFence.MatchString(strings.TrimSpace(line)) {
			continue // drop ``` fence delimiters; keep the code content between them
		}
		if m := mdHeading.FindStringSubmatch(line); m != nil {
			line = m[2]
		}
		out = append(out, stripInlineMarkdown(line))
	}
	return strings.Join(out, "\n")
}

// renderTableForTerminal turns parsed markdown table cells into a
// whitespace-aligned ASCII table — a terminal has no real table rendering,
// so column alignment has to be computed and baked in as literal padding.
// stripInlineMarkdown removes **bold** and `code` markup, leaving the
// underlying text — a terminal has no rendering for either.
func stripInlineMarkdown(s string) string {
	s = mdCode.ReplaceAllString(s, "$1")
	return mdBold.ReplaceAllString(s, "$1")
}

func renderTableForTerminal(header []string, rows [][]string) string {
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

func announceReport(sess *session, reasoning string) {
	path, err := sess.writeSessionReport(reasoning)
	if err != nil {
		fmt.Fprintln(os.Stderr, "report generation failed:", err)
		return
	}
	fmt.Printf("\n✓ I've prepared a report for you: %s — please go through it, and feel free to ask follow-up questions.\n", path)
}
