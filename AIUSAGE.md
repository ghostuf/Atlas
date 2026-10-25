# AI Usage Disclosure

Atlas uses a locally-hosted Ollama model (default: `gemma4:e4b`) for two distinct purposes: **conversational reasoning** and **schema-constrained structured output**. No user data, source code, dependency manifests, or tokens are transmitted to any external AI service. All inference runs on the user's own hardware.

This document identifies every location in the Atlas codebase where AI-generated output is consumed **programmatically** — meaning the model's response is parsed, deserialized into a Go struct, or otherwise used to drive application logic beyond simple display.

---

## 1. Schema-Constrained JSON Agents

Each specialist agent below sends the Ollama model a JSON schema alongside a domain-specific prompt. The model's response is `json.Unmarshal`-ed directly into a typed Go struct. If the model call fails, returns an empty response, or produces invalid JSON, the function falls back to a deterministic heuristic in the same code block. **Raw model text is never passed through as structured data.**

### 1.1 Coupling Agent

| | |
|---|---|
| **File** | `agents/coupling.go` |
| **Function** | `CouplingAgent.Analyze` |
| **AI call** | `core.ChatOllamaJSON` (line 80) |
| **Deserialized into** | `CouplingAssessment` struct (`agents/types.go`, line 36) |
| **Struct fields populated by AI** | `coupling_level`, `architectural_role`, `reason`, `is_load_bearing` |
| **Downstream consumers** | `agents/orchestrator.go` (deep-risk profile assembly) and `core/report.go` (HTML report rendering) |
| **Fallback** | Deterministic heuristic based on production file count, directory spread, blast radius, and tag presence (lines 95–124) |

### 1.2 Vulnerability Agent

| | |
|---|---|
| **File** | `agents/vulnerability.go` |
| **Function** | `VulnerabilityAgent.Analyze` |
| **AI call** | `core.ChatOllamaJSON` (line 103) |
| **Deserialized into** | `VulnerabilityAssessment` struct (`agents/types.go`, line 44) |
| **Struct fields populated by AI** | `contextual_severity`, `exploitability`, `summary`, `urgency` |
| **Downstream consumers** | `agents/orchestrator.go` (deep-risk profile assembly) and `core/report.go` (HTML report rendering) |
| **Fallback** | Deterministic classification based on raw OSV severity, usage classification (`runtime`, `test-only`, etc.), and production file count (lines 118–159) |

### 1.3 Maintenance Agent

| | |
|---|---|
| **File** | `agents/maintenance.go` |
| **Function** | `MaintenanceAgent.Analyze` |
| **AI call** | `core.ChatOllamaJSON` (line 71) |
| **Deserialized into** | `MaintenanceAssessment` struct (`agents/types.go`, line 53) |
| **Struct fields populated by AI** | `abandonment_risk`, `bus_factor_warning`, `maintainer_health`, `recommendation` |
| **Downstream consumers** | `agents/orchestrator.go` (deep-risk profile assembly), `core/report.go` (HTML report rendering), and `agents/remediation.go` (remediation plan synthesis) |
| **Fallback** | Deterministic heuristic using archived status, bus factor percentage, contributor count, weekly downloads, and `core.IsAbandonmentRisk` (lines 86–117) |

### 1.4 Remediation Agent

| | |
|---|---|
| **File** | `agents/remediation.go` |
| **Function** | `RemediationAgent.Synthesize` |
| **AI call** | `core.ChatOllamaJSON` (line 101) |
| **Deserialized into** | `RemediationPlan` struct (`agents/types.go`, line 62) |
| **Struct fields populated by AI** | `suggested_fix`, `alternative_libs`, `migration_effort`, `action_required` |
| **Downstream consumers** | `agents/action.go` — `CollectRisks` and the issue-body template use the `RemediationPlan` to build GitHub issue titles and bodies; `ActionAgent.CreateIssuesConcurrently` then POSTs them via the GitHub REST API |
| **Fallback** | Deterministic plan based on advisory fix-versions, archived status, and abandonment risk level (lines 116–139) |

---

## 2. Conversational Agent (Tool-Call Orchestration)

| | |
|---|---|
| **File** | `agents/orchestrator.go` |
| **Function** | `OrchestratorAgent.RunTurn` |
| **AI call** | `core.ChatOllama` (line 888) |
| **Consumption pattern** | The model returns an `OllamaMessage` whose `ToolCalls` field is iterated over. Each tool call's `Function.Name` and `Function.Arguments` are dispatched to `OrchestratorAgent.DispatchTool`, which executes the corresponding deterministic pipeline function (`analyze_repository`, `scan_usage`, `check_health`, `suggest_issues`, `create_issues`). The model decides **which** tools to invoke and with **what arguments**, but every tool implementation is fully deterministic code. |
| **Fallback** | If the model produces no tool calls and no final answer after `maxToolHops` (6) iterations, the orchestrator prints a fallback message and, if analysis tools ran, writes the report from the already-collected deterministic data (lines 937–941). |

---

## 3. Shared Infrastructure

| Component | File | Purpose |
|---|---|---|
| `ChatOllamaJSON` | `core/ollama.go` (line 141) | Sends a chat request with a `format` schema, receives a raw JSON string, and returns it to the calling agent for `json.Unmarshal`. |
| `ChatOllama` | `core/ollama.go` (line 128) | Sends a chat request with optional tool definitions, returns a full `OllamaMessage` (including any `ToolCalls`). Used by the conversational orchestrator. |
| `OllamaMessage` / `OllamaToolCall` | `core/ollama.go` | Transport types for Ollama API request/response payloads. |

---

## 4. Summary of Programmatic Consumption Points

| # | File | Function | AI Output → Go Type | Side Effects |
|---|---|---|---|---|
| 1 | `agents/coupling.go` | `CouplingAgent.Analyze` | JSON → `CouplingAssessment` | Populates risk profile; rendered in HTML report |
| 2 | `agents/vulnerability.go` | `VulnerabilityAgent.Analyze` | JSON → `VulnerabilityAssessment` | Populates risk profile; rendered in HTML report |
| 3 | `agents/maintenance.go` | `MaintenanceAgent.Analyze` | JSON → `MaintenanceAssessment` | Populates risk profile; rendered in HTML report; feeds into remediation |
| 4 | `agents/remediation.go` | `RemediationAgent.Synthesize` | JSON → `RemediationPlan` | Drives GitHub issue title/body via `agents/action.go` |
| 5 | `agents/orchestrator.go` | `OrchestratorAgent.RunTurn` | `OllamaMessage.ToolCalls` → dispatched function calls | Model selects tools and arguments; tools are deterministic |

Every path listed above includes a deterministic fallback that activates when the model is offline, unreachable, or returns unparseable output. Atlas completes its analysis gracefully in all failure cases.
