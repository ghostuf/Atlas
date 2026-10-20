package core

import (
	"encoding/json"
	"path/filepath"
)

// LockfileVersion 1 (npm 5/6) stores a nested tree under "dependencies":
//
//	"a": { "version": "1.0.0", "requires": {"b": "^1.0.0"}, "dependencies": { nested installs of a } }
//
// There is no "packages" map and no root entry. We convert it into the v2/v3 shape
// ("node_modules/a/node_modules/b" keys) so the rest of the code needs no changes.
type V1Entry struct {
	Version      string             `json:"version"`
	Dev          bool               `json:"dev"`
	Requires     map[string]string  `json:"requires"`
	Dependencies map[string]V1Entry `json:"dependencies"`
}

type PackageJSON struct {
	Name                 string            `json:"name"`
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
}

// LoadPackageJSON loads package.json from either a local file or GitHub repo.
func LoadPackageJSON(repo, ref, dir, file string) (*PackageJSON, error) {
	var data []byte
	var err error
	if file != "" {
		data, err = ReadLocalFile(filepath.Join(filepath.Dir(file), "package.json"), "package.json")
	} else {
		data, err = FetchFile(repo, ref, dir, "package.json")
	}
	if err != nil {
		return nil, err
	}
	var pj PackageJSON
	if err := json.Unmarshal(data, &pj); err != nil {
		return nil, LockfileParseError(err, "package.json")
	}
	return &pj, nil
}

// ConvertV1 converts a v1 lockfile into v2/v3 format.
func ConvertV1(lf *Lockfile, pj *PackageJSON) {
	lf.Packages = map[string]LockEntry{}
	lf.Packages[""] = LockEntry{
		Dependencies:         pj.Dependencies,
		DevDependencies:      pj.DevDependencies,
		OptionalDependencies: pj.OptionalDependencies,
	}
	if lf.Name == "" {
		lf.Name = pj.Name
	}
	flattenV1("", lf.V1Deps, lf.Packages)
}

func flattenV1(prefix string, deps map[string]V1Entry, out map[string]LockEntry) {
	for name, e := range deps {
		path := prefix + "node_modules/" + name
		out[path] = LockEntry{Version: e.Version, Dev: e.Dev, Dependencies: e.Requires}
		flattenV1(path+"/", e.Dependencies, out)
	}
}
