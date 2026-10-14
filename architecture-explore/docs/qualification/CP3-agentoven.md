# CP3: AgentOven source qualification

- Audited: 2026-10-14
- Source: https://github.com/agentoven/agentoven
- Pinned tag: `v0.9.0`
- Pinned commit: `ba194cc435ec0d8e431a9b603663edb145327227`
- Public license: Apache-2.0
- Result: **CLOSED — REJECTED BY SOURCE SCREEN**
- Runtime/provider test: not run

## Decision

Do not install or integrate AgentOven as the product control-plane foundation. The repository contains
real implementations and tests, but its current maintenance, release and product-boundary evidence is
not strong enough for a foundational dependency. The audit stopped before using the Camer Digital key
because provider routing success would not repair the more basic durability, deployment and ownership
problems below.

This is not a claim that AgentOven is abandoned or empty. It is actively developed. The problem is that
the project is young, effectively single-maintainer, changing in very large increments, and publishing a
public edition that does not contain the production state and security topology required by this product.

## Evidence collected

### Maintenance and contributor concentration

The repository was created on 2026-02-13. At the pinned tag it had about 107 commits, 337 tracked files
and approximately 83,000 lines across Go, Rust, Python and TypeScript source. GitHub attributed almost all
contributions to identities belonging to the same maintainer; additional commits used an `AI SDLC Agent`
identity. The repository showed five historical pull requests, all authored by the owner, and negligible
independent issue/PR activity.

The repository is therefore active but has an effective bus factor of one. Activity alone is not evidence
that another team can safely depend on the project, receive reviewed releases or recover if the maintainer
stops.

### Change shape and maintainability

Several commits added thousands of lines at once. Examples in the audited history included changes of
about 6,270, 5,739, 4,535, 3,763 and 2,121 lines. Core files were also unusually large:

| File | Approximate lines at the pinned tag |
|------|-------------------------------------:|
| `control-plane/internal/api/handlers/handlers.go` | 6,555 |
| `crates/agentoven-cli/src/commands/skills.rs` | 2,994 |
| `control-plane/pkg/models/models.go` | 2,795 |
| `control-plane/internal/router/router.go` | 2,718 |
| `control-plane/internal/store/memory.go` | 2,332 |
| `control-plane/internal/workflow/engine.go` | 2,235 |

Generated `.vite/deps` metadata was tracked. The pinned tree also failed `cargo fmt --all -- --check` over
many Rust files. These facts do not prove individual runtime bugs, but together they make review,
ownership and safe upgrading materially harder for a control-plane dependency.

### CI and release hygiene

The latest default-branch CI run inspected during the audit was red. Go tests, the dashboard build,
Docker build, SDK builds and the lightweight Go integration job passed, but Rust formatting failed on
Linux and macOS. The workflow therefore skipped Rust linting, compilation and tests.

The `v0.9.0` release workflow also failed. Rust publication returned a crates.io authentication failure,
Python publication failed, and GitHub release creation was skipped. GitHub Actions history showed the
same `v0.9.0` tag being pushed against different commits while release fixes were attempted. A version
pin cannot be trusted as an immutable upgrade boundary when its tag is moved.

Primary evidence:

- CI run: https://github.com/agentoven/agentoven/actions/runs/37948513770
- release run: https://github.com/agentoven/agentoven/actions/runs/37948433141
- pinned source: https://github.com/agentoven/agentoven/tree/v0.9.0

### Public versus production implementation

The public server constructor calls `store.NewMemoryStore()`. That store can periodically write a local
`data.json` snapshot, which is useful for local restart recovery, but it is not a PostgreSQL control-plane
store suitable for concurrent or horizontally scaled production operation.

The public Docker/CLI path is internally inconsistent:

- `agentoven local up` and the Compose material describe a PostgreSQL-backed full mode;
- the public server still constructs the memory/JSON store;
- `DATABASE_URL` in the public server is used to register pgvector, not to replace control-plane state;
- the PostgreSQL store is supplied through `NewWithStore` by the separate Pro implementation.

AgentOven's architecture document places the PostgreSQL store, production dashboard, stronger identity
controls, scheduler, operator and Kubernetes production topology in the private `agentoven-pro`
repository. The public repository cannot establish whether those components satisfy this project's
durability, recovery, authorization or non-Kubernetes requirements.

Primary evidence:

- public server wiring: https://github.com/agentoven/agentoven/blob/v0.9.0/control-plane/pkg/server/server.go
- memory snapshot store: https://github.com/agentoven/agentoven/blob/v0.9.0/control-plane/internal/store/memory.go
- public/Pro architecture: https://github.com/agentoven/agentoven/blob/v0.9.0/docs/architecture.md
- open-core decision: https://github.com/agentoven/agentoven/blob/v0.9.0/ADR/0003-open-core-two-repo-model.md

### Positive evidence retained

The rejection should not erase evidence that may be useful later:

- `go test ./...` passed locally for the public control-plane source;
- the tree contained about 360 Go test functions and 32 Rust test annotations;
- source implementations exist for provider routing, recipes, sessions, approval records, A2A, MCP,
  execution and local snapshot recovery;
- the latest CI built the public Docker image, dashboard, Go server and SDK artifacts successfully;
- the CLI exposes provider `base_url` configuration.

This establishes that AgentOven is a substantive prototype. It does not establish a supportable,
auditable production foundation.

## Qualification gates

| Gate | Result | Evidence |
|------|--------|----------|
| CP1 install | **NOT RUN** | Source screen made installation unnecessary. |
| CP2 provider routing | **NOT RUN** | No Camer Digital key or endpoint was used. Source exposes `base_url`, but documentation is not runtime proof. |
| CP3 delegated task | **NOT RUN** | No bounded task was executed. |
| CP4 approval authority | **NOT RUN** | Approval code/tests exist; authority binding was not executed. |
| CP5 result evidence | **NOT RUN** | APIs and traces exist; no delegated result was produced. |
| CP6 cancellation/cleanup | **NOT RUN** | No worker or environment was started. |
| CP7 product mapping | **NOT RUN** | Candidate-native records were not mapped through a real run. |
| CP8 adapter portability | **NOT RUN** | Public interfaces exist; private Pro ownership prevents a complete assessment. |
| CP9 deployment fit | **FAIL** | Public production durability/security is incomplete, the documented production topology is Kubernetes/Pro, and release hygiene is not dependable. |

## Consequence for this project

AgentOven is not the next runtime qualification. Paperclip, Kelos and AgentOven have now failed for
different reasons:

- Paperclip: executed provider-routing failure in managed Codex;
- Kelos: Kubernetes conflicts with the selected deployment constraint;
- AgentOven: source maturity, release hygiene and public production-deployment fit fail before runtime.

The project should keep its product work contract and implement only the smallest owned lifecycle core.
Runtimes and environments remain replaceable adapters. E2B, OpenSandbox or another environment provider
may manage compute, but must not become the owner of task, attempt, attention, decision, result or audit
state.

## Archived screening criteria

AgentOven is not on the active roadmap. A new architecture decision should not authorize another screen
unless all of these are true:

1. a published release has an immutable tag and a successful release workflow;
2. default-branch CI is green, including Rust format, lint, build and tests;
3. a publicly inspectable durable state path supports the required task/run/approval/result records, or
   the Pro implementation is available for technical and licensing review;
4. a supported production deployment that does not require Kubernetes is documented and reproducible;
5. upgrade, migration, authentication and secret-handling behavior can be tested without relying on
   marketing claims or private implementation assumptions.
