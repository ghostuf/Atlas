# Ideating Atlas : Frogtober 2026

## Problem.
Every non-trivial codebase quietly depends on a handful of packages that, if they broke or got abandoned, would take down large parts of the system, an auth middleware, a logging library, a single-maintainer utility half the codebase imports indirectly. Developers rarely know which dependencies these are until an incident forces them to find out. npm audit and similar tools flag known vulnerabilities, but none of them answer the actually important question: how deeply is my system architecturally wired into this dependency, and what happens to me specifically if it breaks or its maintainer walks away. For teams managing multiple codebases (agencies, platform teams, solo maintainers), this risk compounds silently until a dependency actually fails, at which point the cost is an emergency migration under pressure instead of a planned one.

## Idea.
Atlas scans a codebase’s dependency graph and actual usage patterns (not just what’s listed in the manifest, but how deeply each dependency is woven through the code) and produces a ranked “load-bearing” report: which dependencies would cause the most damage if they broke, why, who maintains them, and how healthy that maintenance actually looks. For the highest-risk dependencies, it generates concrete suggestions: what to fix now, what migration path exists, and what monitoring or fallback to put in place. Where the risk is severe (single unmaintained maintainer, security history, deep architectural coupling), it can open a tracked issue automatically instead of leaving the finding buried in a report nobody reads.

## How AI powers it.
The model performs three distinct jobs, each consumed programmatically by the next step, not just displayed to a user:

Classifying depth of coupling — reads actual usage sites of each dependency (not import counts) and judges how architecturally embedded it is (surface-level utility vs. load-bearing infrastructure), producing a structured coupling score.
Reasoning over blast radius — combines that coupling score with retrieved maintainer/health signals (commit recency, bus-factor, open security issues) to predict what breaks and how badly if that dependency fails or has a breaking change, output as a structured risk record.
Generating remediation — for dependencies above a risk threshold, generates a concrete fix/migration suggestion grounded in the specific usage pattern found in step 1, and this output is what triggers the system to open a real tracked issue, not just text shown to a developer.
