# dac-plane architecture

Status: **reviewed initial implementation baseline**

This directory defines the architecture to implement after the completed exploration in
[`architecture-explore/`](../../architecture-explore/README.md). The diagrams describe product
responsibility and initial software containers. They do not claim that the product is implemented or
deployed. The findings and remaining gates are recorded in the
[pre-implementation architecture review](review-2026-10-15.md).

## Source precedence

This directory and accepted ADRs under [`docs/adr/`](../adr/) are the active implementation source of
truth. `architecture-explore/` contains historical research, rejected hypotheses and prototypes. An
exploration document is normative only where an active document explicitly adopts a decision from it.
In particular, the historical specification's OCC/OpenClaw assumptions and Plane-Bot's vendor-specific
implementation are not part of this baseline. [ADR-0003](../adr/0003-provider-contract-semantics.md)
adopts only the provider-neutral contract semantics supported by that evidence.

## Views

### Responsibility and ownership

[Editable Draw.io source](responsibility-map.drawio) · [Rendered SVG](responsibility-map.svg) ·
[Rendered PNG](responsibility-map.png)

Question answered: **Which layer owns each kind of state and authority?**

Audience: product, architecture, runtime-integration and security engineers.

The product owns the durable delegated-work lifecycle. Agent runtimes own reasoning and native
session/turn behavior. Environment providers own compute lifecycle. Policy, tool and secret gateways
enforce external actions. PostgreSQL and artifact storage are implementation substrates, not alternate
lifecycle authorities.

The view intentionally omits deployment hosts, network protocols, vendor selections and trust zones.
Protocols and deployment are shown in the views below; vendor selections and trust zones remain open.

### Containers

[Editable Draw.io source](container-view.drawio) · [Rendered SVG](container-view.svg) ·
[Rendered PNG](container-view.png)

Question answered: **What independently running software and stores form the product, and how do they
connect?**

Audience: engineers implementing the control plane, its Surface API and provider adapters.

One modular control-plane application owns the lifecycle and PostgreSQL. API handling, lifecycle
transitions, outbox dispatch, reconciliation and the adapter endpoint are modules of that application,
not separate services ([ADR-0002](../adr/0002-initial-application-boundary.md)). Surfaces use the
Surface API over HTTPS with replayable events; the web workspace is later and uses the same API
([ADR-0007](../adr/0007-surface-api-transport-and-authentication.md)). Every provider role, including
artifact byte storage, is reached through a separate adapter process that opens a TLS WebSocket to the
application and never writes PostgreSQL; in-process simulators serve tests only
([ADR-0008](../adr/0008-adapter-process-boundary-and-wire-transport.md)). Artifact identity, digest
and lineage stay in PostgreSQL. The M1 lifecycle core behind this boundary is proven with
deterministic simulators.

The diagram does not prescribe Kubernetes, a cloud, an event broker, Temporal, a particular agent
runtime or an environment provider.

### First deployment

[Editable Draw.io source](deployment-view.drawio) · [Rendered SVG](deployment-view.svg) ·
[Rendered PNG](deployment-view.png)

Question answered: **What runs where in the first supported deployment, and who opens each
connection?**

Audience: operators and engineers packaging the control plane.

One image runs identical `serve` replicas, each with the Surface API, the adapter endpoint and the
worker, against one PostgreSQL primary. Schema changes happen only in a separate `migrate` step before
a rollout. Surface clients and provider adapters both connect in over TLS with tokens from the
operator's OpenID Connect provider; the control plane never dials an adapter. Simulator adapters and a
test issuer exist only in a development profile. See
[ADR-0009](../adr/0009-deployment-unit-and-first-substrate.md).

The view shows the reference Compose stack. It does not choose PostgreSQL high availability, a cloud,
Kubernetes or a production provider.

## Ownership rules

1. `dac-plane` is authoritative for Task, Attempt, AttentionRequest, Decision, Budget, Artifact,
   AuditEvent, ResultRevision and ResultReview records.
2. A runtime-native session or turn is correlated to an Attempt; it is not the product Task.
3. An environment provider controls workspace and process lifecycle. The product stores correlation and
   observed state without becoming a second compute controller.
4. Policy decisions and approvals are durable product records. Enforcement is mandatory for governed
   actions and occurs on the execution path through a qualified runtime hook or policy/tool gateway.
5. PostgreSQL and the transactional outbox are the initial durability substrate. A workflow engine is
   added only if failure-injection evidence shows that this design is insufficient.
6. Artifact bytes may live outside PostgreSQL, but artifact identity, provenance, integrity metadata and
   result association remain product records.
7. Runtime and environment events are observations until authenticated, deduplicated and accepted by the
   product lifecycle. Providers never write the product database.

## Provider contract semantics

[ADR-0003](../adr/0003-provider-contract-semantics.md) defines the behavior required at replaceable
provider boundaries:

