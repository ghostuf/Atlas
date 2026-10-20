package python

// PyPI registry health signals — the Poetry/Python twin of the npm-registry
// functions in health.go. Only the registry lookup itself differs; GitHub
// repo/contributor/bus-factor signals are shared via fillGithubSignals.

import (
	"atlas/core"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type pypiJSON struct {
	Info struct {
		HomePage    string            `json:"home_page"`
		ProjectURLs map[string]string `json:"project_urls"`
	} `json:"info"`
	Releases map[string][]struct {
		UploadTime string `json:"upload_time_iso_8601"`
	} `json:"releases"`
}

func fetchPypiJSON(name string) (*pypiJSON, error) {
	u := "https://pypi.org/pypi/" + url.PathEscape(name) + "/json"
	body, status, err := core.HttpGet(u, nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("pypi registry: %s", http.StatusText(status))
	}
	var pj pypiJSON
	if err := json.Unmarshal(body, &pj); err != nil {
		return nil, fmt.Errorf("parse pypi json: %w", err)
	}
	return &pj, nil
}

// pypiPublishStats mirrors npmPublishStats's cadence logic, over PyPI's
// releases map (version -> uploaded files) instead of npm's flat time map.
func pypiPublishStats(pj *pypiJSON) (last time.Time, cadenceDays float64) {
	var versions []time.Time
	for _, files := range pj.Releases {
		var earliest time.Time
		for _, f := range files {
			t, err := time.Parse(time.RFC3339, f.UploadTime)
			if err != nil {
				continue
			}
			if earliest.IsZero() || t.Before(earliest) {
				earliest = t
			}
		}
		if !earliest.IsZero() {
			versions = append(versions, earliest)
		}
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].Before(versions[j]) })

	if len(versions) > 0 {
		last = versions[len(versions)-1]
	}

	const window = 10
	if n := len(versions); n >= 2 {
		start := n - window
		if start < 0 {
			start = 0
		}
		recent := versions[start:]
		span := recent[len(recent)-1].Sub(recent[0])
		cadenceDays = span.Hours() / 24 / float64(len(recent)-1)
	}
	return
}

// wellKnownGithubURLKeys are checked first, in order, before falling back to
// scanning every value in project_urls — PyPI's project_urls is an
// arbitrary, inconsistently-cased string-keyed map (no fixed schema the way
// npm's `repository` field has one).
var wellKnownGithubURLKeys = []string{"source", "repository", "github", "code", "source code", "homepage"}

// resolvePypiGithubRepo finds a github.com URL in a PyPI packument's
// project_urls (checking well-known keys first, case-insensitively) or
// info.home_page, and extracts owner/repo from it.
func resolvePypiGithubRepo(pj *pypiJSON) (owner, repo string, ok bool) {
	lowerKeyed := make(map[string]string, len(pj.Info.ProjectURLs))
	for k, v := range pj.Info.ProjectURLs {
		lowerKeyed[strings.ToLower(k)] = v
	}
	for _, key := range wellKnownGithubURLKeys {
		if owner, repo, ok := core.ParseGithubURL(lowerKeyed[key]); ok {
			return owner, repo, true
		}
	}
	for _, v := range pj.Info.ProjectURLs {
		if owner, repo, ok := core.ParseGithubURL(v); ok {
			return owner, repo, true
		}
	}
	return core.ParseGithubURL(pj.Info.HomePage)
}

type pypiStatsResponse struct {
	Data struct {
		LastWeek int `json:"last_week"`
	} `json:"data"`
}

// fetchPypiDownloads uses pypistats.org's unofficial public API — unlike
// npm's first-party downloads endpoint, this is a third-party service, so
// callers should treat any failure here as a soft Note, never Material.
// Unlike PyPI's own JSON API, pypistats.org requires the PEP 503-normalized
// (lowercase) package name — "PyYAML" 404s, "pyyaml" doesn't.
func fetchPypiDownloads(name string) (int, error) {
	u := "https://pypistats.org/api/packages/" + url.PathEscape(core.NormalizePyPIName(name)) + "/recent"
	body, status, err := core.HttpGet(u, nil)
	if err != nil {
		return 0, err
	}
	if status != 200 {
		return 0, fmt.Errorf("pypistats.org: %s", http.StatusText(status))
	}
	var r pypiStatsResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return 0, fmt.Errorf("parse pypistats response: %w", err)
	}
	return r.Data.LastWeek, nil
}

func fetchHealthPyPI(n *core.Node, advisories []core.Advisory) *core.Health {
	h := &core.Health{Package: n.Name, Version: n.Version, Dev: n.Dev, Advisories: advisories}

	pj, err := fetchPypiJSON(n.Name)
	if err != nil {
		h.Notes = append(h.Notes, err.Error())
		h.Material = true
		return h
	}
	h.LastPublish, h.PublishCadenceDays = pypiPublishStats(pj)
	// MaintainerCount has no PyPI equivalent exposed by this API, and is
	// already a dead/unused signal for npm too (computed but never
	// displayed) — left at zero.

	if downloads, err := fetchPypiDownloads(n.Name); err != nil {
		h.Notes = append(h.Notes, "pypi downloads: "+err.Error())
	} else {
		h.WeeklyDownloads = downloads
	}

	owner, repo, ok := resolvePypiGithubRepo(pj)
	if !ok {
		h.Notes = append(h.Notes, "no GitHub repository found")
		return h
	}
	core.FillGithubSignals(h, owner, repo)
	return h
}
