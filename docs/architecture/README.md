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
In particular, the historical specification's OCC/OpenClaw assumptions are not part of this baseline.

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
Those decisions are not yet made.

### Proposed containers

[Editable Draw.io source](container-view.drawio) · [Rendered SVG](container-view.svg) ·
[Rendered PNG](container-view.png)

Question answered: **What independently running software and stores form the first product?**

Audience: engineers planning the first implementation slice.

The first implementation uses one modular control-plane application, PostgreSQL and artifact storage.
API handling, lifecycle transitions, outbox dispatch, reconciliation and adapter orchestration begin as
modules of that application, not separate services. The web workspace is later and must use the same
public API as other surfaces. Runtime, environment and enforcement systems stay behind replaceable
interfaces. See [ADR-0002](../adr/0002-initial-application-boundary.md).

The diagram does not prescribe Kubernetes, a cloud, an event broker, Temporal, a particular agent
runtime or an environment provider.

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
from a production runtime.

The exploration schema is incomplete relative to this target: Budget, ResultRevision and ResultReview
still need authoritative tables and active contracts. The contract-freeze gate in the
[architecture review](review-2026-10-15.md#contract-freeze-gate) is the first implementation task.

## Source decisions

- [ADR-0001: Go for the control-plane backend](../adr/0001-control-plane-language.md)
- [ADR-0002: start with a modular-monolith control plane](../adr/0002-initial-application-boundary.md)
- [ADR-0007: SQL/outbox provisionally](../../architecture-explore/docs/adr/0007-product-durability-sql-outbox.md)
- [ADR-0010: one lifecycle owner per resource](../../architecture-explore/docs/adr/0010-one-lifecycle-owner-per-resource.md)
- [ADR-0013: no adopted control-plane foundation](../../architecture-explore/docs/adr/0013-control-plane-candidate-qualification.md)
- [Historical build specification](../../architecture-explore/docs/spec/SPEC.md), used as design input
  only where the active baseline adopts it

## Change triggers

Revise these views when an accepted ADR changes lifecycle ownership, splits or combines a container,
selects the first runtime/environment integration, or introduces a deployment or trust boundary. Add a
separate deployment or security/data-flow view instead of overloading these diagrams.
