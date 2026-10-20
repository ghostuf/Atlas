package core

// Atlas step 1: build a dependency graph G=(V,E) from an npm package-lock.json
// (lockfileVersion 2 or 3) and compute structural metrics.

import (
	"fmt"
	"sort"
	"strings"
)

// ---------- lockfile parsing ----------

// LockEntry represents an entry inside package-lock.json packages map.
type LockEntry struct {
	Version              string            `json:"version"`
	Link                 bool              `json:"link"`
	Dev                  bool              `json:"dev"`
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
}

// Lockfile represents the root package-lock.json structure.
type Lockfile struct {
	Name            string               `json:"name"`
	LockfileVersion int                  `json:"lockfileVersion"`
	Packages        map[string]LockEntry `json:"packages"`     // v2/v3
	V1Deps          map[string]V1Entry   `json:"dependencies"` // v1 (and legacy section of v2)
}

// ---------- graph ----------

// Node is a vertex: one installed package (a specific path in node_modules).
type Node struct {
	ID      string // lockfile key, e.g. "node_modules/a/node_modules/b"; "" = root
	Name    string // e.g. "b" or "@scope/b"
	Version string
	Dev     bool

	Out []string // edges: packages this node depends on
	In  []string // reverse edges: packages that depend on this node

	// computed metrics
	Depth                int  // hops from root (1 = direct dependency)
	Direct               bool // listed in root manifest
	TransitiveDependents int  // distinct packages that reach this one (excl. root)
}

// Graph represents the entire dependency graph.
type Graph struct {
	Nodes     map[string]*Node
	Root      *Node
	Ecosystem Ecosystem // which package manager this graph was built from
}

// NameFromPath turns "node_modules/a/node_modules/@s/b" into "@s/b".
func NameFromPath(p string) string {
	i := strings.LastIndex(p, "node_modules/")
	if i < 0 {
		return p
	}
	return p[i+len("node_modules/"):]
}

// Resolve mimics Node's resolution: look in from/node_modules/name, then walk up
// parent node_modules directories until the root.
func Resolve(pkgs map[string]LockEntry, from, name string) (string, bool) {
	base := from
	for {
		cand := "node_modules/" + name
		if base != "" {
			cand = base + "/node_modules/" + name
		}
		if _, ok := pkgs[cand]; ok {
			return cand, true
		}
		if base == "" {
			return "", false
		}
		i := strings.LastIndex(base, "node_modules/")
		if i <= 0 {
			base = ""
		} else {
			base = strings.TrimSuffix(base[:i], "/")
		}
	}
}

// BuildGraph builds the in-memory dependency graph from parsed lockfile.
func BuildGraph(lf *Lockfile) (*Graph, error) {
	if len(lf.Packages) == 0 {
		return nil, lockfileEmptyError(lf)
	}
	g := &Graph{Nodes: map[string]*Node{}, Ecosystem: EcosystemNPM}

	for id, e := range lf.Packages {
		n := &Node{ID: id, Name: NameFromPath(id), Version: e.Version, Dev: e.Dev}
		if id == "" {
			n.Name = lf.Name
			if n.Name == "" {
				n.Name = "(root)"
			}
		}
		g.Nodes[id] = n
	}
	g.Root = g.Nodes[""]
	if g.Root == nil {
		return nil, fmt.Errorf("lockfile has no root entry")
	}

	addEdges := func(from string, deps map[string]string) {
		for name := range deps {
			to, ok := Resolve(lf.Packages, from, name)
			if !ok {
				continue // optional/peer dep that was not installed
			}
			g.Nodes[from].Out = append(g.Nodes[from].Out, to)
			g.Nodes[to].In = append(g.Nodes[to].In, from)
		}
	}
	for id, e := range lf.Packages {
		addEdges(id, e.Dependencies)
		addEdges(id, e.OptionalDependencies)
		addEdges(id, e.PeerDependencies)
		if id == "" {
			addEdges(id, e.DevDependencies)
		}
	}
	FinalizeGraph(g)
	return g, nil
}

// FinalizeGraph dedupes edges and computes depth/direct/blast-radius
// metrics. Any graph builder — npm's BuildGraph, or another ecosystem's
// package building its own graph from its own lockfile format — calls this
// once after wiring all nodes and edges.
func FinalizeGraph(g *Graph) {
	dedupe(g)
	computeMetrics(g)
}

func dedupe(g *Graph) {
	uniq := func(s []string) []string {
		seen := map[string]bool{}
		out := s[:0]
		for _, x := range s {
			if !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
		return out
	}
	for _, n := range g.Nodes {
		n.Out = uniq(n.Out)
		n.In = uniq(n.In)
	}
}

func computeMetrics(g *Graph) {
	// Depth: BFS from root along forward edges.
	depth := map[string]int{"": 0}
	queue := []string{""}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, nxt := range g.Nodes[cur].Out {
			if _, seen := depth[nxt]; !seen {
				depth[nxt] = depth[cur] + 1
				queue = append(queue, nxt)
			}
		}
	}
	for id, n := range g.Nodes {
		if d, ok := depth[id]; ok {
			n.Depth = d
		} else {
			n.Depth = -1 // not reachable from root (e.g. unused workspace)
		}
		n.Direct = n.Depth == 1
	}

	// Transitive dependents: reverse BFS from each node along In edges.
	// This is the blast-radius upper bound: "if this breaks, who is affected?"
	for id, n := range g.Nodes {
		if id == "" {
			continue
		}
		seen := map[string]bool{id: true}
		stack := []string{id}
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, parent := range g.Nodes[cur].In {
				if !seen[parent] {
					seen[parent] = true
					stack = append(stack, parent)
				}
			}
		}
		count := len(seen) - 1 // exclude itself
		if seen[""] {
			count-- // exclude the root app
		}
		n.TransitiveDependents = count
	}
}

// RankNodes returns every reachable, non-root node ranked by blast-radius
// upper bound (then direct fan-in), plus the raw edge/direct-dep counts.
func RankNodes(g *Graph) (list []*Node, edges, direct int) {
	for id, n := range g.Nodes {
		edges += len(n.Out)
		if id == "" || n.Depth < 0 {
			continue
		}
		if n.Direct {
			direct++
		}
		list = append(list, n)
	}

	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.TransitiveDependents != b.TransitiveDependents {
			return a.TransitiveDependents > b.TransitiveDependents
		}
		if len(a.In) != len(b.In) {
			return len(a.In) > len(b.In)
		}
		return a.Name < b.Name
	})
	return list, edges, direct
}

// GraphReport renders the summary + ranked table as text.
func GraphReport(g *Graph, top int) string {
	list, edges, direct := RankNodes(g)

	var b strings.Builder
	fmt.Fprintf(&b, "Graph: %d vertices, %d edges, %d direct deps\n\n", len(g.Nodes), edges, direct)
	fmt.Fprintf(&b, "%-40s %-10s %-7s %-6s %-9s %s\n", "PACKAGE", "VERSION", "DIRECT", "DEPTH", "DEPENDENTS", "TRANSITIVE")
	for i, n := range list {
		if i >= top {
			break
		}
		fmt.Fprintf(&b, "%-40s %-10s %-7v %-6d %-9d  %d\n",
			Trunc(n.Name, 40), Trunc(n.Version, 10), n.Direct, n.Depth, len(n.In), n.TransitiveDependents)
	}
	return b.String()
}

// Trunc truncates a string with an ellipsis if longer than n.
func Trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
