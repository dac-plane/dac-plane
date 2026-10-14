# ADR-0013: Qualify existing delegated-work control planes before building lifecycle machinery

- Status: Open — CanDoItAll process/workflow path remains to be qualified
- Spec: §35-§43
- Qualification: `docs/qualification/control-plane-candidates.md`

## Context

ADR-0012 rejects stock OpenClaw Enterprise/OCC for the current runtime dependency because the supported
dedicated Codex path did not pass the custom OpenAI-compatible provider route. That result disqualifies
one candidate foundation; it does not prove that the project should immediately build a complete
greenfield lifecycle controller.

The project still needs a universal delegated-agent control plane: one accountable work lifecycle across
surfaces, runtimes and execution environments. The product-owned layer owns task outcome, attempts,
attention, decisions, artifacts, budgets, audit, result review and portability. Runtimes and environments
are replaceable through adapters. Policy gateways, secret brokers and workflow/queue/storage systems are
supporting layers, not the product model. The open question is whether an existing delegated-work control
plane already covers enough of the lower lifecycle layer to adopt as a foundation or adapter target
without moving the product boundary.

## Decision

Before implementing additional lifecycle machinery, run a bounded qualification track for existing
control-plane candidates. The track begins with Paperclip and screens Kelos and AgentOven as alternative
foundations. OpenFlows, Temporal/Hatchet/Inngest and Prismor are classified separately to determine
whether they supply the product lifecycle or only a narrower component. A passing foundation must
preserve the product's platform-independent work contract; the product must not become tied to one
candidate's UI, storage model, runtime or environment.

The test must use the same bounded scenario for every candidate and record PASS/PARTIAL/FAIL against:

- install/reproducibility;
- custom OpenAI-compatible provider routing;
- durable delegated task/session state;
- approval authority and replay safety;
- result evidence and artifact mapping;
- cancellation and cleanup;
- mapping to the product task/attempt/attention/result schema;
- adapter portability across surfaces, runtimes and environments;
- preservation of the product/infrastructure split, so durable storage or workflow substrate remains an
  implementation detail rather than the public work contract;
- deployment fit.

## Consequences

- Do not build a custom scheduler, workflow engine, worker pool, sandbox fleet or approval platform until
  the candidate reports identify a real gap.
- Do not call a candidate adopted because its docs claim matching features. Adoption requires observed
  runtime evidence in `docs/qualification`.
- Paperclip and Kelos are closed. No primary-foundation candidate is adopted.
- CanDoItAll passed corrected custom provider routing across restart. CP4 and CP7 remain open because
  the direct agent-run path did not exercise the candidate's richer process/workflow authority model.
- If a candidate fails custom provider routing, reject it for this deployment path unless the supported
  configuration can route to OpenCodex/Camer without a fork.
- If a candidate cannot be isolated behind explicit adapters, keep it as an optional backend integration
  only. Do not let it become the product's control-plane model.

## Qualification outcome

The qualification track did not produce an adoptable primary foundation:

- Paperclip was installed and exercised, but managed Codex ignored the configured Camer Digital route
  and called OpenAI. It is closed and rejected for the current architecture; no retest is scheduled.
- Kelos was not installed. Requiring Kubernetes for the primary deployment conflicts with the selected
  deployment constraint. It is closed as a primary-deployment candidate and is not on the active path.
- AgentOven was screened at pinned tag `v0.9.0` and was not installed. Its effectively single-maintainer
  ownership, red CI, failed and mutable release handling, large low-cohesion changes, and dependence on a
  private Pro repository for production persistence/security/deployment make it unsuitable as the
  foundation at this stage.

OpenFlows is disqualified because its current controller, identity, model-routing and production
workspace topology require Coder, failing the hard portability gate. Temporal, Hatchet and Inngest are
disqualified as primary control planes because their native records are workflow/task/function runs,
attempts, waits, events, logs and outputs rather than the required delegated-work product model. Prismor
remains `NOT RUN` only as a narrower policy component.

A final bounded market scan rejected BoringOS and OCCP at the source-model gate, then advanced
CanDoItAll because its source contained durable process/execution records, HITL responses, approvals,
artifact ledgers, audit/history, recovery and non-Kubernetes deployment paths. The pinned runtime run
proved custom Camer Digital routing. The first restart result was invalid as a portability test because
it edited a bootstrap-managed OpenAI seed. A corrected, separately created Camer Digital profile retained
its endpoint and model and completed DeepSeek calls before and after restart.

The final qualification remains open for the process/workflow path. That run must decide whether its
external-request authority, lifecycle records, budgets, cancellation and result review map cleanly enough
to adopt behind the product contract. Existing runtimes and environment providers remain replaceable
adapters. PostgreSQL and an outbox remain the default substrate unless focused evidence proves otherwise.

## Non-goals

- This ADR did not adopt Paperclip, Kelos, AgentOven, CanDoItAll or any other candidate.
- This ADR does not select E2B, OpenSandbox, Daytona or another sandbox provider.
- This ADR does not reopen OCE qualification unless upstream fixes the dedicated Codex base-url probe or
  a supported configuration path is discovered.

