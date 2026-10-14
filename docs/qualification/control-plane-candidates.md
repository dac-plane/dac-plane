# Control-plane candidate qualification

Status: **closed — no candidate qualifies as the primary control-plane foundation**. CanDoItAll passed
custom-provider routing, approval authority, restart reconciliation and cancellation, but failed the hard
CP7 product-mapping gate because it has no authoritative task/attempt budget and enforcement contract.

## Why this exists

Q1 disqualified stock OpenClaw Enterprise/OCC for the current runtime path. The follow-up qualification
tested whether another existing delegated-work control plane could own enough of the lifecycle safely,
while this project kept the product-specific task, attention, result, evidence and portability model.

The question is now:

```text
Can an existing control plane run delegated agent work with our provider key, our approval rules,
our task/result record and deployable operational boundaries?
```

The answer from the completed scan is no. OpenFlows fails the environment-portability boundary because
its current control and production topology require Coder. Temporal, Hatchet and Inngest provide durable
execution primitives but not the authoritative delegated-work records the product requires. CanDoItAll
came closest: a separately created Camer Digital provider survived restart, its workflow authority model
passed, and its process runtime reconciled an interrupted attempt safely. It still lacks the product
task/attempt budget record and enforcement contract required by CP7, and therefore cannot be the primary
lifecycle owner.

## Product boundary that must not move

The product is a **universal delegated-agent control plane**: one accountable work lifecycle across
surfaces, runtimes and execution environments. It is not a wrapper around one provider, one coding agent
or one sandbox vendor.

```text
Surface interface
  Chat, Slack, IDE, tickets, dashboard, schedules or API submit work and respond to waits.

Product work contract
  Owns objective, owner, task, attempt, attention request, decision, budget, artifact, audit,
  result review, cancellation intent, retry/continue intent and portability across integrations.

Runtime adapter
  Starts or steers Codex, Claude Code, OpenCode, Cursor, OpenClaw or another tested runtime.
  Reports native events, approvals, artifacts and cancellation/continuation capability.

Environment adapter
  Provisions or attaches local machines, containers, Coder workspaces, Kubernetes, cloud VMs
  or managed sandboxes. Reports isolation, storage, secrets and cleanup state.

Policy and event interface
  Evaluates authority and budgets, records approval decisions and publishes lifecycle events.

Infrastructure substrate
  Stores durable product state, events, outbox messages, artifacts and logs. PostgreSQL and object
  storage exist now; Temporal, Hatchet or another workflow substrate is only added if a qualification
  report proves the SQL/outbox path is insufficient.
```

A candidate may replace or implement part of the lower layers only if it preserves that product work
contract. Passing a candidate test means "usable foundation or adapter target", not "the product becomes
that platform." Candidate-native terms such as issue, task, session, workspace, run, recipe, worker pool
or approval must map back to the product contract without making the product state dependent on that
platform's UI or storage semantics.

The distinction matters for candidate interpretation:

| Layer | Product ownership | Candidate may manage |
|-------|-------------------|----------------------|
| Surface | Stable task intake, progress, response and result APIs | A connector or UI surface, if replaceable |
| Product control plane | Task/attempt/attention/decision/budget/artifact/audit/result state | Only if it maps cleanly and does not become platform-locked |
| Runtime | Runtime capability mapping and event normalization | Native agent execution, turns, tool calls and runtime-specific cancellation |
| Environment | Environment binding and cleanup intent | Workspace/sandbox/cluster/VM provisioning, isolation and process execution |
| Policy / secret / tool gateway | Product authority, approval records and grant semantics | Enforcement hooks, brokered credentials, native tool-call gates |
| Infrastructure substrate | Durable product records and event publication | Queue/workflow/storage mechanics when they remain implementation details |

## Candidate disposition and order

