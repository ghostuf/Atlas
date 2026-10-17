package main

// Step 2: find where each direct dependency is actually used in the source tree,
// and collect small code snippets ("usage sites") that the LLM will later read.

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Matches the module specifier in:
//
//	import x from 'pkg'      import 'pkg'      export * from 'pkg'
//	require('pkg')           import('pkg')
var importRe = regexp.MustCompile(`(?:\bfrom\s+|\bimport\s+|\brequire\s*\(\s*|\bimport\s*\(\s*)['"]([^'"]+)['"]`)

var skipDirs = map[string]bool{
	"node_modules": true, ".git": true, "dist": true, "build": true,
	"coverage": true, "vendor": true, ".next": true, "out": true,
}

var srcExts = map[string]bool{
	".js": true, ".jsx": true, ".mjs": true, ".cjs": true, ".ts": true, ".tsx": true,
}

// configFileNames are files read for the secondary "referenced by name"
// pass (below) even though they're not application source code — config
// files are exactly where build-tool plugins get named without ever being
// require()'d or import'd anywhere (ESLint "extends", Babel "presets", a
// Grunt config object, npm "scripts", etc).
var configFileNames = map[string]bool{
	"package.json":       true,
	".eslintrc":          true,
	".eslintrc.json":     true,
	".babelrc":           true,
	".babelrc.json":      true,
	".prettierrc":        true,
	".prettierrc.json":   true,
	".stylelintrc":       true,
	".stylelintrc.json":  true,
	".commitlintrc":      true,
	".commitlintrc.json": true,
}

// packageJSONDepKeys lists package.json's own dependency-listing fields —
// every direct dependency trivially appears there by definition (that's
// what makes it a direct dependency), so searching those fields would mark
// every single package as "config-referenced" regardless of real usage.
// They're stripped out before the bare-name search runs on package.json.
var packageJSONDepKeys = []string{
	"dependencies", "devDependencies", "peerDependencies",
	"optionalDependencies", "bundledDependencies", "bundleDependencies",
}

// packageJSONSearchText returns package.json's content with the dependency
// list fields removed, so only the fields that actually mean something
// ("scripts", "eslintConfig", "babel", "prettier", "jest", ...) are
// searched. Returns "" if the file isn't valid JSON.
func packageJSONSearchText(data []byte) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	for _, k := range packageJSONDepKeys {
		delete(m, k)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

func isConfigFile(name string) bool {
	if configFileNames[name] {
		return true
	}
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".yml") || strings.HasSuffix(lower, ".yaml")
}

// Cheap path-based hints about which architectural area a file belongs to.
var tagKeywords = map[string][]string{
	"auth":       {"auth", "login", "session", "passport", "jwt", "token"},
	"db":         {"db", "database", "model", "repository", "prisma", "sql", "migration"},
	"routing":    {"route", "router", "controller", "handler", "server", "api"},
	"middleware": {"middleware"},
	"config":     {"config", "env"},
}

type Site struct {
	File    string // path relative to scan root
	Line    int    // 1-based
	Snippet string // surrounding lines, numbered
	IsTest  bool
}

type Usage struct {
	Package     string
	Sites       []Site
	ProdFiles   map[string]bool
	TestFiles   map[string]bool
	Dirs        map[string]bool // top-level directories touched (prod files only)
	Tags        map[string]bool // auth/db/routing/... seen in prod file paths
	ConfigFiles map[string]bool // files where the package name appeared as a string, not a parsed import — weaker evidence, see containsWholeToken
}

func pkgFromSpecifier(spec string) (string, bool) {
	if spec == "" || spec[0] == '.' || spec[0] == '/' || strings.HasPrefix(spec, "node:") {
		return "", false
	}
	parts := strings.Split(spec, "/")
	if spec[0] == '@' {
		if len(parts) < 2 {
			return "", false
		}
		return parts[0] + "/" + parts[1], true
	}
	return parts[0], true
}

func isTestPath(rel string) bool {
	l := strings.ToLower(rel)
	return strings.Contains(l, "test") || strings.Contains(l, "spec") || strings.Contains(l, "__mocks__")
}

