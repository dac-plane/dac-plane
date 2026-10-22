# Issue #68 — Decide the deployment unit and first substrate

Tracker: https://github.com/dac-plane/dac-plane/issues/68
Branch: `docs/68-deployment-unit-adr`, stacked on `docs/67-accept-adr-0008` (#79), from `main` at `01184ee`.
Parent: #65 (M2: Deployable control plane).

## Direction

Record ADR-0009 and add the deployment view the baseline README calls for. This is a decision and a
diagram, not an implementation. Inputs already decided are reused:

- ADR-0002: one application, API and loops in one process until a split trigger is proven.
- ADR-0007: TLS placement is left to this ADR, but plaintext is never served off loopback.
- ADR-0008: adapters dial in; in-process ports stay out of production; a replica claims work only
  while it holds a provider connection.

Decisions confirmed with the maintainer on 2026-10-22, after reviewing the evidence:

- Migrations run only through a separate `dac-plane migrate` step; serving processes refuse to start
  outside their release's schema range.
- First substrate: one OCI image plus a production-capable reference Docker Compose stack; Kubernetes
  later on evidence.
- dac-plane terminates TLS itself; an operator proxy is allowed only with TLS on the hop to dac-plane.
- Every replica runs all roles (adopted from ADR-0002, not reopened).

Evidence used: the CP9 deployment-fit gate in
`architecture-explore/docs/qualification/control-plane-candidates.md` ("local-only demos do not
pass"; a Kubernetes-only topology counted against a candidate), the operator setup, upgrade and
rollback measurement in `architecture-explore/docs/spec/SPEC.md`, and Grok Bot's single update lease
and claimed upgrade schedules in `plane-bot-explore/notes/MASTER-RECORD.md` §5. Grok Bot is
vendor-operated and offers no self-hosted deployment model.

## Implementation

- [x] Confirm the open decisions before drafting.
- [x] Read the entry point, configuration, bootstrap and migration provider.
- [x] Draft `docs/adr/0009-deployment-unit-and-first-substrate.md`; marked Accepted in the PR so merging records acceptance.
- [x] Add `docs/architecture/deployment-view.drawio` and export `.svg` and `.png` from it.
- [x] Describe the deployment view in `docs/architecture/README.md`.
- [ ] Maintainer review of the draft and the view.
- [x] Link ADR-0009 from "Source decisions".

## Verification

- `git diff --check`
- Every relative link in the ADR and README resolves on this branch.
- The `.svg` and `.png` are exported from the committed `.drawio` source.

## Findings carried to implementation issues

- `cmd/dac-plane` has one mode; it needs `serve`, `migrate` and `provision` subcommands (#73, with
  `provision` from #71).
- Startup never checks the schema version; `serve` needs the release-range check (#73).
- Nothing yet guards that the release binary excludes `internal/simulator`; add a dependency-graph
  check as a justfile recipe CI calls (#73).
- Migrations need a declared compatibility range and the expand-then-contract rule (#73, proven in
  #74).

## Remaining boundaries

This issue does not choose PostgreSQL high availability or backup tooling, observability export,
resource sizing, Kubernetes packaging, or an artifact store. No production provider is qualified (#75).