| Order | Candidate | Disposition | Why test it / risk to prove or reject |
|-------|-----------|-------------|---------------------------------------|
| 1 | Paperclip | **CLOSED — REJECTED** | The tested release did not propagate the configured custom provider into managed Codex execution. This fails a hard provider-neutrality gate. There is no scheduled retest. |
| 2 | Kelos | **CLOSED — REJECTED FOR PRIMARY DEPLOYMENT** | Kubernetes-native Tasks, Sessions, Workspaces and WorkerPools require Kubernetes/cert-manager. The primary product deployment must not require Kubernetes. It is not on the active implementation path. |
| 3 | AgentOven | **CLOSED — REJECTED** | The repository is too immature to become the foundation: effectively one maintainer, red CI, failed and mutable release handling, large low-cohesion files, and production persistence/security/deployment components in the private Pro repository. See `CP3-agentoven.md`. |
| 4 | OpenFlows | **CLOSED — DISQUALIFIED BY CODER DEPENDENCY** | Its current controller, identity, model-routing and production workspace topology require Coder. This fails the hard CP8 portability gate. See `CP4-openflows.md`. |
| 5 | Temporal/Hatchet/Inngest | **CLOSED — DISQUALIFIED AS PRIMARY CONTROL PLANES** | Their native models are workflow/function/task runs, attempts, waits, events, logs and outputs. They do not supply authoritative attention, decision, budget, artifact, audit and result-review records. See `CP5-workflow-substrates.md`. |
| 6 | BoringOS / OCCP / CanDoItAll final scan | **CLOSED — ALL REJECTED** | BoringOS and OCCP fail approval/state mapping. CanDoItAll passes CP2, CP4, CP6 and CP8 but fails hard gate CP7 because loop budgets and observed cost do not supply an authoritative task/attempt budget. It also fails current CP9 deployment fit. See `CP6-final-market-scan.md`. |
| 7 | Prismor | **NOT RUN — POLICY COMPONENT CANDIDATE** | It is proposed for policy/tool-call enforcement rather than lifecycle ownership. Its fit has not been tested. |

Sources used for the initial screen:

- Paperclip docs: https://docs.paperclip.ing/
- Paperclip approvals API: https://docs.paperclip.ing/reference/api/approvals/
- Paperclip tool gateway API: https://docs.paperclip.ing/reference/api/tool-gateway/
- Kelos repository/docs: https://github.com/kelos-dev/kelos
- AgentOven architecture: https://docs.agentoven.dev/architecture
- AgentOven recipes: https://docs.agentoven.dev/recipes
- OpenFlows concepts: https://openflows.dev/docs/concepts/
- Temporal workflow execution: https://docs.temporal.io/workflow-execution
- Hatchet concepts: https://docs.hatchet.run/v1
- Inngest platform overview: https://www.inngest.com/docs/platform-overview
- CanDoItAll source and API control plane: https://github.com/fyziktom/CanDoItAll
- BoringOS source: https://github.com/hebbs-ai/boringos
- OCCP source: https://github.com/azar-management-consulting/occp-core
- Prismor site: https://www.prismor.dev/

These sources supported the screens. The evidence-backed dispositions and exact pinned versions are
recorded in the individual CP1, CP3, CP4, CP5 and CP6 reports.

## What the completed qualifications prove

The completed qualifications show what the rejected tools do not supply. CanDoItAll demonstrated more
of the lifecycle than the other candidates, including scoped decisions and safe restart reconciliation,
but it did not supply the complete product work contract. The project must implement:

- authoritative task and attempt transitions, including retry, continue and cancellation intent;
- durable attention requests and approval decisions with authority, expiry and replay-safe binding;
- budgets, artifacts, audit events, result revisions and result review;
- idempotent command handling, transactional event/outbox publication and restart reconciliation;
- runtime and environment adapter contracts, normalized events and capability checks.

The project does not need to build model inference, a coding-agent runtime, a sandbox/VM platform, a
general workflow engine or a general policy gateway. Those remain replaceable implementation components
and are qualified only when a concrete integration requires one.

The sequence is now:

1. freeze the stable product work contract and state transitions;
2. implement the minimum product-owned lifecycle core and outbox-backed command handling;
3. integrate one runtime and one environment through replaceable adapters; and
4. reuse proven candidate mechanisms only where they remain subordinate to that contract.

## Required proof for any candidate

A candidate passes only if it runs the same bounded scenario and produces inspectable evidence.

### Scenario

Use a throwaway repository and a non-destructive task:

```text
Read the repository README and one source file. Create a short evidence file in a branch or workspace.
Before writing the file, request approval with the exact proposed file path and content summary.
After approval, write the file, run one harmless verification command, publish the result and stop.
```

The candidate must use the Camer Digital/OpenCodex route or another explicitly configured
OpenAI-compatible endpoint, not a hidden default OpenAI key. The test must record the exact model,
base URL setting, credential injection path and whether the value is visible to the agent process.

### Pass gates