// stripComments removes comments before either detection pass runs, so a
// package name (or import) mentioned only in dead/commented-out code never
// counts as real usage. It's a line-based heuristic, not a real parser —
// consistent with the regex-based approach used everywhere else in this
// file — so it won't handle every edge case (e.g. "//" or "/*" embedded in
// a string literal), but it covers the common cases.
func stripComments(name string, lines []string) []string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".yml") || strings.HasSuffix(lower, ".yaml"):
		return stripYAMLComments(lines)
	case strings.HasSuffix(lower, ".json"):
		return lines // JSON has no comments
	default:
		return stripJSComments(lines) // .js/.ts family, plus extensionless dotfiles (.eslintrc, .babelrc)
	}
}

func stripJSComments(lines []string) []string {
	out := make([]string, len(lines))
	inBlock := false
	for i, l := range lines {
		if inBlock {
			if idx := strings.Index(l, "*/"); idx >= 0 {
				l = l[idx+2:]
				inBlock = false
			} else {
				out[i] = ""
				continue
			}
		}
		l = stripSlashSlashComment(l)
		if idx := strings.Index(l, "/*"); idx >= 0 {
			if end := strings.Index(l[idx+2:], "*/"); end >= 0 {
				l = l[:idx] + l[idx+2+end+2:]
			} else {
				l = l[:idx]
				inBlock = true
			}
		}
		out[i] = l
	}
	return out
}

// stripSlashSlashComment cuts a line at "//", unless it's immediately
// preceded by ":" — the common case of that being a URL (http://, https://)
// inside a string rather than a real comment start.
func stripSlashSlashComment(line string) string {
	idx := strings.Index(line, "//")
	for idx >= 0 {
		if idx == 0 || line[idx-1] != ':' {
			return line[:idx]
		}
		next := strings.Index(line[idx+2:], "//")
		if next < 0 {
			return line
		}
		idx = idx + 2 + next
	}
	return line
}

func stripYAMLComments(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		if idx := strings.Index(l, "#"); idx >= 0 {
			out[i] = l[:idx]
		} else {
			out[i] = l
		}
	}
	return out
}

func isIdentChar(b byte) bool {
	return b == '_' || b == '.' ||
		('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z') || ('0' <= b && b <= '9')
}

// containsWholeToken reports whether name appears in line bounded by
// non-identifier characters on both sides (quotes, brackets, commas,
// whitespace, hyphens, slashes all count as boundaries) — so "jshint"
// doesn't falsely match inside "jshint2" or "myjshint".
func containsWholeToken(line, name string) bool {
	if name == "" {
		return false
	}
	idx := strings.Index(line, name)
	for idx >= 0 {
		var before, after byte
		if idx > 0 {
			before = line[idx-1]
		}
		if end := idx + len(name); end < len(line) {
			after = line[end]
		}
		if !isIdentChar(before) && !isIdentChar(after) {
			return true
		}
		next := strings.Index(line[idx+1:], name)
		if next < 0 {
			return false
		}
		idx += 1 + next
	}
	return false
}

func snippetAround(lines []string, i int) string {
	lo, hi := i-5, i+6
	if lo < 0 {
		lo = 0
	}
	if hi > len(lines) {
		hi = len(lines)
	}
	var b strings.Builder
	for j := lo; j < hi; j++ {
		l := lines[j]
		if len(l) > 200 {
			l = l[:200] + "…"
		}
		marker := " "
		if j == i {
			marker = ">"
		}
		fmt.Fprintf(&b, "%s%4d | %s\n", marker, j+1, l)
	}
	return b.String()
}

func getOrCreateUsage(usage map[string]*Usage, pkg string) *Usage {
	u := usage[pkg]
	if u == nil {
		u = &Usage{
			Package: pkg, ProdFiles: map[string]bool{}, TestFiles: map[string]bool{},
			Dirs: map[string]bool{}, Tags: map[string]bool{}, ConfigFiles: map[string]bool{},
		}
		usage[pkg] = u
	}
	return u
}

