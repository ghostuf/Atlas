package core

// Everything that varies by package-manager ecosystem is centralized here —
// the OSV.dev ecosystem string, registry display name, remediation commands,
// audit tooling hint, and registry link — so adding a third ecosystem later
// never requires hunting down scattered "npm"-vs-"poetry" branches in
// health.go/scan.go/agents/*.go.

import (
	"fmt"
	"path/filepath"
	"strings"
)

type Ecosystem string

const (
	EcosystemNPM    Ecosystem = "npm"
	EcosystemPoetry Ecosystem = "poetry"
)

// DetectEcosystem identifies the ecosystem from a lockfile's base name.
// Unrecognized names fall back to npm, preserving today's behavior.
func DetectEcosystem(lockfileName string) Ecosystem {
	if filepath.Base(lockfileName) == "poetry.lock" {
		return EcosystemPoetry
	}
	return EcosystemNPM
}

// OSVEcosystem is the exact ecosystem string OSV.dev's API expects.
func (e Ecosystem) OSVEcosystem() string {
	if e == EcosystemPoetry {
		return "PyPI"
	}
	return "npm"
}

// RegistryName is the package registry's display name, for status/spinner text.
func (e Ecosystem) RegistryName() string {
	if e == EcosystemPoetry {
		return "PyPI"
	}
	return "npm"
}

// DisplayName is how this ecosystem should be referred to in agent prompts.
func (e Ecosystem) DisplayName() string {
	if e == EcosystemPoetry {
		return "Python (poetry)"
	}
	return "npm"
}

// RemoveCommand is the idiomatic command to drop a dependency.
func (e Ecosystem) RemoveCommand(pkg string) string {
	if e == EcosystemPoetry {
		return fmt.Sprintf("poetry remove %s", pkg)
	}
	return fmt.Sprintf("npm uninstall %s", pkg)
}

// InstallCommand is the idiomatic command to install/upgrade to a specific version.
func (e Ecosystem) InstallCommand(pkg, version string) string {
	if e == EcosystemPoetry {
		return fmt.Sprintf("poetry add %s@%s", pkg, version)
	}
	return fmt.Sprintf("npm install %s@%s", pkg, version)
}

// AuditHint names the tool to cross-check advisories with. PyPI has no
// first-party audit command (unlike npm's built-in `npm audit`), so this
// names a well-known third-party tool instead of inventing one.
func (e Ecosystem) AuditHint() string {
	if e == EcosystemPoetry {
		return "`pip-audit` (third-party) or re-check https://osv.dev directly"
	}
	return "`npm audit`"
}

// ManifestName is the file where direct dependencies are declared.
func (e Ecosystem) ManifestName() string {
	if e == EcosystemPoetry {
		return "pyproject.toml"
	}
	return "package.json"
}

// RegistryLink is a direct link to the package's registry page.
func (e Ecosystem) RegistryLink(pkg string) string {
	if e == EcosystemPoetry {
		return "https://pypi.org/project/" + pkg + "/"
	}
	return "https://www.npmjs.com/package/" + pkg
}

// NormalizePyPIName canonicalizes a PyPI/poetry package name per PEP 503:
// lowercase, with runs of '-', '_', '.' collapsed to a single '-'. Needed
// because poetry.lock/pyproject.toml don't always spell a dependency name in
// canonical form at every reference site, and because Python's
// import-name/package-name mismatch means usage scanning must compare
// normalized names, not raw strings.
func NormalizePyPIName(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		if r == '-' || r == '_' || r == '.' {
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
			continue
		}
		b.WriteRune(r)
		lastDash = false
	}
	return strings.TrimSuffix(b.String(), "-")
}
