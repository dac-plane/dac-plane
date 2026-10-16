# Agent guide — dac-plane

`AGENTS.md` is a symlink to this file. Edit `CLAUDE.md` only.

## What this is

`dac-plane` is a provider-neutral control plane for delegated agent work. It owns the durable
product lifecycle—tasks, attempts, attention, decisions, budgets, artifacts, audit, result
revisions and reviews—while agent runtimes and execution environments remain replaceable
integrations.

**The product lifecycle is not implemented yet.** The repository holds the reviewed architecture
baseline, its ADRs and the Go process foundation (`cmd/`, `internal/`): configuration, PostgreSQL
connectivity, the Goose migration provider and integration-test support, with no product records,
schema or HTTP surface. M1 is tracked by epic #11, whose ordered issues are the implementation
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
internal/             application modules: bootstrap, config, ids, identity, lifecycle, platform
contracts/jsonschema/ language-neutral JSON Schema 2020-12 contracts, tested against the Go types
tasks/plan.md         the plan for the issue currently in progress, rewritten per issue
architecture-explore/ retained research, experiments and prototypes (read-only by default)
.github/              the PR-title gate and workflow conventions
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

CI (`.github/workflows/ci.yml`) runs these; run them before opening a PR. Integration tests need
Docker.

```sh
test -z "$(gofmt -l cmd internal)"
go vet ./...
go test -race ./...
go test -tags=integration ./...
```

## Hygiene

- **No secrets in the tree, a log or a PR body.** Nothing here needs one yet; keep it that way.
- **`.work/` is local scratch** (logs, caches, scripts): never commit it. The root
  `.gitignore` excludes it.