// scanDir walks root and records usage of any package in `wanted` (direct
// deps), via two passes per file:
//
//  1. A parsed import/require match (the strict, high-confidence signal).
//  2. A plain name match, comment-stripped, across a broader file set that
//     also includes config files (package.json, .eslintrc, etc.) — this
//     catches packages a build tool loads by convention rather than a
//     literal import statement (Grunt plugins named in a config object,
//     ESLint/Babel plugins named in their config, CLI tools named in
//     package.json's "scripts"). It's a weaker signal — a name match, not a
//     verified import — so it's tracked separately in Usage.ConfigFiles
//     rather than folded into ProdFiles/TestFiles.
func scanDir(root string, wanted map[string]bool) (map[string]*Usage, int, error) {
	usage := map[string]*Usage{}
	scanned := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			if skipDirs[d.Name()] && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		isSrc := srcExts[ext]
		isConfig := isConfigFile(d.Name())
		if !isSrc && !isConfig {
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > 1<<20 {
			return nil // skip huge files (usually bundles)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		scanned++

		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		isTest := isTestPath(rel)
		topDir := "."
		if i := strings.Index(rel, "/"); i > 0 {
			topDir = rel[:i]
		}
		lowerRel := strings.ToLower(rel)

		lines := strings.Split(string(data), "\n")
		stripped := stripComments(d.Name(), lines)

		if isSrc {
			for i, line := range stripped {
				for _, m := range importRe.FindAllStringSubmatch(line, -1) {
					pkg, ok := pkgFromSpecifier(m[1])
					if !ok || !wanted[pkg] {
						continue
					}
					u := getOrCreateUsage(usage, pkg)
					u.Sites = append(u.Sites, Site{File: rel, Line: i + 1, Snippet: snippetAround(lines, i), IsTest: isTest})
					if isTest {
						u.TestFiles[rel] = true
					} else {
						u.ProdFiles[rel] = true
						u.Dirs[topDir] = true
						for tag, kws := range tagKeywords {
							for _, kw := range kws {
								if strings.Contains(lowerRel, kw) {
									u.Tags[tag] = true
									break
								}
							}
						}
					}
				}
			}
		}

		// Secondary pass: does the bare package name show up anywhere in
		// this file (comments already stripped), regardless of whether it
		// was already matched as a real import above? A package can be
		// both actually imported AND separately named in a config file.
		// package.json's own dependency-listing fields are excluded first —
		// every direct dep trivially appears there, which isn't evidence of
		// anything.
		configStripped := stripped
		if d.Name() == "package.json" {
			cleaned := packageJSONSearchText(data)
			if cleaned == "" {
				configStripped = nil // not valid JSON; don't risk false positives on raw text
			} else {
				configStripped = stripComments(d.Name(), strings.Split(cleaned, "\n"))
			}
		}
		for pkg := range wanted {
			u := usage[pkg]
			if u != nil && (u.ProdFiles[rel] || u.TestFiles[rel]) {
				continue // already a real import in this exact file — don't also tag it as the weaker config-reference signal
			}
			for _, line := range configStripped {
				if containsWholeToken(line, pkg) {
					getOrCreateUsage(usage, pkg).ConfigFiles[rel] = true
					break
				}
			}
		}
		return nil
	})
	return usage, scanned, err
}

// sampleSites picks up to n sites, preferring production code and distinct files.
func sampleSites(u *Usage, n int) []Site {
	sites := append([]Site(nil), u.Sites...)
	sort.SliceStable(sites, func(i, j int) bool { return !sites[i].IsTest && sites[j].IsTest })
	var out []Site
	seen := map[string]bool{}
	for _, s := range sites {
		if len(out) >= n {
			break
		}
		if !seen[s.File] {
			seen[s.File] = true
			out = append(out, s)
		}
	}
	return out
}

func sortedKeys(m map[string]bool) string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return strings.Join(k, ",")
}

// sortedFileList is sortedKeys but returns the slice instead of a joined
// string, for callers that want to render file names individually (e.g. in
// an HTML list) rather than as one comma-separated blob.
func sortedFileList(m map[string]bool) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

