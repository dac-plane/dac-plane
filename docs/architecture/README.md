# dac-plane architecture

Status: **proposed architecture baseline**

This directory defines the architecture to implement after the completed exploration in
[`architecture-explore/`](../../architecture-explore/README.md). The diagrams describe product
responsibility and proposed software containers. They do not claim that the product is implemented or
deployed.

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

The first implementation uses a small TypeScript control-plane service, a dispatcher/reconciler worker,
an execution adapter worker, PostgreSQL and artifact storage. The web workspace is a later container and
must use the same public API as other surfaces. Runtime, environment and enforcement systems stay behind
replaceable interfaces.

The diagram does not prescribe Kubernetes, a cloud, an event broker, Temporal, a particular agent
runtime or an environment provider.

## Ownership rules

1. `dac-plane` is authoritative for Task, Attempt, AttentionRequest, Decision, Budget, Artifact,
   AuditEvent, ResultRevision and ResultReview records.
2. A runtime-native session or turn is correlated to an Attempt; it is not the product Task.
3. An environment provider controls workspace and process lifecycle. The product stores correlation and
   observed state without becoming a second compute controller.
4. Policy decisions and approvals are durable product records. Enforcement occurs on the execution path
   through the runtime or a policy/tool gateway.
5. PostgreSQL and the transactional outbox are the initial durability substrate. A workflow engine is
   added only if failure-injection evidence shows that this design is insufficient.
6. Artifact bytes may live outside PostgreSQL, but artifact identity, provenance, integrity metadata and
   result association remain product records.

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

## Source decisions

- [ADR-0006: TypeScript and PostgreSQL](../../architecture-explore/docs/adr/0006-product-backend-stack.md)
- [ADR-0007: SQL/outbox provisionally](../../architecture-explore/docs/adr/0007-product-durability-sql-outbox.md)
- [ADR-0010: one lifecycle owner per resource](../../architecture-explore/docs/adr/0010-one-lifecycle-owner-per-resource.md)
- [ADR-0013: no adopted control-plane foundation](../../architecture-explore/docs/adr/0013-control-plane-candidate-qualification.md)
- [Build specification, sections 6-17 and 21-27](../../architecture-explore/docs/spec/SPEC.md)

## Change triggers

Revise these views when an accepted ADR changes lifecycle ownership, splits or combines a container,
selects the first runtime/environment integration, or introduces a deployment or trust boundary. Add a
separate deployment or security/data-flow view instead of overloading these diagrams.