| Gate | Requirement | PASS means |
|------|-------------|------------|
| CP1 install | Candidate installs locally or in an isolated dev cluster from a pinned version. | Operator can reproduce setup without manual state hidden outside documented config. |
| CP2 provider routing | Candidate runs one model call through the configured OpenAI-compatible endpoint. | Logs or probe prove the request goes to the configured base URL, not a default vendor endpoint. |
| CP3 delegated task | Candidate creates a durable work item/session/task and runs the bounded scenario. | Work survives long enough to inspect state, logs and result after process/UI refresh. |
| CP4 approval authority | Candidate blocks before the write and resumes only after the intended approval. | Approval record includes requester, responder/authority, action identity, expiry or invalidation behavior. |
| CP5 result evidence | Candidate exposes artifact, logs/events, model/provider metadata and final outcome through API/CLI. | Product can map native evidence to ResultRevision and Artifact without scraping a UI only. |
| CP6 cancellation/cleanup | Cancel or timeout stops work and leaves no active orphan worker/session. | Remaining resources are inspectable and either gone or marked for explicit cleanup. |
| CP7 product mapping | Candidate state can map cleanly to task, attempt, attention request, decision, budget, artifact, audit event and result review. | No dual owner conflict with the product schema. |
| CP8 adapter portability | Candidate can be used through explicit APIs/adapters without hard-coding one surface, runtime or environment into the product model. | A second runtime or environment can be modeled without changing product task semantics. |
| CP9 deployment fit | Candidate can be self-hosted or operated in the target environment with acceptable secrets, auth and upgrade story. | Real deployment path is plausible; local-only demos do not pass. |

Any FAIL in CP2, CP4, CP6, CP7 or CP8 disqualifies the candidate as the primary control-plane foundation.
It may still be reused as a narrow adapter or component.

## First run: Paperclip

Paperclip is the first test because its public docs claim the broadest overlap with the project:
control-plane API, agents, tasks/issues, approvals, costs/budgets, adapters, deployment and activity.
The first run is not a full integration. It is a fast disqualification or fit check:

1. Pin the Paperclip source/version and license.
2. Install it locally or in a disposable environment following its deployment docs.
3. Configure an agent adapter for Codex or HTTP/custom runtime using the OpenCodex/Camer route.
4. Run the bounded scenario.
5. Export API/CLI evidence for task state, approval state, budget/cost record, artifact/result and cleanup.
6. Write `docs/qualification/CP1-paperclip.md` with PASS/PARTIAL/FAIL for CP1-CP9.

Do not build a product integration until this report exists.

## Second screen: Kelos

Result: **CLOSED — REJECTED FOR PRIMARY DEPLOYMENT**.

Kelos requires Kubernetes and cert-manager. The primary product deployment must not require Kubernetes,
so installing Kelos would test an architecture the project has not selected. This is a deployment-fit
disqualification, not evidence that Kelos is technically defective.

Kelos is not on the active implementation path. A future architecture that deliberately selects
Kubernetes could make a new decision about using it as an environment adapter, but this qualification
does not retain it as a current option.

## Third screen: AgentOven

Result: **CLOSED — REJECTED BY SOURCE SCREEN**.

The source audit pinned `v0.9.0` and stopped before installation or provider-key use. AgentOven is active,
but activity is concentrated in one maintainer and unusually large commits. The pinned source has red CI,
its release workflow failed, the same release tag was moved while release fixes were attempted, and the
public server does not provide the PostgreSQL-backed production state implied by its local deployment
path. The durable PostgreSQL store, stronger identity controls and production topology live in the
separate private Pro repository; its documented production deployment is Kubernetes-based.

This is enough to fail foundation maturity and deployment fit without spending a Camer Digital key or
running the bounded task. The full evidence and archived screening criteria are in
`docs/qualification/CP3-agentoven.md`.

## Current decision

After the Paperclip and CanDoItAll runtime tests, Kelos deployment screen, AgentOven source screen,
OpenFlows architecture screen and workflow-substrate API/state-model screen:

- No candidate is adopted as the primary foundation.
- The broad exploratory search and bounded CanDoItAll qualification are closed.
- Do not repeat the OCE/Paperclip mistake: any runtime adapter must prove supported custom-provider routing
  before integration.
- Choose one runtime and one managed-sandbox/environment backend behind replaceable adapters; do not use
  an environment provider as a substitute for product lifecycle state.
- Evaluate a sandbox provider under ADR-0008 only when the environment adapter reaches that integration
  gate. E2B or OpenSandbox would be an environment component, not a replacement for the product control
  plane.

CanDoItAll passes CP4, CP6 and CP8. Its rejection is not based on the invalid managed-seed restart run or
on Camer Digital: the supported custom profile and process calls worked. It is rejected because CP7 lacks
an authoritative task/attempt budget contract, and its current release/security/operations posture also
fails CP9 for foundation adoption.

## Evidence standard

Every run must include:

- Candidate name, source URL, commit/tag/version, license and install date.
- Host or cluster details.
- Full non-secret configuration shape.
- Provider/model/base URL proof.
- Commands/API calls used.
- Native IDs for task/session/workspace/approval/artifacts.
- Logs or screenshots only when an API/CLI record is unavailable.
- Cleanup command and observed cleanup result.
- Explicit product mapping table.
- Explicit adapter boundary: what the candidate owns, what the product owns and what remains replaceable.

