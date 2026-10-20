// Package python implements Atlas's Python (Poetry) ecosystem support:
// poetry.lock + pyproject.toml parsing, PyPI health signals, and Python
// import-based usage scanning. It imports atlas/core for shared types
// (core.Node, core.Graph, core.Health, ...); core never imports this package back (that
// would be a cycle) — instead core defines an EcosystemAdapter interface,
// and Adapter (adapter.go) implements it, wired up by the caller
// (main.go/agents/orchestrator.go) once the ecosystem is detected.
package python

// poetry.lock (TOML, flat and fully resolved — one pinned version per
// package, no node_modules-style nesting) plus its companion pyproject.toml
// (which, like npm v1 lockfiles needing a companion package.json, is the
// only place a poetry project's actual *direct* dependencies are declared —
// poetry.lock itself carries zero root/direct-dependency information).

import (
	"atlas/core"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// PoetryLockEntry is one [[package]] block from poetry.lock.
type PoetryLockEntry struct {
	Name, Version, Category string
	Groups                  []string
	Dependencies            map[string]bool // dependency name -> present; constraint value ignored
}

// IsDev handles both lock-format generations: lock-version 2.0+ uses
// `groups = ["main"]` (any group other than "main" counts as dev), while
// lock-version <2.0 uses `category = "main"/"dev"`. An entry with neither
// set (shouldn't happen for a real poetry.lock) is treated as main rather
// than silently dropped from the runtime classification.
func (e PoetryLockEntry) IsDev() bool {
	if len(e.Groups) > 0 {
		return !slices.Contains(e.Groups, "main")
	}
	return e.Category == "dev"
}

// ParsePoetryLock parses poetry.lock into its package entries.
func ParsePoetryLock(data []byte) ([]PoetryLockEntry, error) {
	sections, err := ParseTOMLSections(data)
	if err != nil {
		return nil, err
	}

	var entries []PoetryLockEntry
	var cur *PoetryLockEntry
	for _, s := range sections {
		switch {
		case len(s.Path) == 1 && s.Path[0] == "package" && s.IsArray:
			if cur != nil {
				entries = append(entries, *cur)
			}
			cur = &PoetryLockEntry{Dependencies: map[string]bool{}}
			for _, p := range s.Pairs {
				switch p.Key {
				case "name":
					cur.Name, _ = TOMLString(p.Raw)
				case "version":
					cur.Version, _ = TOMLString(p.Raw)
				case "category":
					cur.Category, _ = TOMLString(p.Raw)
				case "groups":
					cur.Groups, _ = TOMLStringArray(p.Raw)
				}
			}
		case len(s.Path) == 2 && s.Path[0] == "package" && s.Path[1] == "dependencies":
			if cur == nil {
				continue // malformed: dependencies sub-table before any [[package]]
			}
			for _, p := range s.Pairs {
				cur.Dependencies[p.Key] = true
			}
		}
	}
	if cur != nil {
		entries = append(entries, *cur)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no [[package]] entries found in poetry.lock")
	}
	return entries, nil
}

// PyProjectDeps is the subset of pyproject.toml Atlas needs: the project
// name and the names of its direct main/dev dependencies.
type PyProjectDeps struct {
	Name      string
	Main, Dev []string
}

// ParsePyProjectTOML reads [tool.poetry.dependencies] (main, skipping the
// "python" version-constraint key), legacy [tool.poetry.dev-dependencies],
// and any [tool.poetry.group.<name>.dependencies] (every group other than
// "main" itself counts as dev — matches poetry's own semantics for how
// `poetry install --only main` treats groups).
//
// Scope boundary: only the legacy [tool.poetry.*] sub-table shape is
// supported. PEP 621-native `[project] dependencies = ["requests>=2.0"]`
// (a flat array of requirement-spec strings, used by Poetry 2.x's
// non-legacy mode, PDM, Hatch) is structurally different and out of scope.
func ParsePyProjectTOML(data []byte) (*PyProjectDeps, error) {
	sections, err := ParseTOMLSections(data)
	if err != nil {
		return nil, err
	}
	pd := &PyProjectDeps{}
	for _, s := range sections {
		switch {
		case pathEquals(s.Path, "tool", "poetry"):
			for _, p := range s.Pairs {
				if p.Key == "name" {
					pd.Name, _ = TOMLString(p.Raw)
				}
			}
		case pathEquals(s.Path, "tool", "poetry", "dependencies"):
			for _, p := range s.Pairs {
				if p.Key != "python" {
					pd.Main = append(pd.Main, p.Key)
				}
			}
		case pathEquals(s.Path, "tool", "poetry", "dev-dependencies"):
			for _, p := range s.Pairs {
				pd.Dev = append(pd.Dev, p.Key)
			}
		case len(s.Path) == 5 && pathEquals(s.Path[:3], "tool", "poetry", "group") && s.Path[4] == "dependencies":
			for _, p := range s.Pairs {
				pd.Dev = append(pd.Dev, p.Key)
			}
		}
	}
	if len(pd.Main) == 0 && len(pd.Dev) == 0 {
		return nil, fmt.Errorf("no [tool.poetry.dependencies] found in pyproject.toml (PEP 621-native [project] dependencies are not supported)")
	}
	return pd, nil
}

func pathEquals(path []string, want ...string) bool {
	if len(path) != len(want) {
		return false
	}
	for i := range want {
		if path[i] != want[i] {
			return false
		}
	}
	return true
}

// LoadPyProjectTOML loads pyproject.toml from either a local sibling file
// next to the lockfile, or the GitHub repo — the same "lockfile alone isn't
// enough, fetch the companion manifest too" shape as LoadPackageJSON.
func LoadPyProjectTOML(repo, ref, dir, lockFile string) (*PyProjectDeps, error) {
	var data []byte
	var err error
	if lockFile != "" {
		data, err = os.ReadFile(filepath.Join(filepath.Dir(lockFile), "pyproject.toml"))
	} else {
		data, err = core.FetchFile(repo, ref, dir, "pyproject.toml")
	}
	if err != nil {
		return nil, err
	}
	return ParsePyProjectTOML(data)
}

// BuildGraphFromPoetry builds the dependency graph from parsed poetry.lock
// entries plus the companion pyproject.toml's direct-dependency lists.
// Unlike npm, no Resolve()/NameFromPath() equivalent is needed: poetry.lock
// is flat and fully resolved (one version per package, no nesting), so node
// IDs are simply the normalized package name.
func BuildGraphFromPoetry(entries []PoetryLockEntry, pd *PyProjectDeps) (*core.Graph, error) {
	g := &core.Graph{Nodes: map[string]*core.Node{}, Ecosystem: core.EcosystemPoetry}

	rootName := pd.Name
	if rootName == "" {
		rootName = "(root)"
	}
	g.Nodes[""] = &core.Node{ID: "", Name: rootName}
	g.Root = g.Nodes[""]

	for _, e := range entries {
		id := core.NormalizePyPIName(e.Name)
		g.Nodes[id] = &core.Node{ID: id, Name: e.Name, Version: e.Version, Dev: e.IsDev()}
	}

	addEdges := func(fromID string, deps map[string]bool) {
		for dep := range deps {
			toID := core.NormalizePyPIName(dep)
			if _, ok := g.Nodes[toID]; !ok {
				continue // optional extra that wasn't actually installed
			}
			g.Nodes[fromID].Out = append(g.Nodes[fromID].Out, toID)
			g.Nodes[toID].In = append(g.Nodes[toID].In, fromID)
		}
	}
	for _, e := range entries {
		addEdges(core.NormalizePyPIName(e.Name), e.Dependencies)
	}

	root := map[string]bool{}
	for _, n := range pd.Main {
		root[n] = true
	}
	for _, n := range pd.Dev {
		root[n] = true
	}
	addEdges("", root)

	core.FinalizeGraph(g)
	return g, nil
}

// BuildPoetryGraph is the single entry point main.go/agents/orchestrator.go
// call for a poetry project — the poetry equivalent of
// json.Unmarshal(data, &lf) + BuildGraph(&lf) for npm.
func BuildPoetryGraph(lockData []byte, repo, ref, dir, file string) (*core.Graph, error) {
	entries, err := ParsePoetryLock(lockData)
	if err != nil {
		return nil, fmt.Errorf("parse poetry.lock: %w", err)
	}
	pd, err := LoadPyProjectTOML(repo, ref, dir, file)
	if err != nil {
		return nil, fmt.Errorf("pyproject.toml: %w", err)
	}
	return BuildGraphFromPoetry(entries, pd)
}
