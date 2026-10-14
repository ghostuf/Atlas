package main

// Step 2: find where each direct dependency is actually used in the source tree,
// and collect small code snippets ("usage sites") that the LLM will later read.

import (
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
	Package   string
	Sites     []Site
	ProdFiles map[string]bool
	TestFiles map[string]bool
	Dirs      map[string]bool // top-level directories touched (prod files only)
	Tags      map[string]bool // auth/db/routing/... seen in prod file paths
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

// scanDir walks root and records usage of any package in `wanted` (direct deps).
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
		if !srcExts[strings.ToLower(filepath.Ext(path))] {
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
		for i, line := range lines {
			for _, m := range importRe.FindAllStringSubmatch(line, -1) {
				pkg, ok := pkgFromSpecifier(m[1])
				if !ok || !wanted[pkg] {
					continue
				}
				u := usage[pkg]
				if u == nil {
					u = &Usage{
						Package: pkg, ProdFiles: map[string]bool{}, TestFiles: map[string]bool{},
						Dirs: map[string]bool{}, Tags: map[string]bool{},
					}
					usage[pkg] = u
				}
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

func runScan(g *Graph, root, show string) {
	wanted := map[string]bool{}
	for _, n := range g.Nodes {
		if n.Direct {
			wanted[n.Name] = true
		}
	}
	usage, scanned, err := scanDir(root, wanted)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scan error:", err)
		return
	}
	fmt.Printf("\nScanned %d source files under %s\n\n", scanned, root)

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

	fmt.Printf("%-32s %-10s %-10s %-6s %-6s %s\n", "PACKAGE", "PROD_FILES", "TEST_FILES", "DIRS", "SITES", "TAGS")
	for _, u := range rows {
		fmt.Printf("%-32s %-10d %-10d %-6d %-6d %s\n",
			trunc(u.Package, 32), len(u.ProdFiles), len(u.TestFiles), len(u.Dirs), len(u.Sites), sortedKeys(u.Tags))
	}

	var unused []string
	for name := range wanted {
		if usage[name] == nil {
			unused = append(unused, name)
		}
	}
	sort.Strings(unused)
	if len(unused) > 0 {
		fmt.Printf("\nDirect deps never imported in source (CLI tools, plugins, @types, or unused): %s\n", strings.Join(unused, ", "))
	}

	if show != "" {
		u := usage[show]
		if u == nil {
			fmt.Printf("\nNo usage found for %q\n", show)
			return
		}
		fmt.Printf("\n=== sampled usage of %s ===\n", show)
		for _, s := range sampleSites(u, 8) {
			fmt.Printf("\n%s:%d\n%s", s.File, s.Line, s.Snippet)
		}
	}
}

// cloneRepo shallow-clones github.com/<repo> into a temp dir (needs git installed).
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
	args = append(args, "https://github.com/"+repo+".git", tmp)
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("git clone failed: %v\n%s", err, out)
	}
	return tmp, cleanup, nil
}
