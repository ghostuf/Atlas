package python

import (
	"atlas/core"
	"strings"
)

// Adapter implements core.EcosystemAdapter for Python (Poetry) projects.
// The caller (main.go, agents/orchestrator.go) selects this once the
// detected ecosystem is EcosystemPoetry and passes it into core.RunHealth
// and core.RunScan — core itself never imports this package.
type Adapter struct{}

func (Adapter) FetchHealth(n *core.Node, advisories []core.Advisory) *core.Health {
	return fetchHealthPyPI(n, advisories)
}

func (Adapter) SrcExts() map[string]bool { return pySrcExts }

func (Adapter) ImportCandidates(line string) []string {
	var out []string
	for _, m := range pythonImportRe.FindAllStringSubmatch(line, -1) {
		switch {
		case m[1] != "":
			out = append(out, m[1])
		case m[2] != "":
			out = append(out, strings.Split(m[2], ",")...)
		}
	}
	return out
}

func (Adapter) NewResolver(wanted map[string]bool) func(string) (string, bool) {
	index := map[string]string{}
	for name := range wanted {
		index[core.NormalizePyPIName(name)] = name
	}
	return func(item string) (string, bool) {
		return resolvePythonSpecifier(item, wanted, index)
	}
}

func (Adapter) ManifestSearchText(fileName string, data []byte) (string, bool) {
	if fileName != "pyproject.toml" {
		return "", false
	}
	return pyprojectSearchText(data), true
}
