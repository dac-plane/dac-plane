# Issue #75 — Qualify the first runtime, environment and artifact store

Tracker: https://github.com/dac-plane/dac-plane/issues/75
Branch: `test/75-joint-provider-qualification`, from `main` at `10d3920`.
Parent: #65, last open item. Blocks M3 (#94).

1. Run Codex, E2B and SeaweedFS together with no simulator: `just qualify-providers`.
2. Add the unresolved-approval scenarios the #91 note left open.
3. Record dispositions and limits in `docs/architecture/provider-qualification-75.md`.
4. Move the registry digest requirement to #95, then close #75 and #65.
