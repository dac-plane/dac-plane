# Control-plane candidate qualification

Status: **planned**. No candidate below is adopted until it passes the bounded runtime test in this
document.

## Why this exists

Q1 disqualified stock OpenClaw Enterprise/OCC for the current runtime path. That does not mean the
project should immediately build every control-plane function itself. It means the next qualification
must test whether an existing delegated-work control plane can own enough of the lifecycle safely,
while this project keeps the product-specific task, attention, result, evidence and portability model.

The question is now:

```text
Can an existing control plane run delegated agent work with our provider key, our approval rules,
our task/result record and deployable operational boundaries?
```

If yes, integrate it as a foundation or adapter target. If no, build only the missing product-owned
control-plane core and use existing sandbox/runtime services as backends.

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
| 1 | Paperclip | **FAIL / WATCH** | Closest documented match to the delegated-work layer, but the tested release did not propagate the configured custom provider into managed Codex execution. Retest only after the trigger recorded in `CP1-paperclip.md`. |
| 2 | Kelos | **NOT RUN — DISQUALIFIED BY DEPLOYMENT CONSTRAINT** | Kubernetes-native Tasks, Sessions, Workspaces and WorkerPools require Kubernetes/cert-manager. The primary product deployment must not require Kubernetes. Retain Kelos only as a possible future Kubernetes environment adapter. |
| 3 | AgentOven | **NEXT** | Docs and source expose a control-plane server, registry, recipe engine, task router, sessions, A2A/MCP and human gates. Prove that the standalone server/container path is operable without Kubernetes and that its records map to coding-agent work, artifacts and approval authority. |
| 4 | OpenFlows | **DEFERRED** | Useful only if Coder becomes the selected environment; it is not the universal product control plane by itself. |
| 5 | Temporal/Hatchet/Inngest | **FALLBACK SUBSTRATE** | Durable workflow substrate if no agent-native candidate passes. These do not provide agent workspace/runtime/sandbox semantics and would support a product-owned control-plane core. |
| 6 | Prismor | **OPTIONAL COMPONENT** | Policy, approval and audit layer for tool calls, not a lifecycle owner for delegated work. |

Sources used for the initial screen:

- Paperclip docs: https://docs.paperclip.ing/
- Paperclip approvals API: https://docs.paperclip.ing/reference/api/approvals/
- Paperclip tool gateway API: https://docs.paperclip.ing/reference/api/tool-gateway/
- Kelos repository/docs: https://github.com/kelos-dev/kelos
- AgentOven architecture: https://docs.agentoven.dev/architecture
- AgentOven recipes: https://docs.agentoven.dev/recipes
- OpenFlows concepts: https://openflows.dev/docs/concepts/
- Prismor site: https://www.prismor.dev/

These sources are not adoption evidence. They only justify which candidate to test first.

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

Result: **NOT RUN — DISQUALIFIED BY DEPLOYMENT CONSTRAINT**.

Kelos requires Kubernetes and cert-manager. The primary product deployment must not require Kubernetes,
so installing Kelos would test an architecture the project has not selected. This is a deployment-fit
disqualification, not evidence that Kelos is technically defective.

Kelos may be reconsidered later as an optional Kubernetes environment adapter. It must not own the
portable product task, attention, result or audit contract.

## Third run: AgentOven

AgentOven is the next bounded control-plane qualification. Pin the current source tag and test its
standalone server/container path with PostgreSQL; Kubernetes is not permitted in the qualification
deployment. Docker Compose may be used to reproduce the test, but its existence alone does not pass the
production deployment-fit gate.

The run must prove all general gates above and these candidate-specific points:

1. configure Camer Digital through AgentOven's provider `base_url` and prove the request reaches that
   endpoint rather than a default vendor endpoint;
2. invoke a coding-capable external or custom runtime without making AgentOven's built-in agent loop the
   product contract;
3. persist the delegated task/run, session, human gate, decision, logs, artifact and final outcome through
   a control-plane restart;
4. bind the human gate to the exact proposed write and reject stale or mismatched approval;
5. cancel or time out the run and prove that no delegated process or environment remains active;
6. show a credible non-Kubernetes deployment, secret-injection and upgrade path for an ordinary VM or
   container host.

Write `docs/qualification/CP3-agentoven.md` after the run. Do not build a product integration until that
report exists.

## Decision rule after two runs

After the executed Paperclip and AgentOven runs:

- If one candidate passes CP1-CP8, create an ADR adopting it as the qualified foundation or adapter
  target and reduce the product backlog to the remaining gaps.
- If both fail only on provider routing, do not repeat the OCE mistake: reject stock adoption unless the
  candidate exposes a supported base-url/provider configuration path.
- If both fail on product lifecycle ownership, keep the project-owned control-plane core and choose one
  runtime plus one managed-sandbox/environment backend.
- If both fail only on sandbox/runtime isolation, evaluate one sandbox provider under ADR-0008. E2B or
  OpenSandbox is an environment candidate, not a replacement for the product control plane.
- If a candidate passes execution but fails adapter portability, keep it as one backend integration only;
  do not let it define the product control-plane state.

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

