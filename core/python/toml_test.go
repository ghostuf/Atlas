package python

import "testing"

func findSection(t *testing.T, sections []TOMLSection, path ...string) TOMLSection {
	t.Helper()
	for _, s := range sections {
		if len(s.Path) != len(path) {
			continue
		}
		match := true
		for i := range path {
			if s.Path[i] != path[i] {
				match = false
				break
			}
		}
		if match {
			return s
		}
	}
	t.Fatalf("section %v not found among %d sections", path, len(sections))
	return TOMLSection{}
}

func pairValue(t *testing.T, s TOMLSection, key string) string {
	t.Helper()
	for _, p := range s.Pairs {
		if p.Key == key {
			return p.Raw
		}
	}
	t.Fatalf("key %q not found in section %v", key, s.Path)
	return ""
}

func TestParseTOMLSections_MultiLineArrayAndInlineTables(t *testing.T) {
	data := []byte(`[[package]]
name = "requests"
version = "2.31.0"
category = "main"
optional = false

[package.dependencies]
certifi = ">=2017.4.17"
urllib3 = {version = ">=1.21.1,<3", optional = true}

[package.extras]
socks = ["PySocks (>=1.5.6,!=1.5.7)"]

[[package]]
name = "pytest"
version = "7.4.0"
category = "dev"

[metadata]
lock-version = "2.0"
`)
	sections, err := ParseTOMLSections(data)
	if err != nil {
		t.Fatalf("ParseTOMLSections: %v", err)
	}

	pkg1 := findSection(t, sections, "package")
	if v, _ := TOMLString(pairValue(t, pkg1, "name")); v != "requests" {
		t.Errorf("name = %q, want requests", v)
	}
	if v, _ := TOMLBool(pairValue(t, pkg1, "optional")); v != false {
		t.Errorf("optional = %v, want false", v)
	}

	deps := findSection(t, sections, "package", "dependencies")
	if len(deps.Pairs) != 2 {
		t.Fatalf("expected 2 dependency pairs, got %d: %+v", len(deps.Pairs), deps.Pairs)
	}
	keys := map[string]bool{}
	for _, p := range deps.Pairs {
		keys[p.Key] = true
	}
	if !keys["certifi"] || !keys["urllib3"] {
		t.Errorf("expected certifi and urllib3 keys, got %+v", deps.Pairs)
	}

	// second [[package]] must be a distinct entry, not merged with the first
	count := 0
	for _, s := range sections {
		if len(s.Path) == 1 && s.Path[0] == "package" && s.IsArray {
			count++
		}
	}
	if count != 2 {
		t.Errorf("expected 2 [[package]] sections, got %d", count)
	}
}

func TestParseTOMLSections_MultiLineArrayOfInlineTables(t *testing.T) {
	data := []byte(`[[package]]
name = "chalk"
version = "4.1.2"
files = [
    {file = "chalk-4.1.2-py3-none-any.whl", hash = "sha256:aaaa"},
    {file = "chalk-4.1.2.tar.gz", hash = "sha256:bbbb"},
]

[[package]]
name = "next"
version = "1.0.0"
`)
	sections, err := ParseTOMLSections(data)
	if err != nil {
		t.Fatalf("ParseTOMLSections: %v", err)
	}
	// the multi-line "files" array must not be mistaken for new sections —
	// there should be exactly 2 [[package]] blocks, and "next" must still be
	// parsed correctly after it.
	count := 0
	for _, s := range sections {
		if len(s.Path) == 1 && s.Path[0] == "package" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("expected 2 [[package]] sections, got %d: %+v", count, sections)
	}
	pkg2 := sections[len(sections)-1]
	if v, _ := TOMLString(pairValue(t, pkg2, "name")); v != "next" {
		t.Errorf("second package name = %q, want next", v)
	}
}

func TestParseTOMLSections_CategoryVsGroups(t *testing.T) {
	// lock-version 1.x style
	data1 := []byte(`[[package]]
name = "old-style"
category = "dev"
`)
	s1, err := ParseTOMLSections(data1)
	if err != nil {
		t.Fatal(err)
	}
	cat, ok := TOMLString(pairValue(t, findSection(t, s1, "package"), "category"))
	if !ok || cat != "dev" {
		t.Errorf("category = %q, %v", cat, ok)
	}

	// lock-version 2.0 style
	data2 := []byte(`[[package]]
name = "new-style"
groups = ["main", "dev"]
`)
	s2, err := ParseTOMLSections(data2)
	if err != nil {
		t.Fatal(err)
	}
	groups, ok := TOMLStringArray(pairValue(t, findSection(t, s2, "package"), "groups"))
	if !ok || len(groups) != 2 || groups[0] != "main" || groups[1] != "dev" {
		t.Errorf("groups = %+v, %v", groups, ok)
	}
}

func TestParseTOMLSections_HashInsideQuotedStringNotStripped(t *testing.T) {
	data := []byte(`[[package]]
name = "requests"
description = "HTTP for humans #1 choice" # trailing real comment
`)
	sections, err := ParseTOMLSections(data)
	if err != nil {
		t.Fatal(err)
	}
	desc, ok := TOMLString(pairValue(t, findSection(t, sections, "package"), "description"))
	if !ok {
		t.Fatalf("description not parsed as string")
	}
	if desc != "HTTP for humans #1 choice" {
		t.Errorf("description = %q, want %q", desc, "HTTP for humans #1 choice")
	}
}

func TestParseTOMLSections_CRLF(t *testing.T) {
	data := []byte("[[package]]\r\nname = \"requests\"\r\nversion = \"2.31.0\"\r\n")
	sections, err := ParseTOMLSections(data)
	if err != nil {
		t.Fatal(err)
	}
	pkg := findSection(t, sections, "package")
	if v, _ := TOMLString(pairValue(t, pkg, "name")); v != "requests" {
		t.Errorf("name = %q, want requests (CRLF not handled)", v)
	}
}

func TestParseTOMLSections_UnrecognizedSectionIgnoredNotError(t *testing.T) {
	data := []byte(`[[package]]
name = "requests"

[metadata.files]
requests = [
    {file = "a.whl", hash = "sha256:x"},
]

[some.totally.unknown.future.section]
whatever = true
`)
	sections, err := ParseTOMLSections(data)
	if err != nil {
		t.Fatalf("unrecognized sections should not error: %v", err)
	}
	// must still find the real package section despite unknown sections around it
	findSection(t, sections, "package")
	findSection(t, sections, "metadata", "files")
	findSection(t, sections, "some", "totally", "unknown", "future", "section")
}

func TestParsePyProjectDependenciesSection(t *testing.T) {
	data := []byte(`[tool.poetry]
name = "myproject"

[tool.poetry.dependencies]
python = "^3.11"
requests = "^2.31"

[tool.poetry.group.dev.dependencies]
pytest = "^7.4"
`)
	sections, err := ParseTOMLSections(data)
	if err != nil {
		t.Fatal(err)
	}
	main := findSection(t, sections, "tool", "poetry", "dependencies")
	dev := findSection(t, sections, "tool", "poetry", "group", "dev", "dependencies")
	if len(main.Pairs) != 2 {
		t.Errorf("expected 2 main deps (python+requests), got %+v", main.Pairs)
	}
	if len(dev.Pairs) != 1 || dev.Pairs[0].Key != "pytest" {
		t.Errorf("expected pytest dev dep, got %+v", dev.Pairs)
	}
}
