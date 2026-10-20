package python

// Python-specific usage-scanning pieces: import syntax and pyproject.toml's
// version of packageJSONSearchText's self-match avoidance. Comment-stripping
// by extension (including hash-comment files like .py/.toml) stays in
// core/scan.go since it's generic, not Python-specific logic.

import (
	"atlas/core"
	"regexp"
	"strings"
)

// Matches the module specifier in:
//
//	from a.b import x      from a.b import x as y
//	import a                import a, b.c        import a as b
//
// Group 1 captures the "from X import" module; group 2 captures the
// (possibly comma-separated) module list after a bare "import". Anchored at
// line start (allowing leading whitespace, for imports indented inside
// functions/try-blocks).
var pythonImportRe = regexp.MustCompile(`^\s*(?:from\s+([A-Za-z_][\w.]*)\s+import\b|import\s+([A-Za-z_][\w.]*(?:\s*,\s*[A-Za-z_][\w.]*)*))`)

var pySrcExts = map[string]bool{".py": true}

// pyImportAliases is a small, explicitly non-exhaustive map of the common
// cases where a Python import name doesn't match its PyPI package name —
// a nice-to-have, not a guarantee: anything not listed here just won't
// resolve to its dependency (falls through to "not imported in source"),
// which is an accepted, known gap rather than a silent correctness bug.
var pyImportAliases = map[string]string{
	"yaml":     "pyyaml",
	"pil":      "pillow",
	"cv2":      "opencv-python",
	"sklearn":  "scikit-learn",
	"bs4":      "beautifulsoup4",
	"dotenv":   "python-dotenv",
	"dateutil": "python-dateutil",
	"jwt":      "pyjwt",
}

// pkgFromPythonSpecifier extracts the top-level module name from one
// "import" item — stripping an "as alias" suffix and taking only the first
// dotted segment (os.path -> os). Relative imports (leading '.') are
// rejected, same as npm's leading './'/'/' rejection in pkgFromSpecifier.
func pkgFromPythonSpecifier(item string) (string, bool) {
	item = strings.TrimSpace(item)
	if idx := strings.Index(item, " as "); idx >= 0 {
		item = item[:idx]
	}
	item = strings.TrimSpace(item)
	if item == "" || item[0] == '.' {
		return "", false
	}
	first, _, _ := strings.Cut(item, ".")
	if first == "" {
		return "", false
	}
	return first, true
}

// resolvePythonSpecifier turns one raw import item into the canonical
// dependency name it refers to (if any), applying the alias table and
// PEP 503 normalization before looking it up in pyIndex.
func resolvePythonSpecifier(item string, wanted map[string]bool, pyIndex map[string]string) (string, bool) {
	top, ok := pkgFromPythonSpecifier(item)
	if !ok {
		return "", false
	}
	norm := core.NormalizePyPIName(top)
	if alias, ok := pyImportAliases[norm]; ok {
		norm = alias
	}
	canonical, ok := pyIndex[norm]
	if !ok || !wanted[canonical] {
		return "", false
	}
	return canonical, true
}

// pyprojectSearchText is pyproject.toml's version of packageJSONSearchText's
// self-match-avoidance fix: every direct dependency trivially appears in its
// own pyproject.toml's dependency-listing sections, so those sections'
// *lines* get blanked out (TOML isn't flat JSON, so this works structurally
// via TOMLSection.StartLine rather than via key deletion) before the
// bare-name search runs.
func pyprojectSearchText(data []byte) string {
	sections, err := ParseTOMLSections(data)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")

	isDepSection := func(path []string) bool {
		if pathEquals(path, "tool", "poetry", "dependencies") {
			return true
		}
		if pathEquals(path, "tool", "poetry", "dev-dependencies") {
			return true
		}
		if len(path) == 5 && pathEquals(path[:3], "tool", "poetry", "group") && path[4] == "dependencies" {
			return true
		}
		return false
	}

	for i, s := range sections {
		if !isDepSection(s.Path) {
			continue
		}
		end := len(lines)
		if i+1 < len(sections) {
			end = sections[i+1].StartLine
		}
		for ln := s.StartLine; ln < end && ln < len(lines); ln++ {
			lines[ln] = ""
		}
	}
	return strings.Join(lines, "\n")
}
