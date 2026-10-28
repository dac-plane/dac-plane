# Issue #75 — Qualify the first production providers

Tracker: https://github.com/dac-plane/dac-plane/issues/75
Branch: `feature/75-provider-qualification-plan`, from `main` at `5ac7fa2`.
Parent: #65 (M2: Deployable control plane).
Tasks are tracked as linked GitHub sub-issues rather than duplicated in a local checklist.

## Direction

Complete M2 by qualifying one production agent runtime, one execution-environment provider and one
artifact byte store against the existing provider-neutral contracts. Qualification is an evidence
decision, not a vendor-integration claim: every selected provider must pass its applicable conformance
and failure suite against a pinned real version. A failed candidate remains unqualified and in shadow
or off mode.

Publishing, operating and demonstrating dac-plane are intentionally downstream work. They are tracked
under #94 and cannot weaken or bypass the #75 qualification gate.

## Task List

### Phase 1: Provider qualification

1. [#91 — qualify the first production agent runtime](https://github.com/dac-plane/dac-plane/issues/91)
2. [#92 — qualify the first execution environment provider](https://github.com/dac-plane/dac-plane/issues/92)
3. [#93 — qualify the first artifact byte store](https://github.com/dac-plane/dac-plane/issues/93)

The three roles have independent qualification decisions. Candidate investigation may proceed in
parallel, but shared adapter-contract changes must be decided before provider-specific implementations
diverge. Each issue owns its adapter, provider-specific recipe and observed evidence note.

### Checkpoint: close M2

- Every qualified provider passes the full applicable conformance and failure suite.
- Exact provider and adapter revisions, commands, durations, failures and limits are recorded.
- Unsupported capabilities are not advertised; failed providers remain shadow or off.
- `just ci` passes with no simulator behavior leaking into the release binary.
- #75 closes only after #91, #92 and #93 are complete; #65 closes only after #75.

### Phase 2: Release and adoption

Successor epic: [#94 — release, operate and demonstrate dac-plane](https://github.com/dac-plane/dac-plane/issues/94)

1. [#95 — publish the first supported dac-plane distribution](https://github.com/dac-plane/dac-plane/issues/95)
2. [#96 — operate the first supported dac-plane pilot](https://github.com/dac-plane/dac-plane/issues/96)
3. [#97 — demonstrate the deployment from an external application](https://github.com/dac-plane/dac-plane/issues/97)

The dependency order is #75 → #95 → #96 → #97. The external example belongs in a separate repository
and consumes only the published Surface API, OpenAPI document and adapter kit. It must not import
`internal/`, share the product database or rely on simulator-only controls.

## Architecture Decisions

- Keep production adapters out of the dac-plane process; they connect through ControlFrame as
  required by ADR-0008. Go adapters can use `adapterkit`; adapters in other languages use the same
  JSON Schemas and conformance suite.
- Keep provider names and provider-specific fields out of the neutral schemas.
- Publish immutable, versioned artifacts before any external example depends on dac-plane.
- Use ADR-0009's Compose substrate for the first pilot; Kubernetes and unmeasured availability claims
  remain out of scope.
- Treat the example as an independent consumer and demonstration, not as the canonical first-party
  workspace or proof of production readiness.

## Risks and Mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| A candidate documents a capability it cannot uphold under failure | High | Require executed failure and recovery evidence before live qualification. |
| One vendor couples runtime and environment roles | Medium | Test and record each role independently even if one deployment supplies both. |
| Release automation publishes incompatible image and kit versions | High | Publish one compatibility statement from an immutable tag and verify from a clean consumer. |
| The example bypasses public contracts for convenience | High | Separate repository, no internal imports or database access, and an end-to-end public-interface test. |
| Pilot evidence is overstated as production readiness | Medium | Record exact topology and limits; make no SLO, HA, RPO or RTO claim without measurement. |

## Verification

- Confirm #91, #92 and #93 are linked sub-issues of #75.
- Confirm #95, #96 and #97 are linked sub-issues of #94.
- Confirm every issue has explicit scope, acceptance criteria, verification and dependency statements.
- Run `git diff --check` for this planning change.
