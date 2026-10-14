package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// lockfileVersion 1 (npm 5/6) stores a nested tree under "dependencies":
//
//	"a": { "version": "1.0.0", "requires": {"b": "^1.0.0"}, "dependencies": { nested installs of a } }
//
// There is no "packages" map and no root entry. We convert it into the v2/v3 shape
// ("node_modules/a/node_modules/b" keys) so the rest of the code needs no changes.
type v1Entry struct {
	Version      string             `json:"version"`
	Dev          bool               `json:"dev"`
	Requires     map[string]string  `json:"requires"`
	Dependencies map[string]v1Entry `json:"dependencies"`
}

type packageJSON struct {
	Name                 string            `json:"name"`
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
}

func loadPackageJSON(repo, ref, dir, file string) (*packageJSON, error) {
	var data []byte
	var err error
	if file != "" {
		data, err = os.ReadFile(filepath.Join(filepath.Dir(file), "package.json"))
	} else {
		data, err = fetchFile(repo, ref, dir, "package.json")
	}
	if err != nil {
		return nil, err
	}
	var pj packageJSON
	if err := json.Unmarshal(data, &pj); err != nil {
		return nil, fmt.Errorf("parse package.json: %w", err)
	}
	return &pj, nil
}

func convertV1(lf *lockfile, pj *packageJSON) {
	lf.Packages = map[string]lockEntry{}
	lf.Packages[""] = lockEntry{
		Dependencies:         pj.Dependencies,
		DevDependencies:      pj.DevDependencies,
		OptionalDependencies: pj.OptionalDependencies,
	}
	if lf.Name == "" {
		lf.Name = pj.Name
	}
	flattenV1("", lf.V1Deps, lf.Packages)
}

func flattenV1(prefix string, deps map[string]v1Entry, out map[string]lockEntry) {
	for name, e := range deps {
		path := prefix + "node_modules/" + name
		out[path] = lockEntry{Version: e.Version, Dev: e.Dev, Dependencies: e.Requires}
		flattenV1(path+"/", e.Dependencies, out)
	}
}
