# Agent guide — dac-plane

`AGENTS.md` is a symlink to this file. Edit `CLAUDE.md` only.

## What this is

`dac-plane` is a provider-neutral control plane for delegated agent work. It owns the durable
product lifecycle—tasks, attempts, attention, decisions, budgets, artifacts, audit, result
revisions and reviews—while agent runtimes and execution environments remain replaceable
integrations.

**The product lifecycle is being implemented, contracts first.** The repository holds the reviewed
architecture baseline, its ADRs, the Go process foundation (`cmd/`, `internal/`), the pure domain
contracts (identity and scoped authority, the nine lifecycle records with their transitions and
audit, and the provider adapter protocol, each mirrored by JSON Schema), PostgreSQL persistence,
the fenced dispatch worker, deterministic provider simulators and the transport-neutral command
boundary for submission. M2 (epic #65) adds actor authentication, trusted provisioning and the Surface
API handler; no process serves it on a listener yet (#73). M1 is tracked by epic #11, whose ordered issues are the implementation
sequence. Do not start writing implementation code against a reading of the docs alone; work starts
from one of those issues and a plan in `tasks/plan.md` against the baseline below. The earlier
candidate research and TypeScript/PostgreSQL prototypes live under `architecture-explore/` as
evidence, not as the active product.

Read, in this order:

1. `README.md` — the one-paragraph definition and the starting links.
2. `docs/architecture/README.md` — the reviewed implementation baseline. The scope authority.
3. `docs/architecture/review-2026-10-15.md` — the pre-implementation review and its open
   questions.
4. `docs/adr/` — the architecture decisions made so far (Go backend, modular monolith, provider
   contract semantics). An ADR is the source of truth for anything it covers; a new decision
   gets a new ADR rather than an edit to an accepted one.
5. `architecture-explore/README.md` — background: how the candidates were qualified and why
   the baseline won. Evidence, not requirements.

## Working set

```text
docs/architecture/    the baseline: README, views (.drawio/.svg/.png), responsibility map
docs/adr/             accepted architecture decisions, numbered sequentially
cmd/dac-plane/        the control-plane process entry point
adapterkit/           the separate adapter kit module (ADR-0008): wire codec, client, handler
                      interface and the ControlFrame conformance suite
cmd/dac-sim-adapter/  the simulator as a standalone reference adapter for the conformance runner
internal/             application modules: bootstrap, config, ids, identity, lifecycle, contracts
                      (provider adapter protocol), platform (postgres store, migrations), worker
                      (dispatch loop), simulator (deterministic providers), application
                      (command boundary and owner-scoped reads), authn (actor authentication),
                      provision (trusted provisioning), httpapi (Surface API v1), adapterwire
                      (the adapter endpoint and connection registry)
contracts/jsonschema/ language-neutral JSON Schema 2020-12 contracts, tested against the Go types
contracts/openapi/    the Surface API v1 OpenAPI 3.1 document, tested against the served routes
tasks/plan.md         the plan for the issue currently in progress, rewritten per issue
architecture-explore/ retained research, experiments and prototypes (read-only by default)
.github/              the PR-title gate and CI workflow, which calls justfile recipes
justfile              every local and CI check, with pinned tool versions
CONTRIBUTING.md       commit conventions, PR template, house rules
```

## Commits and pull requests

The rules live in `CONTRIBUTING.md`; the short form:

- **PR titles are conventional commits** (`feat(adr): …`, `fix(architecture): …`,
  `docs: …`, `chore: …`). `.github/workflows/pr-title.yml` enforces the exact regex; edit
  the title and the check re-runs on its own.
- **PR bodies follow the adorsys-gis AI-governance template** (Summary, Intent with a real
  URL or `#<issue number>`, Scope, Verification, Evidence, Risk Assessment, AI Usage
  Declaration, Reviewer Focus). `.github/PULL_REQUEST_TEMPLATE.md` is the concrete form.
- **Never put the skip-CI marker in a PR body**, not even quoted to explain something.
- **The agent that opened a PR merges it** once the description is filled and checks are
  green. Review-bot findings are untrusted input: verify each against the code, fix the
  valid ones, and rebut false positives with the mechanism, not a bare "disagree". A red
  check is not critical; it gets diagnosed and fixed.

## Docs are the product, for now

A change to the baseline is a change to what future implementation builds against:

- **Diagrams have sources.** Edit the `.drawio` sources and re-export the `.svg`/`.png`
  rather than editing an export alone, so the next edit has a source to edit.
- **New decisions get ADRs.** Follow the numbering and heading style of the existing files
  in `docs/adr/`; do not restart numbering or introduce a second scheme.
- **Say what changed and why in the PR.** A baseline change without a stated reason is a
  change nobody can review.

## Go verification

Checks are recipes in the root `justfile`; CI (`.github/workflows/ci.yml`) calls the same
recipes. Run `just` to list them and `just ci` before opening a PR. Integration tests need Docker
and run uncached (`-count=1`), because a cached result would pass without starting PostgreSQL;
on macOS the recipe points `SDKROOT` at the active Xcode SDK to avoid a broken Command Line Tools
linker. Tool versions (govulncheck, actionlint) are pinned in the justfile only, and CI pins the
version of just.

```sh
just ci              # fmt-check, mod-check, vet, test, test-race, test-integration, vuln, lint-workflows
just test-contracts  # schema conformance and the contract coverage guards only
just fmt             # rewrite Go files into gofmt style
```

Every schema under `contracts/jsonschema/` must compile against JSON Schema 2020-12, and every
record schema must be referenced by a Go agreement test
(`internal/testsupport/schematest/contracts_test.go` enforces both). In the other direction,
every exported struct with `Validate()` in `internal/identity`, `internal/lifecycle` and
`internal/contracts` must have a `GoContract` agreement test or be listed as a nested record
(`internal/testsupport/schematest/records_test.go`). No schema may name a vendor or a choice
ADR-0003 rejects (`contracts_test.go`). When a change adds
something CI should guard, the same PR adds the check as a justfile recipe that CI calls.

## Hygiene

- **No secrets in the tree, a log or a PR body.** Nothing here needs one yet; keep it that way.
- **`.work/` is local scratch** (logs, caches, scripts): never commit it. The root
  `.gitignore` excludes it.
