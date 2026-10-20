<img width="1280" height="640" alt="Atlas" src="https://github.com/user-attachments/assets/b8b96783-5833-4121-9b6d-f0b747dd3ba7" />


# ATLAS : Dependency Mapper & Multi-Agent Risk Analyzer
**Srijan Dahal, Parth Poudyal, Arun Mainali, Suprit Phuyal** <br>
*Frogtober Fest 2026*  

## Overview
Atlas is a multi-agent dependency risk analyzer. It maps architectural coupling, blast radius, maintainer sustainability, and security vulnerabilities across a project's dependency graph. It supports npm (`package-lock.json`) and Python/Poetry (`poetry.lock`) projects. Ecosystem support is adapter-based, so adding a third package manager means writing a new adapter, not rewriting the pipeline.

## Architecture

Atlas separates deterministic pipeline analysis from AI agent operations and runs specialist agents concurrently. Ecosystem-specific logic lives behind a small `EcosystemAdapter` interface (`core/adapter.go`), so `core` never depends on a particular package manager directly. `core/python` is a self-contained plug-in, not a special case inside `core`.

```
Atlas/
├── agents/        # Multi-agent runtime: Orchestrator + 4 specialist agents + Action agent
├── core/          # Deterministic pipeline: graph, scan, health, scoring, report, errors (npm-native)
│   └── python/    # Python/Poetry ecosystem plug-in, implementing core's EcosystemAdapter
├── examples/      # Hand-written teaching/test fixtures (npm and Python)
└── main.go        # CLI & interactive REPL entrypoint
```

<img width="861" height="983" alt="image" src="https://github.com/user-attachments/assets/a9352879-d68e-49f7-a81d-bb7068544252" />


## Parallel Multi-Agent Execution

1. **Deterministic Foundation (`core/`)**:
   - Detects the ecosystem from the lockfile (`package-lock.json` → npm, `poetry.lock` → Python/Poetry, trying both when fetching from a `-repo`), parses it, computes transitive reachability (blast radius), and builds an interactive visualization.
   - Statically scans source code (JS/TS imports, or Python `import`/`from` statements) to categorize dependencies into `runtime`, `test-only`, `config-referenced`, `build-tooling`, and `unused-candidate`.
   - Fetches registry metadata (npm registry or PyPI), commit activity, bus factor, and OSV.dev vulnerability advisories.

2. **Parallel Specialist Agents (`agents/`)**:
   - High-risk candidate dependencies are analyzed simultaneously across 4 specialized agents using bounded concurrency:
     - **Coupling Agent**: Classifies whether a dependency is a surface utility or load-bearing infrastructure based on import density, directory span, and architectural domain tags.
     - **Vulnerability Agent**: Re-weights abstract OSV advisory severity against concrete project usage context (e.g. downgrades test-only advisories, prioritizes production-facing ones).
     - **Maintenance Agent**: Evaluates bus factor alongside contributor count and weekly downloads to detect single-point-of-failure abandonment risks.
     - **Remediation Agent**: Synthesizes actionable version upgrades, alternative packages, and mitigation plans.

3. **Concurrent Downstream Action Agent**:
   - **Concurrent Duplicate Check**: Queries GitHub search API concurrently across all candidate issue drafts using fingerprint HTML comments.
   - **Rate-Limited Concurrent Dispatch**: POSTs confirmed issues to GitHub with worker pool pacing to avoid GitHub API abuse rate limits.

## Quick Start

```bash
# Run multi-agent interactive REPL mode (requires Ollama)
go run .

# Run one-shot CLI analysis on a GitHub repository (tries package-lock.json, then poetry.lock)
go run . -repo owner/repo

# Run one-shot analysis on a local npm lockfile
go run . -file ./examples/package-lock.json

# Run one-shot analysis on a local Poetry lockfile (needs a sibling pyproject.toml)
go run . -file ./examples/python/poetry.lock

# Run unit tests
go test -v ./...
```

## Startup Validation & Errors

Atlas checks what the requested operation needs *before* doing any work, and reports every failure it can establish in one go. Checks that do not depend on each other run independently, so one failure never hides another. Reporting order is fixed:

1. **Configuration**: `GITHUB_TOKEN` present (interactive mode only), a model name, a well-formed `OLLAMA_HOST`.
2. **GitHub**: is `GITHUB_TOKEN` accepted by GitHub? One call to `GET /rate_limit`, which needs no scope and does not count against the rate limit. This establishes token *validity* only; access to a particular repository is determined when it is fetched.
3. **Ollama**: reachable? Only if it answered is the model looked for (so "model not installed" is never reported for an unreachable server).

| Mode | Token | Ollama |
|------|-------|--------|
| `go run .` (interactive) | required; validated | required; model checked |
| `go run . -repo owner/name` | optional; validated if set (a rejected token fails public repos too) | not used |
| `go run . -file ...` | validated only if set and `-health` is on | not used |

If any blocking prerequisite fails, nothing else runs. A token that cannot be verified because GitHub is rate-limiting or down is a warning, not a blocker.

Errors share one shape: what failed, what Atlas was doing, **Known** (facts proven by a response or syscall), **Not verified** (hypotheses, always labelled), **Next** (concrete steps), and redacted technical **Detail**. Credentials are removed from every message, including text echoed back by `git` or GitHub.
