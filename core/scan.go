package core

// Step 2: find where each direct dependency is actually used in the source tree,
// and collect small code snippets ("usage sites") that the LLM will later read.

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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

var packageJSONDepKeys = []string{
	"dependencies", "devDependencies", "peerDependencies",
	"optionalDependencies", "bundledDependencies", "bundleDependencies",
}

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

var tagKeywords = map[string][]string{
	"auth":       {"auth", "login", "session", "passport", "jwt", "token"},
	"db":         {"db", "database", "model", "repository", "prisma", "sql", "migration"},
	"routing":    {"route", "router", "controller", "handler", "server", "api"},
	"middleware": {"middleware"},
	"config":     {"config", "env"},
}

// Site represents a single import site found in code.
type Site struct {
	File    string // path relative to scan root
	Line    int    // 1-based
	Snippet string // surrounding lines, numbered
	IsTest  bool
}

// Usage captures all usage signals detected for a package in the codebase.
type Usage struct {
	Package     string
	Sites       []Site
	ProdFiles   map[string]bool
	TestFiles   map[string]bool
	Dirs        map[string]bool // top-level directories touched (prod files only)
	Tags        map[string]bool // auth/db/routing/... seen in prod file paths
	ConfigFiles map[string]bool // files where the package name appeared as a string
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

func stripComments(name string, lines []string) []string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".yml") || strings.HasSuffix(lower, ".yaml"):
		return stripYAMLComments(lines)
	case strings.HasSuffix(lower, ".json"):
		return lines
	default:
		return stripJSComments(lines)
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

func scanDir(root string, wanted map[string]bool) (map[string]*Usage, int, error) {
	usage := map[string]*Usage{}
	scanned := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
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
			return nil
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

		configStripped := stripped
		if d.Name() == "package.json" {
			cleaned := packageJSONSearchText(data)
			if cleaned == "" {
				configStripped = nil
			} else {
				configStripped = stripComments(d.Name(), strings.Split(cleaned, "\n"))
			}
		}
		for pkg := range wanted {
			u := usage[pkg]
			if u != nil && (u.ProdFiles[rel] || u.TestFiles[rel]) {
				continue
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

// SampleSites picks up to n sites, preferring production code and distinct files.
func SampleSites(u *Usage, n int) []Site {
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

// SortedKeys returns comma-separated sorted keys of a map.
func SortedKeys(m map[string]bool) string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return strings.Join(k, ",")
}

// SortedFileList returns sorted keys of a map as a slice of strings.
func SortedFileList(m map[string]bool) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

// UsageScan is the structured result of RunScan.
type UsageScan struct {
	Rows    []*Usage // sorted by prod-file count, descending
	Unused  []string // direct deps never imported in source
	Scanned int      // source files scanned
	Root    string
}

// RunScan builds the usage report as text and returns both the text and structured data.
func RunScan(g *Graph, root, show string, print bool) (string, UsageScan) {
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
			Trunc(u.Package, 32), len(u.ProdFiles), len(u.TestFiles), len(u.ConfigFiles), len(u.Dirs), len(u.Sites), SortedKeys(u.Tags))
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
			for _, s := range SampleSites(u, 8) {
				fmt.Fprintf(&b, "\n%s:%d\n%s", s.File, s.Line, s.Snippet)
			}
		}
	}

	if print {
		fmt.Print(b.String())
	}
	return b.String(), UsageScan{Rows: rows, Unused: unused, Scanned: scanned, Root: root}
}

// Classification constants for dependency usage
const (
	ClassRuntime          = "runtime"
	ClassTestOnly         = "test-only"
	ClassConfigReferenced = "config-referenced"
	ClassBuildTooling     = "build-tooling"
	ClassUnusedCandidate  = "unused-candidate"
	ClassNotScanned       = "not-scanned"
)

// ClassifyUsage deterministically categorizes how a dependency is used.
func ClassifyUsage(scanned, isDev bool, u *Usage) string {
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

const maxUsageFilesListed = 8

// ListFiles renders up to maxUsageFilesListed file names, or count only.
func ListFiles(files []string) string {
	if len(files) > maxUsageFilesListed {
		return strconv.Itoa(len(files)) + " files"
	}
	return strings.Join(files, ", ")
}

// UsageSummary renders how deeply a package is actually used in this project.
func UsageSummary(u *Usage) string {
	if u == nil {
		return "not imported in source"
	}
	var parts []string
	if len(u.ProdFiles) > 0 {
		parts = append(parts, "prod: "+ListFiles(SortedFileList(u.ProdFiles)))
	}
	if len(u.TestFiles) > 0 {
		parts = append(parts, "test: "+ListFiles(SortedFileList(u.TestFiles)))
	}
	if len(u.ConfigFiles) > 0 {
		parts = append(parts, "config-referenced: "+ListFiles(SortedFileList(u.ConfigFiles)))
	}
	if len(parts) == 0 {
		return "not imported in source"
	}
	s := strings.Join(parts, "; ")
	if tags := SortedKeys(u.Tags); tags != "" {
		s += " (" + tags + ")"
	}
	return s
}
