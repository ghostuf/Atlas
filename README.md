# ATLAS : Dependency Mapper & Multi-Agent Risk Analyzer
**Srijan Dahal, Parth Poudyal, Arun Mainali, Suprit Phuyal** <br>
*Frogtober Fest 2026*  

## Overview
Atlas is a multi-agent dependency risk analyzer that maps architectural coupling, blast radius, maintainer sustainability, and security vulnerabilities across chained npm dependencies.

## Architecture

Atlas decouples deterministic pipeline analysis from AI agent operations, running specialist agents concurrently:

```
Atlas/
├── agents/                     # Multi-Agent Parallel Runtime
│   ├── types.go                # Agent interface, context, and structured assessment schemas
│   ├── pool.go                 # Concurrency limiter, semaphore & parallel worker runner
│   ├── orchestrator.go         # Generalist Orchestrator Agent (pipeline & parallel coordination)
│   ├── coupling.go             # Architectural Coupling Agent (surface vs load-bearing depth)
│   ├── vulnerability.go        # Vulnerability Risk Agent (contextual exploitability)
│   ├── maintenance.go          # Maintainer & Abandonment Agent (bus-factor & sustainability)
│   ├── remediation.go          # Remediation Agent (concrete migration & mitigation plans)
│   └── action.go               # Downstream Action Agent (concurrent dedup & rate-limited issue dispatch)
├── core/                       # Deterministic Signals & Data Pipeline
│   ├── graph.go                # Dependency graph G=(V,E), BFS metrics, blast radius
│   ├── scan.go                 # Static AST/regex usage scanning, prod vs test classification
│   ├── health.go               # npm registry, GitHub API, OSV.dev advisories
│   ├── score.go                # Deterministic repository health scoring
│   ├── report.go               # Comprehensive HTML report generator
│   ├── viz.go                  # Interactive D3/HTML graph visualization
│   ├── gauge.go                # SVG score gauge generator
│   ├── fetch.go                # GitHub file fetching & shallow cloning
│   ├── lockv1.go               # npm lockfile v1 conversion
│   ├── ollama.go               # Ollama client with tools & schema-constrained JSON
│   ├── spinner.go              # CLI terminal animated spinners
│   └── env.go                  # .env file loader
└── main.go                     # CLI & Interactive REPL entrypoint
```

## Parallel Multi-Agent Execution

1. **Deterministic Foundation (`core/`)**:
   - Parses `package-lock.json`, computes transitive reachability (blast radius), and builds an interactive visualization.
   - Statically scans source code to categorize dependencies into `runtime`, `test-only`, `config-referenced`, `build-tooling`, and `unused-candidate`.
   - Fetches registry metadata, commit activity, bus factor, and OSV.dev vulnerability advisories.

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

# Run one-shot CLI analysis on a GitHub repository
go run . -repo owner/repo

# Run one-shot analysis on a local lockfile
go run . -file ./package-lock.json

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
