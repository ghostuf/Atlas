# Contributing to Atlas

Atlas answers questions that package managers cannot: *How deeply is a codebase coupled to its dependencies? What is the real blast radius if a package fails? Which vulnerabilities are truly reachable in production, and what is the maintainer health and bus factor?*

Whether you want to implement a new ecosystem adapter (like Cargo or Go modules), introduce a new specialist agent, optimize the deterministic static scanner, refine the interactive D3 visualization, or improve documentation and test coverage — this guide explains how to get started.

---

## Table of Contents

1. [Architectural Philosophy](#architectural-philosophy)
2. [Where You Can Contribute](#where-you-can-contribute)
   - [A. Ecosystem Adapters](#a-ecosystem-adapters-coreadaptergo-coreecosystemgo-corelang)
   - [B. Multi-Agent Reasoning & Specialists](#b-multi-agent-reasoning--specialists-agents)
   - [C. Core Deterministic Data Engine](#c-core-deterministic-data-engine-core)
   - [D. Visualizations & Interactive Reports](#d-visualizations--interactive-reports-corevizgo-corereportgo)
   - [E. CLI, REPL & Developer Experience](#e-cli-repl--developer-experience-maingo-agentsorchestratorgo)
   - [F. Testing, Mocking & Fixtures](#f-testing-mocking--fixtures)
3. [Development Environment Setup](#development-environment-setup)
4. [Contribution Workflow & Git Protocol](#contribution-workflow--git-protocol)
5. [Pull Request (PR) Structure & Schematics](#pull-request-pr-structure--schematics)
   - [PR Title Convention](#pr-title-convention)
   - [The PR Description Schematic](#the-pr-description-schematic)
   - [PR Review & Merging Lifecycle](#pr-review--merging-lifecycle)
6. [Coding Standards & Architectural Tenets](#coding-standards--architectural-tenets)
7. [Getting Help & Community](#getting-help--community)

---

## Architectural Philosophy

Before writing code, please keep our core tenets in mind:

1. **Zero External Go Dependencies (Standard Library Purity):**  
   Atlas is built exclusively using Go's standard library (`net/http`, `encoding/json`, `sync`, `regexp`, `runtime/pprof`, etc.). Do **not** import external Go packages without prior RFC discussion and maintainer approval.
2. **Local-First & Privacy-First:**  
   Atlas does not send user source code, dependency manifests, or tokens to any external AI SaaS. All agent reasoning runs locally via [Ollama](https://ollama.com).
3. **Deterministic Fallbacks Are Mandatory:**  
   Every AI specialist agent must feature a deterministic heuristic fallback. If Ollama is offline, unreachable, or returns invalid JSON, Atlas must complete its analysis gracefully without crashing.

---

## Where You Can Contribute

Atlas is designed with modular seams so contributors can work independently on distinct layers. Below are the key areas where contributions are most valuable:

```
Atlas Architecture
┌────────────────────────────────────────────────────────────────────────┐
│                               main.go                                  │
│                 (CLI flags, REPL entry, Ecosystem Router)              │
└──────────────────┬─────────────────────────────────┬───────────────────┘
                   │                                 │
                   ▼                                 ▼
       ┌───────────────────────┐         ┌───────────────────────┐
       │     agents/ Layer     │         │      core/ Layer      │
       │ Multi-Agent Reasoning │         │  Deterministic Engine │
       │  • Coupling Agent     │         │  • Graph Builder      │
       │  • Vulnerability Agt  │         │  • Static Scanner     │
       │  • Maintenance Agt    │         │  • Health/OSV Client  │
       │  • Remediation Agt    │         │  • 0-100 Score        │
       │  • Issue Action Agt   │         │  • D3 Viz & Reports   │
       │  • Concurrency Pool   │         │  • Preflight & Errors │
       └───────────────────────┘         └───────────┬───────────┘
                                                     │
                                                     ▼
                                         ┌───────────────────────┐
                                         │   Ecosystem Adapters  │
                                         │  (core.EcosystemAdapter)
                                         │  • npm (native core)  │
                                         │  • python (core/python)
                                         │  • [Your Ecosystem!]  │
                                         └───────────────────────┘
```

### A. Ecosystem Adapters (`core/adapter.go`, `core/ecosystem.go`, `core/<lang>/`)

Atlas supports **npm** (`package-lock.json`) and **Python/Poetry** (`poetry.lock`). We welcome new package managers and language ecosystems!

To add a new ecosystem (e.g., **Cargo / Rust**, **Go Modules**, **Composer / PHP**, **Ruby / Bundler**, **Maven / Gradle**, or **uv / Pipenv**):

1. **Implement `EcosystemAdapter`** (`core/adapter.go`):
   - `RegistryAdapter`: `FetchHealth(n *Node, advisories []Advisory) *Health` (fetches release data, downloads, repository URLs from registry APIs like crates.io, pkg.go.dev, Packagist, etc.).
   - `ScanAdapter`:
     - `SrcExts() map[string]bool`: Supported source file extensions (e.g., `.rs`, `.go`, `.php`).
     - `ImportCandidates(line string) []string`: Parses import statements from source lines.
     - `NewResolver(wanted map[string]bool) func(candidate string) (string, bool)`: Matches import candidates against resolved dependencies.
     - `ManifestSearchText(fileName string, data []byte) (string, bool)`: Extracts manifest references without self-matching.
2. **Implement Lockfile Parsing & Graph Construction**:
   - Create a subpackage under `core/<ecosystem>` (e.g., `core/cargo`, `core/golang`).
   - Parse the lockfile format and populate `*core.Graph` with `*core.Node` entries, directed edges, and direct dependency flags.
3. **Register the Ecosystem** (`core/ecosystem.go` & `main.go` / `agents/orchestrator.go`):
   - Add the enum constant, lockfile detection in `DetectEcosystem()`, and OSV.dev ecosystem string in `(e Ecosystem).OSVEcosystem()`.

### B. Multi-Agent Reasoning & Specialists (`agents/`)

Atlas dispatches parallel agents to evaluate high-risk dependencies concurrently:

- **Coupling Agent (`agents/coupling.go`):** Assesses whether a dependency is deeply load-bearing or surface-level utility.
- **Vulnerability Agent (`agents/vulnerability.go`):** Contextualizes CVE/GHSA severity by reachability in production vs. test-only usage.
- **Maintenance Agent (`agents/maintenance.go`):** Computes bus factor, single-contributor risk, commit recency, and abandonment probability.
- **Remediation Agent (`agents/remediation.go`):** Formulates step-by-step upgrade, replacement, or mitigation plans.
- **Action Agent (`agents/action.go`):** Formats findings into GitHub issue drafts, checks for existing duplicate issues concurrently, and files issues with rate limiting.

**Contribution Ideas:**
- **New Specialist Agents:**
  - *License Compliance Agent:* Detects copyleft/restrictive licenses (AGPL, GPL) in proprietary or commercial codebases.
  - *Supply Chain & Typosquatting Agent:* Flags newly registered packages, rapid maintainer shifts, or suspicious install scripts.
  - *Dependency Bloat Agent:* Flags micro-packages, polyfills that modern runtimes supersede, or opportunities for zero-dependency replacements.
- **Prompt Engineering & Schema Tuning:** Improve system prompts, few-shot schemas, and precision in Ollama outputs.
- **Robust Fallbacks:** Improve deterministic heuristics when Ollama is unavailable.

### C. Core Deterministic Data Engine (`core/`)

The deterministic engine provides verified facts:

- **Graph Algorithms (`core/graph.go`):** Compute transitive blast radius, betweenness centrality, shortest dependency paths, and cycle detection.
- **Static Usage Scanner (`core/scan.go`):** Scans cloned source trees to classify dependencies as `runtime`, `test-only`, `config-referenced`, `build-tooling`, or `unused-candidate`. Contributions can improve AST parsing, regex performance, and reduce false positives/negatives in complex imports (e.g. dynamic imports, JSX/TSX, monorepos).
- **Health & Security Aggregator (`core/health.go`):** Queries GitHub REST API (commit activity, contributor stats, bus factor), package registries, and OSV.dev advisories. Contributions can add caching, optimize concurrency, or handle rate limits more cleanly.
- **Scoring Model (`core/score.go`):** Refine the 0–100 deterministic health score formula to balance advisory weight, abandonment signals, and bloat penalties.
- **Preflight & Error Handling (`core/preflight.go`, `core/errors.go`):** Extend typed error classification (`ErrKind`), diagnostic hints, and non-blocking warnings.

### D. Visualizations & Interactive Reports (`core/viz.go`, `core/report.go`)

- **Interactive Graph (`graph.html` / `core/viz.go`):**
  - Powered by D3.js force-directed simulations.
  - Features: Node clustering by ecosystem/risk, search filtering, zoom/pan navigation, node sizing proportional to blast radius.
  - Opportunities: Export to SVG/PNG, subgraph filtering (isolate vulnerable subtrees), dark/light theme toggles.
- **Comprehensive HTML Report (`core/report.go`):**
  - Self-contained, single-file HTML report with embedded styles, summary cards, and multi-agent breakdowns.
  - **Design System Requirement:** All UI styling must adhere strictly to the **Monkeytype `iv_spade` dark theme** defined in [design.md](file:///c:/Users/acer/Desktop/DDC%20projects/FrogtoberAtlas/design.md) (`--bg: #0c0c0c`, `--sub-alt: #121212`, `--sub: #404040`, `--main: #b7976a`, `--text: #d3c2c3`, typography: Roboto Mono and Lexend Deca).

### E. CLI, REPL & Developer Experience (`main.go`, `agents/orchestrator.go`)

- **Interactive Agent REPL:** Enhance interactive command parsing, slash commands (`/help`, `/export`, `/filter`), and terminal rendering.
- **Output Formats:** Add export flags for **SARIF**, **CycloneDX / SPDX SBOM**, or structured **JSON**.
- **CI/CD Integration:** Build GitHub Actions workflows for running Atlas in pull requests to enforce dependency health budgets.

### F. Testing, Mocking & Fixtures

- **Unit & Integration Tests:** Add comprehensive unit tests in `_test.go` files.
- **Offline / Fake HTTP Transports:** Atlas tests should never make live internet calls to GitHub or Ollama. Use `net/http/httptest` or mock transports (see `core/nettest_test.go` and `core/errors_test.go`).
- **Test Lockfiles (`examples/`):** Add representative lockfiles exhibiting edge cases (deep nesting, peer dependency conflicts, cyclic references).

---

## Development Environment Setup

### 1. Prerequisites

| Tool | Version | Purpose |
|---|---|---|
| **Go** | `1.22+` (or `1.21+`) | Core compiler and runtime |
| **Git** | `2.30+` | Version control & shallow cloning |
| **Ollama** *(Optional)* | Latest | Local LLM inference for agent mode (`ollama pull gemma4:e4b`) |
| **GitHub Token** *(Optional for local scan, required for private repos & issue creation)* | Classic / Fine-grained | GitHub REST API access |

### 2. Local Setup Steps

```bash
# 1. Clone the repository
git clone https://github.com/ghostuf/Atlas.git
cd Atlas

# 2. Configure environment variables
cp .env.example .env
```

Edit `.env` to supply your credentials:
```env
GITHUB_TOKEN=ghp_yourPersonalAccessTokenHere
OLLAMA_HOST=http://localhost:11434
```

### 3. Running Atlas Locally

**One-Shot Deterministic Mode (No Ollama required):**
```bash
# Analyze a local npm lockfile
go run . -file ./examples/package-lock.json -top 20

# Generate the interactive D3 dependency graph
go run . -file ./examples/package-lock.json -viz graph.html

# Analyze a local Python Poetry lockfile
go run . -file ./examples/python/poetry.lock -top 20
```

**Interactive Agent Mode (Uses Ollama):**
```bash
# Launch interactive REPL
go run .

# Preload a repository directly
go run . -repo expressjs/express
```

### 4. Running Tests & Quality Checks

Always verify that your changes pass tests and lint checks:

```bash
# Run all tests across all packages
go test -v ./...

# Run tests with race condition detector
go test -race ./...

# Run static analysis
go vet ./...

# Ensure code is formatted according to standard Go conventions
gofmt -s -w .
```

---

## Contribution Workflow & Git Protocol

We follow a structured Git branching model to ensure project stability:

### 1. Branching Strategy

- **`main`**: Production-ready, stable releases. Do **not** submit PRs directly to `main`.
- **`dev`**: Active integration branch. **All feature and bugfix PRs must target `dev`**.
- **Topic Branches**: Branch off from `dev` using descriptive prefixes:
  - `feat/<feature-name>` (e.g., `feat/cargo-ecosystem`, `feat/sarif-export`)
  - `fix/<bug-name>` (e.g., `fix/pypi-url-normalization`, `fix/rate-limit-retry`)
  - `perf/<optimization>` (e.g., `perf/scanner-worker-pool`)
  - `refactor/<cleanup>` (e.g., `refactor/adapter-interface`)
  - `docs/<topic>` (e.g., `docs/contributing-guide`)
  - `test/<suite>` (e.g., `test/offline-mock-fixtures`)

### 2. Commit Message Standards

We follow the [Conventional Commits](https://www.conventionalcommits.org/) specification. Write clear, concise, imperative commit messages:

```
<type>(<scope>): <imperative summary>

[optional body explaining motivation, context, and alternatives considered]

[optional issue reference: Fixes #123]
```

**Types:**
- `feat`: A new feature or capability
- `fix`: A bug fix
- `perf`: A code change that improves performance
- `refactor`: Code change that neither fixes a bug nor adds a feature
- `test`: Adding or correcting tests
- `docs`: Documentation only changes
- `chore`: Maintenance, build tasks, or tooling updates

**Examples:**
- `feat(adapter): add crates.io registry health adapter`
- `fix(health): retry GitHub contributor stats on HTTP 202`
- `perf(scan): parallelize AST import parsing across source files`
- `test(core): add fake HTTP transport test for OSV query batching`

---

## Pull Request (PR) Structure & Schematics

To maintain high code quality and streamline reviews, **every pull request must adhere to the schematics below**.

### PR Title Convention

Format your PR title as:
```
<type>(<scope>): <concise description in imperative mood>
```
*Examples:*
- `feat(agents): add license compliance specialist agent`
- `fix(errors): clarify network timeout during shallow clone`
- `refactor(python): streamline PEP 503 package name normalization`

### The PR Description Schematic

When opening a Pull Request, copy and complete the following markdown template:

```markdown
## 1. Overview & Motivation
<!-- What problem does this PR solve? Why is this change necessary? -->
<!-- Link related issues: Fixes #XX or Closes #XX -->

## 2. Architectural Impact & Seam
<!-- Which architectural components are modified? (Check all that apply) -->
- [ ] Ecosystem Adapters (`core/adapter.go`, `core/ecosystem.go`, `core/<lang>`)
- [ ] Deterministic Data Engine (`core/graph.go`, `core/scan.go`, `core/health.go`)
- [ ] Multi-Agent Reasoning (`agents/`)
- [ ] Visualization & HTML Reports (`core/viz.go`, `core/report.go`, `reports/`)
- [ ] CLI & Developer Experience (`main.go`, `agents/orchestrator.go`)
- [ ] Testing, Mocks & Infrastructure

## 3. Detailed Changes
<!-- Provide a bulleted summary of concrete changes made in this PR -->
- Added ...
- Modified ...
- Deprecated/Removed ...

## 4. AI & Deterministic Fallback Resilience
<!-- If this PR modifies agent prompts, schemas, or LLM interactions: -->
<!-- Describe how deterministic fallbacks behave when Ollama is offline or returns invalid output. -->
<!-- If non-agent code, write "N/A". -->

## 5. Testing & Verification
<!-- How did you test these changes? Live credentials must NOT be required for tests. -->
- [ ] Unit tests added / updated
- [ ] Ran `go test -race ./...` (All tests passed cleanly)
- [ ] Ran `go vet ./...`
- [ ] Ran `gofmt -s -w .`
<!-- Include relevant test output or sample CLI command used for manual verification -->
```
go test -v ./core/...
```

## 6. Screenshots / Terminal Output (if applicable)
<!-- If changes affect terminal output, D3 visualizations, or HTML reports, paste screenshots or diffs below. -->

## 7. Contributor Checklist
- [ ] My PR targets the `dev` branch (not `main`).
- [ ] I have not added any external Go dependencies (`go.mod` remains stdlib-only).
- [ ] New and existing unit tests pass locally with `-race`.
- [ ] My code adheres to the project's error handling patterns (`core.Error` with separated `Facts` and `Maybe`).
- [ ] All UI changes strictly conform to the Monkeytype `iv_spade` design theme tokens.
- [ ] Documentation and comments have been updated accordingly.
```

### PR Review & Merging Lifecycle

1. **Automated & Manual Review:** Maintainers will review your PR for architectural alignment, zero-dependency purity, error handling, and test coverage.
2. **Review Feedback:** Address feedback by pushing additional commits to your branch. Avoid force-pushing while active review comments are open.
3. **Squash & Merge:** Once approved, maintainers will squash and merge your branch into `dev`. Periodic release PRs will merge `dev` into `main` with appropriate release tags.

---

## Coding Standards & Architectural Tenets

To ensure Atlas remains maintainable and reliable, please follow these guidelines:

### 1. Zero External Go Dependencies
Atlas is committed to standard library purity. If you believe a task requires an external library, please open an issue first to discuss alternatives. Go's standard library provides rich networking, JSON parsing, cryptography, concurrency, and profiling tools that meet our requirements.

### 2. Seam Decoupling & Import Cycle Prevention
- Subpackages like `core/python` import `atlas/core` for types (`Node`, `Health`, `Graph`, etc.).
- `atlas/core` **must never** import `atlas/core/python` or other ecosystem packages.
- The glue logic resides in `main.go` and `agents/orchestrator.go`, which select the appropriate `core.EcosystemAdapter`.

### 3. Truthful, Classified Error Handling
Atlas uses structured, user-facing error reporting (`core/errors.go`):
- Errors must be classified with an explicit `ErrKind`.
- Maintain strict separation between **`Facts`** (empirically proven by a status code, response, or syscall) and **`Maybe`** (unverified hypotheses).
- Always redact API tokens, secrets, or sensitive credentials in error details.
- Provide actionable `Next` steps whenever possible.

```go
// Example of structured error creation
return &core.Error{
    Kind:    core.ErrFileNotFound,
    Title:   "lockfile not found in repository",
    Summary: "Atlas could not locate package-lock.json or poetry.lock at the requested path.",
    Facts:   []string{"Repository clone succeeded", "Path 'frontend/package-lock.json' does not exist"},
    Maybe:   []string{"The lockfile might be in a different subdirectory (try -dir)", "The repository might use an unsupported package manager"},
    Next:    []string{"Run with -dir <subpath> to point to the directory containing your lockfile"},
}
```

### 4. Concurrency & Rate Limiting
- Use bounded concurrency primitives (`agents.ConcurrencyLimiter` or `agents.RunParallel`) to prevent overwhelming local hardware or remote APIs.
- Respect GitHub API rate limits. Provide polite pacing and handle HTTP 202 (computing stats) with bounded retries.

### 5. UI & Design System Consistency
Any modifications to `graph.html`, the D3 visualization, or HTML reports must adhere to the **`iv_spade` dark theme**:
- Canvas Background: `#0c0c0c` (`--bg`)
- Surface / Cards: `#121212` (`--sub-alt`)
- Borders / Inactive Dividers: `#404040` (`--sub`)
- Primary Body Text: `#d3c2c3` (`--text`)
- Warm Gold Accent: `#b7976a` (`--main`), Hover: `#c1a174` (`--main-hover`)
- Error / Warnings: `#9d7b7d` (`--error`), `#a78587` (`--error-extra`)
- Typography: Roboto Mono (code / metrics) and Lexend Deca (interface / labels)

---

## Getting Help & Community

- **Have a question or proposed feature?** Open a [GitHub Issue](https://github.com/ghostuf/Atlas/issues) with the tag `question` or `proposal`.
- **Found a bug?** Open an issue with reproduction steps, the lockfile ecosystem, and relevant CLI flags.
- **Participating in Frogtober Fest 2026?** Tag your PR with `frogtoberfest` so maintainers can celebrate and track your contributions!

Thank you for helping make Atlas the definitive tool for understanding real-world software supply chains! Happy hacking! 🐸✨