// UsageScan is the structured result of runScan, for callers (the HTML
// report) that want to render it themselves instead of using the text form.
type UsageScan struct {
	Rows    []*Usage // sorted by prod-file count, descending
	Unused  []string // direct deps never imported in source
	Scanned int      // source files scanned
	Root    string
}

// runScan builds the usage report as text and returns both the text (for the
// model/terminal) and the structured data behind it (for the HTML report).
// print controls whether it's also echoed to stdout — the CLI wants that,
// the interactive agent shows its own animated status instead.
func runScan(g *Graph, root, show string, print bool) (string, UsageScan) {
	wanted := map[string]bool{}
	for _, n := range g.Nodes {
		if n.Direct {
			wanted[n.Name] = true
		}
	}
	usage, scanned, err := scanDir(root, wanted)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scan error:", err)
		return "", UsageScan{}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\nScanned %d source files under %s\n\n", scanned, root)

	var rows []*Usage
	for _, u := range usage {
		rows = append(rows, u)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if len(a.ProdFiles) != len(b.ProdFiles) {
			return len(a.ProdFiles) > len(b.ProdFiles)
		}
		return a.Package < b.Package
	})

	fmt.Fprintf(&b, "%-32s %-10s %-10s %-11s %-6s %-6s %s\n", "PACKAGE", "PROD_FILES", "TEST_FILES", "CONFIG_REFS", "DIRS", "SITES", "TAGS")
	for _, u := range rows {
		fmt.Fprintf(&b, "%-32s %-10d %-10d %-11d %-6d %-6d %s\n",
			trunc(u.Package, 32), len(u.ProdFiles), len(u.TestFiles), len(u.ConfigFiles), len(u.Dirs), len(u.Sites), sortedKeys(u.Tags))
	}
	fmt.Fprintf(&b, "\nCONFIG_REFS: the package name was found as a plain string in a config file "+
		"(package.json, .eslintrc, etc.), not a parsed import — weaker evidence than PROD_FILES/TEST_FILES, "+
		"but still a real signal for build-tool plugins loaded by convention (e.g. Grunt, ESLint, Babel).\n")

	var unused []string
	for name := range wanted {
		if usage[name] == nil {
			unused = append(unused, name)
		}
	}
	sort.Strings(unused)
	if len(unused) > 0 {
		fmt.Fprintf(&b, "\nDirect deps with no usage evidence at all (no import, no config reference) — genuinely unused, or loaded via a convention this scan can't detect: %s\n", strings.Join(unused, ", "))
	}

	if show != "" {
		u := usage[show]
		if u == nil {
			fmt.Fprintf(&b, "\nNo usage found for %q\n", show)
		} else {
			fmt.Fprintf(&b, "\n=== sampled usage of %s ===\n", show)
			for _, s := range sampleSites(u, 8) {
				fmt.Fprintf(&b, "\n%s:%d\n%s", s.File, s.Line, s.Snippet)
			}
		}
	}

	if print {
		fmt.Print(b.String())
	}
	return b.String(), UsageScan{Rows: rows, Unused: unused, Scanned: scanned, Root: root}
}

// cloneRepo shallow-clones github.com/<repo> into a temp dir (needs git installed).
// When GITHUB_TOKEN is set it's embedded in the URL so private repos clone too.
func cloneRepo(repo, ref string) (string, func(), error) {
	tmp, err := os.MkdirTemp("", "atlas-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(tmp) }
	args := []string{"clone", "--depth", "1"}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	cloneURL := "https://github.com/" + repo + ".git"
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		cloneURL = "https://x-access-token:" + tok + "@github.com/" + repo + ".git"
	}
	args = append(args, cloneURL, tmp)
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		cleanup()
		msg := string(out)
		// git can echo the clone URL (and therefore the embedded token) in
		// its error output; never let the token reach the terminal or logs.
		if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
			msg = strings.ReplaceAll(msg, tok, "***")
		}
		return "", nil, fmt.Errorf("git clone failed: %v\n%s", err, msg)
	}
	return tmp, cleanup, nil
}