- versioned `ControlFrame` lifecycle, request, cancel, reply and event frames with fail-fast compatible
  version and capability selection, typed failures and person/background demand hints;
- replayable `EventCursor` streams with generations, sequences, revisions, tombstones, heartbeats,
  cursor recovery, payload hashes and explicit unknown durability;
- runtime-session, runtime-turn, environment-resource and provider-operation bindings to an Attempt;
- environment observations, fenced maintenance claims and `off`/`shadow`/`live` harness qualification
  with durable migration passes, holds and preview/execute administration;
- scoped `AuthorityGrant` and auditable `AuthorityUse` semantics, including one-time retirement,
  standing-grant revocation, policy generations and permission ceilings;
- `HookRequest` and `HookResponse` interception for permission, input rewriting, messages, context,
  usage accounting and post-action evidence, with fail-closed pre-action enforcement and redacted or
  digest-only audit data;
- provider-neutral two-phase `SpendChallenge` authorization kept separate from authoritative Budget
  accounting;
- fenced `ArtifactCommitOutcome` admission with committed, retry and fenced results; and
- `ActionPrecondition` tokens plus serialized stateful actions.

These contracts refine adapter behavior; they do not change ownership, deployment or container
boundaries. A provider reports native state and effects. Only the control-plane application decides
whether those observations change authoritative product records.

## Initial implementation boundary

The first vertical slice is:

```text
submit task
  -> create and dispatch attempt
  -> start one runtime through one environment adapter
  -> persist progress and an attention request
  -> authorize and record a decision
  -> resume or cancel
  -> record artifacts and a result revision
  -> accept, reject or request rework
```

Use a deterministic runtime simulator before integrating a real coding agent. The simulator must expose
the same adapter contract, attention behavior, cancellation acknowledgement and event replay expected
from a production runtime. The simulator suite must also exercise protocol negotiation and failure,
cursor replay and rehydration, authority retirement and revocation, hook rewriting and denial, spend
challenge replay, environment and shadow behavior, artifact fencing and stale action preconditions.
Passing that suite proves contract behavior, not production-provider qualification.

M1 delivered this slice against the deterministic simulators: all nine authoritative records have
active contracts and PostgreSQL storage, and the
[contract-freeze gate](review-2026-10-15.md#contract-freeze-gate) is closed by the
[M1 contract freeze](contract-freeze-30.md). M2 makes the core reachable by surfaces and real
providers and deployable, without changing lifecycle ownership.

## Source decisions

- [ADR-0001: Go for the control-plane backend](../adr/0001-control-plane-language.md)
- [ADR-0002: start with a modular-monolith control plane](../adr/0002-initial-application-boundary.md)
- [ADR-0003: adopt provider-neutral execution contract semantics](../adr/0003-provider-contract-semantics.md)
- [ADR-0004: restricted migration administration and transfer evidence](../adr/0004-restricted-migration-administration.md)
- [ADR-0005: fenced harness migration restarts work with explicit evidence](../adr/0005-fenced-harness-migration.md)
- [ADR-0006: PostgreSQL and transactional outbox for the bounded M1 lifecycle](../adr/0006-sql-outbox-durability-decision.md)
- [ADR-0007: versioned HTTP Surface API authenticated by external OpenID Connect](../adr/0007-surface-api-transport-and-authentication.md)
- [ADR-0008: out-of-process provider adapters over an adapter-initiated ControlFrame connection](../adr/0008-adapter-process-boundary-and-wire-transport.md)
- [ADR-0009: one container image, migrated by an explicit step, on a reference Compose stack](../adr/0009-deployment-unit-and-first-substrate.md)
- [ADR-0010: open provider event streams with subscribe and unsubscribe frames](../adr/0010-controlframe-stream-subscription.md)
- [Exploration ADR-0007: SQL/outbox provisionally](../../architecture-explore/docs/adr/0007-product-durability-sql-outbox.md)
- [Exploration ADR-0010: one lifecycle owner per resource](../../architecture-explore/docs/adr/0010-one-lifecycle-owner-per-resource.md)
- [Exploration ADR-0013: no adopted control-plane foundation](../../architecture-explore/docs/adr/0013-control-plane-candidate-qualification.md)
- [Historical build specification](../../architecture-explore/docs/spec/SPEC.md), used as design input
  only where the active baseline adopts it

## Implementation notes

- [Artifact admission, immutable results and review/rework (#26)](artifact-results-26.md)
- [Recovery, replay, fencing and restart evidence (#27)](recovery-27.md)
- [Security and lifecycle acceptance (#28)](security-28.md)
- [Fenced harness migration execution (#60)](harness-migration-60.md)
- [M1 lifecycle contract freeze and evidence (#30)](contract-freeze-30.md)

## Change triggers

Revise these views when an accepted ADR changes lifecycle ownership, splits or combines a container,
selects the first runtime/environment integration, or introduces a deployment or trust boundary. Add a
separate deployment or security/data-flow view instead of overloading these diagrams.
