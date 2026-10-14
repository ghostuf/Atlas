// Atlas step 1: build a dependency graph G=(V,E) from an npm package-lock.json
// (lockfileVersion 2 or 3) and compute structural metrics.
//
// Usage:
//
//	go run . -repo expressjs/express            # fetches package-lock.json from default branch
//	go run . -repo owner/repo -ref main -dir packages/app
//	go run . -file ./package-lock.json          # local file
//	go run . -repo owner/repo -top 30
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---------- lockfile parsing ----------

type lockEntry struct {
	Version              string            `json:"version"`
	Link                 bool              `json:"link"`
	Dev                  bool              `json:"dev"`
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
}

type lockfile struct {
	Name            string               `json:"name"`
	LockfileVersion int                  `json:"lockfileVersion"`
	Packages        map[string]lockEntry `json:"packages"`     // v2/v3
	V1Deps          map[string]v1Entry   `json:"dependencies"` // v1 (and legacy section of v2)
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

type Graph struct {
	Nodes map[string]*Node
	Root  *Node
}

// nameFromPath turns "node_modules/a/node_modules/@s/b" into "@s/b".
func nameFromPath(p string) string {
	i := strings.LastIndex(p, "node_modules/")
	if i < 0 {
		return p
	}
	return p[i+len("node_modules/"):]
}

// resolve mimics Node's resolution: look in from/node_modules/name, then walk up
// parent node_modules directories until the root.
func resolve(pkgs map[string]lockEntry, from, name string) (string, bool) {
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

func buildGraph(lf *lockfile) (*Graph, error) {
	if len(lf.Packages) == 0 {
		return nil, fmt.Errorf("no packages found in lockfile (lockfileVersion %d)", lf.LockfileVersion)
	}
	g := &Graph{Nodes: map[string]*Node{}}

	for id, e := range lf.Packages {
		n := &Node{ID: id, Name: nameFromPath(id), Version: e.Version, Dev: e.Dev}
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
			to, ok := resolve(lf.Packages, from, name)
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
	dedupe(g)
	computeMetrics(g)
	return g, nil
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

// ---------- fetching ----------

func fetchFile(repo, ref, dir, name string) ([]byte, error) {
	if ref == "" {
		ref = "HEAD" // raw.githubusercontent.com resolves HEAD to the default branch
	}
	path := name
	if dir != "" {
		path = strings.Trim(dir, "/") + "/" + path
	}
	url := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s", repo, ref, path)

	req, _ := http.NewRequest("GET", url, nil)
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %s (does the repo have a committed package-lock.json at that path?)", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// ---------- main ----------

func main() {
	repo := flag.String("repo", "", "GitHub repo as owner/name")
	ref := flag.String("ref", "", "branch, tag or sha (default: default branch)")
	dir := flag.String("dir", "", "subdirectory containing package-lock.json (monorepos)")
	file := flag.String("file", "", "local package-lock.json instead of fetching")
	top := flag.Int("top", 25, "rows to print")
	src := flag.String("src", "", "local source dir to scan for usage (default: shallow-clone -repo, or dir of -file)")
	show := flag.String("show", "", "print sampled usage snippets for this package")
	viz := flag.String("viz", "", "write an interactive HTML graph to this file, e.g. graph.html")
	vizMax := flag.Int("viz-max", 150, "max nodes in the visualization (root + direct deps always included)")
	flag.Parse()

	var data []byte
	var err error
	switch {
	case *file != "":
		data, err = os.ReadFile(*file)
	case *repo != "":
		data, err = fetchFile(*repo, *ref, *dir, "package-lock.json")
	default:
		fmt.Fprintln(os.Stderr, "provide -repo owner/name or -file path")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	var lf lockfile
	if err := json.Unmarshal(data, &lf); err != nil {
		fmt.Fprintln(os.Stderr, "parse error:", err)
		os.Exit(1)
	}
	if len(lf.Packages) == 0 && len(lf.V1Deps) > 0 {
		// lockfileVersion 1 has no root entry; we need package.json to know the direct deps.
		pj, err := loadPackageJSON(*repo, *ref, *dir, *file)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: v1 lockfile needs package.json:", err)
			os.Exit(1)
		}
		convertV1(&lf, pj)
	}
	g, err := buildGraph(&lf)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	edges := 0
	direct := 0
	var list []*Node
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
	fmt.Printf("Graph: %d vertices, %d edges, %d direct deps\n\n", len(g.Nodes), edges, direct)

	// Rank by blast-radius upper bound, then direct fan-in.
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

	fmt.Printf("%-40s %-10s %-7s %-6s %-9s %s\n", "PACKAGE", "VERSION", "DIRECT", "DEPTH", "DEPENDENTS", "TRANSITIVE")
	for i, n := range list {
		if i >= *top {
			break
		}
		fmt.Printf("%-40s %-10s %-7v %-6d %-9d  %d\n",
			trunc(n.Name, 40), trunc(n.Version, 10), n.Direct, n.Depth, len(n.In), n.TransitiveDependents)
	}

	if *viz != "" {
		if err := writeViz(g, *viz, *vizMax); err != nil {
			fmt.Fprintln(os.Stderr, "viz error:", err)
		} else {
			fmt.Printf("\nWrote interactive graph to %s (open it in a browser)\n", *viz)
		}
	}

	// ---- step 2: usage scan ----
	srcDir := *src
	if srcDir == "" && *repo != "" {
		d, cleanup, err := cloneRepo(*repo, *ref)
		if err != nil {
			fmt.Fprintln(os.Stderr, "clone error:", err)
			os.Exit(1)
		}
		defer cleanup()
		srcDir = filepath.Join(d, *dir)
	} else if srcDir == "" && *file != "" {
		srcDir = filepath.Dir(*file)
	}
	if srcDir != "" {
		runScan(g, srcDir, *show)
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
