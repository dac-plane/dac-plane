# ADR-0003: Explicit trust scope

- Status: Proposed
- Spec: §6 (isolation proposal), §17, §27

## Decision
Begin with an isolated environment per active task (or per explicit, reviewed trust group). Do not adopt
a shared per-user machine by default. `project.trust_group` and `environment` records carry the scope.
Never share a writable checkout between unrelated attempts.

## Open
Whether the selected provider gives the required isolation is unverified (Q4).
