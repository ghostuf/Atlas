package core

// EcosystemAdapter is the seam every ecosystem (npm, Python/poetry, ...)
// plugs into. core itself only ever knows this interface — never a
// concrete non-npm implementation — so a subpackage like atlas/core/python
// can import core for its types (Node, Health, Graph, ...) without core
// importing it back, which Go's compiler would otherwise reject as an
// import cycle. The caller (main.go, agents/orchestrator.go) is the one
// place that knows about every ecosystem package and picks the right
// adapter value based on the detected Ecosystem.
type EcosystemAdapter interface {
	RegistryAdapter
	ScanAdapter
}

// RegistryAdapter fetches health signals for one dependency from whichever
// package registry this ecosystem uses (npm registry, PyPI, ...).
type RegistryAdapter interface {
	FetchHealth(n *Node, advisories []Advisory) *Health
}

// ScanAdapter supplies the ecosystem-specific pieces of usage scanning:
// which files count as source, how to pull import/dependency candidates out
// of a source line, how to resolve a raw candidate down to one of the
// project's tracked dependencies, and how to clean a project manifest file
// (package.json, pyproject.toml, ...) so a dependency doesn't trivially
// "self-match" its own name in its own manifest.
type ScanAdapter interface {
	SrcExts() map[string]bool
	ImportCandidates(line string) []string
	NewResolver(wanted map[string]bool) func(candidate string) (string, bool)
	ManifestSearchText(fileName string, data []byte) (cleaned string, ok bool)
}

// NpmAdapter implements EcosystemAdapter for npm/JS projects. All of npm's
// ecosystem-specific logic stays native to core (it never needs to cross a
// package boundary) — only non-npm ecosystems live in their own subpackage.
type NpmAdapter struct{}

func (NpmAdapter) FetchHealth(n *Node, advisories []Advisory) *Health {
	return fetchHealthNpm(n, advisories)
}

func (NpmAdapter) SrcExts() map[string]bool { return srcExts }

func (NpmAdapter) ImportCandidates(line string) []string {
	var out []string
	for _, m := range importRe.FindAllStringSubmatch(line, -1) {
		out = append(out, m[1])
	}
	return out
}

func (NpmAdapter) NewResolver(wanted map[string]bool) func(string) (string, bool) {
	return func(spec string) (string, bool) {
		pkg, ok := pkgFromSpecifier(spec)
		return pkg, ok && wanted[pkg]
	}
}

func (NpmAdapter) ManifestSearchText(fileName string, data []byte) (string, bool) {
	if fileName != "package.json" {
		return "", false
	}
	return packageJSONSearchText(data), true
}
