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
```bash
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
