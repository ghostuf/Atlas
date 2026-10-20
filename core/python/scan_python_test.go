package python

import (
	"atlas/core"
	"testing"
)

func TestPkgFromPythonSpecifier(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"requests", "requests", true},
		{"os.path", "os", true},
		{"numpy as np", "numpy", true},
		{".relative", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := pkgFromPythonSpecifier(c.in)
		if got != c.want || ok != c.wantOK {
			t.Errorf("pkgFromPythonSpecifier(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

func TestResolvePythonSpecifier_AliasAndNormalization(t *testing.T) {
	wanted := map[string]bool{"PyYAML": true, "requests": true}
	pyIndex := map[string]string{
		"pyyaml":   "PyYAML",
		"requests": "requests",
	}

	// "import yaml" should resolve to the PyYAML dependency via the alias table.
	got, ok := resolvePythonSpecifier("yaml", wanted, pyIndex)
	if !ok || got != "PyYAML" {
		t.Errorf("resolvePythonSpecifier(yaml) = (%q, %v), want (PyYAML, true)", got, ok)
	}

	// direct match, no alias needed
	got, ok = resolvePythonSpecifier("requests", wanted, pyIndex)
	if !ok || got != "requests" {
		t.Errorf("resolvePythonSpecifier(requests) = (%q, %v), want (requests, true)", got, ok)
	}

	// not a dependency at all
	_, ok = resolvePythonSpecifier("os", wanted, pyIndex)
	if ok {
		t.Errorf("resolvePythonSpecifier(os) should not resolve (not a tracked dependency)")
	}
}

func TestPythonImportRe(t *testing.T) {
	cases := []struct {
		line       string
		wantFrom   string
		wantImport string
	}{
		{"import requests", "", "requests"},
		{"from flask import Flask", "flask", ""},
		{"    import os, sys", "", "os, sys"},
		{"from . import views", "", ""}, // relative from-import: our regex requires a bare identifier start, "." won't match group
		{"not an import line", "", ""},
	}
	for _, c := range cases {
		m := pythonImportRe.FindStringSubmatch(c.line)
		var from, imp string
		if m != nil {
			from, imp = m[1], m[2]
		}
		if from != c.wantFrom || imp != c.wantImport {
			t.Errorf("pythonImportRe.FindStringSubmatch(%q) = (from=%q, import=%q), want (from=%q, import=%q)",
				c.line, from, imp, c.wantFrom, c.wantImport)
		}
	}
}

func TestPyprojectSearchText_BlanksDependencySections(t *testing.T) {
	data := []byte(`[tool.poetry]
name = "myproject"

[tool.poetry.dependencies]
python = "^3.11"
requests = "^2.31"

[tool.poetry.group.dev.dependencies]
pytest = "^7.4"

[tool.poetry.urls]
"Homepage" = "https://example.com/myproject"
`)
	cleaned := pyprojectSearchText(data)
	if core.ContainsWholeToken(cleaned, "requests") {
		t.Errorf("requests should be blanked out of the dependencies section, got:\n%s", cleaned)
	}
	if core.ContainsWholeToken(cleaned, "pytest") {
		t.Errorf("pytest should be blanked out of the dev-dependencies section, got:\n%s", cleaned)
	}
	// unrelated sections must survive untouched
	if !core.ContainsWholeToken(cleaned, "myproject") {
		t.Errorf("unrelated content should survive, got:\n%s", cleaned)
	}
}
