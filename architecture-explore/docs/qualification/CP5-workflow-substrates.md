# CP5: Workflow-substrate control-plane screen

- Screened: 2026-10-14
- Result: **CLOSED — ALL THREE DISQUALIFIED AS PRIMARY CONTROL PLANES**
- Runtime tests: not run; native concepts and public APIs fail CP7 before installation

## Question

Do Temporal, Hatchet or Inngest natively own the authoritative delegated-work records required by the
product: task, attempt, attention request, decision, budget, artifact, audit event, result revision and
result review?

The answer is no. All three can execute code that implements those records, but none supplies that
product model. They are workflow execution infrastructure, not delegated-agent control planes.

## Versions and sources

| Candidate | Pinned release / commit | License observed | Primary sources |
|-----------|-------------------------|------------------|-----------------|
| Temporal | `v1.32.1` / `af08a5e5f2dd40f545f2afac7780daaa6987001e` | MIT | [Workflow execution](https://docs.temporal.io/workflow-execution), [event history](https://docs.temporal.io/workflow-execution/event), [message passing](https://docs.temporal.io/encyclopedia/workflow-message-passing), [visibility](https://docs.temporal.io/visibility) |
| Hatchet | `v0.110.5` / `56c617e0da399bc9d8a10d2ba74b4950f8a950b8` | MIT | [Platform concepts](https://docs.hatchet.run/v1), [TypeScript API](https://docs.hatchet.run/reference/typescript), [cancellation/replay](https://docs.hatchet.run/v1/bulk-retries-and-cancellations), [Temporal migration](https://docs.hatchet.run/v1/from-temporal-to-hatchet) |
| Inngest | `v1.46.0` / `879bc8e75f5f079be31f273d168ab4ee81724509` | SSPL-1.0 with time-delayed Apache-2.0 grant | [Platform overview](https://www.inngest.com/docs/platform-overview), [human approval pattern](https://www.inngest.com/docs/durable-execution/durable-agents/human-in-the-loop), [metadata](https://www.inngest.com/docs/reference/typescript/functions/metadata), [cancellation](https://www.inngest.com/docs/durable-execution/guides-and-advanced/cancellation) |

## Required-state mapping

`Native` means the platform supplies an authoritative record with semantics that the product can adopt.
`Primitive only` means our application must define, validate, persist and govern the record itself.

| Required product state | Temporal | Hatchet | Inngest |
|------------------------|----------|---------|---------|
| Task | Primitive only: Workflow Execution | Primitive only: workflow/task run | Primitive only: function run |
| Attempt | Primitive only: run/activity attempts | Primitive only: task attempts/replay | Primitive only: step attempts/retry |
| Attention request | Primitive only: application-defined Signal/Update and workflow state | Primitive only: application-defined event wait | Primitive only: application-defined `waitForEvent` and notification |
| Decision | No native authority-bound decision record | No native authority-bound decision record | No native authority-bound decision record; examples put responder fields in user events |
| Budget | No product budget ledger | Rate limits constrain throughput; they are not a per-task budget ledger | AI cost metadata and flow control are not an enforceable product budget ledger |
| Artifact | No product artifact/manifest record | Run output/logs are not governed artifacts | Run output/metadata are not governed artifacts; large data is stored externally by the application |
| Audit | Workflow Event History is execution history, not the product authority/audit model | Durable event log and task logs are execution history | Traces, events and metadata are execution history |
| Result | Workflow result, no result revision/review lifecycle | Run result, no result revision/review lifecycle | Function output/score, no result revision/review lifecycle |

## Candidate conclusions

### Temporal

Temporal provides the strongest generic durability mechanics of the three: durable workflow state,
replay, retries, timers, cancellation, Signals, Updates, Queries and append-only Event History. Its own
documentation says a Workflow Execution is the main execution unit and user-defined workflow code owns
its local state. Search Attributes and Visibility index executions; they do not introduce task,
approval, budget, artifact or result-review domain records.

Result: **FAIL CP7 as a primary control plane**. Using Temporal would still require building the complete
product state model and mapping it into workflow code.

### Hatchet

Hatchet provides persisted task/workflow runs, retries, replay, cancellation, durable event waits,
results, logs, rate limits and metadata. Its approval example explicitly models approval as an
application event correlated to a waiting durable task. Its migration guide states that synchronous,
validated workflow updates have no direct Hatchet equivalent and must be redesigned with events and
child tasks.

Result: **FAIL CP7 as a primary control plane**. Rate limits are useful execution controls but do not
become budgets, and run logs/results do not become authority-bound decisions, artifacts or reviewed
result revisions.

### Inngest

Inngest provides durable function runs, step attempts, retries, waits, cancellation, traces, metadata,
realtime updates, sessions and scores. Its human-approval guide requires the application to generate the
approval ID, notify the reviewer, authorize the HTTP endpoint, define the response event and interpret
approved/rejected/timed-out outcomes. That is an implementation recipe, not a native approval authority
or decision ledger. Its metadata documentation recommends external storage for large artifacts.

Result: **FAIL CP7 as a primary control plane**. Sessions, scores and AI-cost metadata add observability,
but they do not supply the complete product lifecycle or make Inngest its authoritative owner.

## Decision

Temporal, Hatchet and Inngest are removed from the primary control-plane candidate track. Installing
them as if one might reveal a hidden delegated-work model would be testing for its own sake; their
documented concepts and APIs already establish that the model must be built by the application.

This screen does not select any of them as infrastructure. PostgreSQL plus transactional outbox remains
the current substrate under ADR-0007. A later durability spike may compare workflow engines only if
failure-injection evidence proves SQL/outbox insufficient; that would be a separate infrastructure
decision, not a reopened control-plane qualification.
