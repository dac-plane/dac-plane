# ADR-0013: Qualify existing delegated-work control planes before building lifecycle machinery

- Status: Proposed
- Spec: §35-§43
- Qualification: `docs/qualification/control-plane-candidates.md`

## Context

ADR-0012 rejects stock OpenClaw Enterprise/OCC for the current runtime dependency because the supported
dedicated Codex path did not pass the custom OpenAI-compatible provider route. That result disqualifies
one candidate foundation; it does not prove that the project should immediately build a complete
greenfield lifecycle controller.

The project still needs a control-plane layer that owns task outcome, attempts, attention, decisions,
artifacts, budgets, audit and portability across surfaces. The open question is whether an existing
delegated-work control plane already covers enough of that layer to adopt or adapt.

## Decision

Before implementing additional lifecycle machinery, run a bounded qualification track for existing
control-plane candidates. Test Paperclip first, then Kelos if Paperclip is rejected or insufficient.
AgentOven, OpenFlows, Temporal/Hatchet/Inngest and Prismor remain secondary candidates or components,
not first-run foundations.

The test must use the same bounded scenario for every candidate and record PASS/PARTIAL/FAIL against:

- install/reproducibility;
- custom OpenAI-compatible provider routing;
- durable delegated task/session state;
- approval authority and replay safety;
- result evidence and artifact mapping;
- cancellation and cleanup;
- mapping to the product task/attempt/attention/result schema;
- deployment fit.

## Consequences

- Do not build a custom scheduler, workflow engine, worker pool, sandbox fleet or approval platform until
  the candidate reports identify a real gap.
- Do not call a candidate adopted because its docs claim matching features. Adoption requires observed
  runtime evidence in `docs/qualification`.
- If Paperclip passes, integrate it as the first foundation or adapter target and reduce the product
  backlog to the missing product semantics.
- If Paperclip fails but Kelos passes, use Kelos as the Kubernetes-native execution foundation and keep
  product task state separate unless Kelos state maps cleanly.
- If both fail on lifecycle ownership, build the project-owned control-plane core and reuse only narrower
  runtime/sandbox/policy components.
- If a candidate fails custom provider routing, reject it for this deployment path unless the supported
  configuration can route to OpenCodex/Camer without a fork.

## Non-goals

- This ADR does not adopt Paperclip, Kelos, AgentOven or any other candidate.
- This ADR does not select E2B, OpenSandbox, Daytona or another sandbox provider.
- This ADR does not reopen OCE qualification unless upstream fixes the dedicated Codex base-url probe or
  a supported configuration path is discovered.

