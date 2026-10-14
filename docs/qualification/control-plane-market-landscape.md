# Delegated-agent control-plane market landscape

- Research date: 2026-10-14
- Status: **COMPLETE — no production-ready horizontal lifecycle foundation identified**
- Decision authority: [ADR-0013](../adr/0013-control-plane-candidate-qualification.md)
- Detailed candidate evidence: [control-plane-candidates.md](control-plane-candidates.md)

## Question answered

The qualification reports rejected the candidates that appeared able to own this product's control
plane. Before implementation begins, this report answers the remaining market question:

```text
Are companies already using or selling a production-ready control plane that owns
task, attempt, attention, decision, budget, artifact, audit and result-review state
across replaceable agent runtimes and execution environments?
```

The public evidence reviewed here says **no**. Companies do build substantial control planes, but the
market separates into different products that use the same name for different responsibilities:

1. Coding-agent companies own a complete work lifecycle for their own agent runtime.
2. Horizontal governance products manage agent identity, access, tools, policy and telemetry, but do
   not become the authoritative delegated-work lifecycle.
3. Workflow and environment platforms provide durable execution or compute lifecycle, not the product
   work contract.
4. Early open-source projects approach the required horizontal lifecycle model, but none meets the
   product's maturity, budget and deployment gates.

The conclusion is not that every layer must be written here. The product must own the small,
authoritative lifecycle core. Agent runtimes, environments, policy systems and durable infrastructure
remain replaceable integrations.

## Evidence standard and limits

This is a source and API landscape review, not a runtime qualification. Product behavior is described
as documented by each vendor. It does not prove undocumented internals, production reliability or a
capability that is not exposed through a supported interface.

The review used official product documentation and public source where available. Public evidence can
establish that a vendor exposes sessions, tasks, approvals, artifacts or policy. It cannot establish
the vendor's private database schema, workflow engine or internal service topology. In particular,
Grok Bot's internal lifecycle implementation remains unknown and is not used as evidence for a build or
adoption decision.

Confidence is **high** in the category boundaries and **moderate** in market exhaustiveness. Enterprise
features can be private, and the market changes quickly. A newly proposed foundation must still pass
CP1-CP9 rather than relying on this report.

## Category 1: vertical delegated-work control planes

The strongest coding-agent products already persist and operate task-like work. They provision or bind
an environment, run an agent, preserve progress, accept steering or decisions, collect evidence and
hand work back for review. Their product record is nevertheless a session or task owned by that vendor's
agent runtime.

| Product | Documented lifecycle ownership | Boundary that prevents foundation adoption |
|---------|--------------------------------|---------------------------------------------|
| [Cursor Cloud Agents](https://prod.cursor.com/docs/cloud-agent/security) | Start, provision, run, persist conversation and artifacts, hand off a branch/draft PR, then recycle the VM. | The agent loop and authoritative run remain Cursor-managed. [Self-hosted execution](https://cursor.com/docs/cloud-agent/self-hosted/choose-runtime) moves tool execution, not the agent control plane, into customer infrastructure. |
| [OpenAI Codex](https://openai.com/index/introducing-codex/) | Parallel tasks in isolated sandboxes with progress, results, review and PR/diff handoff. | The work unit is a Codex task. No supported provider-neutral lifecycle service is exposed for arbitrary agent runtimes. |
| [GitHub Copilot coding agent](https://docs.github.com/en/copilot/how-tos/copilot-on-github/use-copilot-agents/manage-and-track-agents) | Sessions, ephemeral environments, progress, token usage, logs, steering, stopping and signed commits linked to session evidence. | The lifecycle is integrated with Copilot, GitHub Actions and GitHub pull requests. GitHub documents that steering is unavailable for third-party coding agents. |
| [Google Jules](https://jules.google/docs/api/reference/types/) | A public Session resource represents a unit of work; Activities carry plans, approvals, progress, messages, failures and artifacts. | The API is clean but executes Jules. It is not an authority over Codex, Claude Code or another replaceable runtime. |
| [Claude Code cloud sessions](https://code.claude.com/docs/en/claude-code-on-the-web) | Persistent cloud sessions, queued messages, questions, permission modes, parallel work, review, PR monitoring, archive and deletion. | Cloud sessions require Anthropic account authority and are not available through third-party providers. Self-hosted environments change execution placement, not session ownership. |
| [Devin](https://docs.devin.ai/api-reference/overview) | Session lifecycle, reviews, environments, automations, schedules, queue, RBAC, audit logs, guardrail violations and consumption records. | The authoritative work record is a Devin session and the executor is Devin. Its breadth makes it a useful product reference, not a neutral lifecycle owner. |
| [Factory](https://docs.factory.com/api-reference/sessions) | Droid sessions, computers, environments, messages, permissions, artifacts, usage limits and enterprise controls. | Factory supports custom model routes and [cloud, hybrid and air-gapped deployment](https://docs.factory.com/enterprise/network-and-deployment), but the controlled runtime is Droid. Factory's [headless interface](https://docs.factory.com/droid-exec/overview) explicitly leaves external queues, persistence, results and policy orchestration to the integrating product. |
| [Replit Agent](https://replit.com/blog/whats-changed-agent3-to-agent4) | Planned and active tasks, isolated project copies, parallel execution, dependency handling, review and explicit application to the main project. | Task state, environment state and acceptance are part of the Replit project and Agent product. |

### Finding

These companies did not treat task state, waits, evidence and review as generic features of a model API
or sandbox. They built those semantics into their products. This validates the need for the layer; it
does not supply a reusable implementation of it.

Factory is the closest commercial runtime component because it supports several model providers,
customer-controlled compute and a substantial session API. It remains a **runtime-adapter candidate**,
not the product lifecycle, because replacing Droid would replace the record Factory controls.

## Category 2: horizontal governance control planes

Horizontal products can span agents from multiple vendors, but their authoritative resource is the
agent identity, connection, policy or telemetry stream—not the delegated work item and its result.

| Product | What it controls horizontally | Missing product authority |
|---------|-------------------------------|---------------------------|
| [Microsoft Agent 365](https://learn.microsoft.com/en-us/microsoft-agent-365/connect-existing-agents) | Registry, ownership, Entra identity, access, security, compliance and telemetry for Microsoft and external agents. | Its documented [Run](https://learn.microsoft.com/en-us/microsoft-agent-365/developer/observability-concepts) is an observability trace for one user message and reply. It does not own task attempts, attention decisions, cancellation intent, budgets and result acceptance. |
| [Runlayer](https://www.runlayer.com/platform) | Cross-client discovery, MCP/tool access, identity, scoped credentials, real-time policy, runtime security, usage and audit. | It governs interactions from Cursor, Codex, Claude Code and other clients. Native hosted agents have a Runlayer-owned runtime; third-party client sessions do not become one portable delegated-work lifecycle. |
| [ryanwi/agent-control-plane](https://github.com/ryanwi/agent-control-plane) | Policy evaluation, action proposals, approvals, identity-scoped budgets, kill switches, event storage and session recovery. | It is a governance library around sessions and proposed actions. It does not expose the complete task/result/review aggregate required by CP7. |

### Finding

This category may prevent the product from having to build a general policy gateway, enterprise agent
registry, credential broker or telemetry platform. It cannot decide whether the product's task is
complete, which attempt owns the result, whether a decision is current, or whether the user accepted a
result revision.

## Category 3: durable execution and environment components

The earlier qualification reports already established this boundary:

- [Temporal, Hatchet and Inngest](CP5-workflow-substrates.md) can persist executions, attempts, waits,
  retries, events and outputs. They do not provide the complete delegated-work record. They remain
  optional infrastructure below the product model.
- E2B, Daytona, OpenSandbox, Coder, Kubernetes and VM/container systems can own workspace, process,
  isolation and cleanup state. They are environment implementations, not task and result authorities.
- Model providers and coding-agent CLIs own inference and native agent sessions. A runtime adapter maps
  their capabilities and events without transferring the product task identity to them.

Using one of these systems can reduce implementation and operational work. It does not remove the need
for the product lifecycle aggregate.

## Category 4: open-source horizontal lifecycle attempts

### Agent Control Center Core

[Agent Control Center Core](https://github.com/trietphan/agent-control-center-core) is the closest public
match found during this landscape review. Its source defines:

- durable task, run, artifact, message and review records;
- explicit run attempts and state transitions;
- Codex, Claude and HTTP/OpenClaw adapters;
- SQLite coordination, process supervision and restart recovery;
- independent verification and evidence hashes;
- compare-and-swap review decisions bound to an evidence revision; and
- an adapter protocol, conformance runner, CLI, loopback API and MCP bridge.

It is useful evidence that a provider-neutral work contract and adapter architecture are technically
coherent. It is not an acceptable foundation today:

- the project labels itself alpha and its package is `0.1.0-alpha.2`;
- the repository had one listed maintainer, zero stars and zero forks when inspected;
- the public core explicitly excludes customer authentication, billing, multi-tenant operations and a
  managed dashboard;
- its roadmap says stable task/adapter/evidence contracts, resilience conformance and reviewed security
  boundaries are still future release gates; and
- it records provider usage after execution but has no authoritative task/attempt budget reservation or
  pre-execution enforcement contract.

Adopting or forking it would therefore transfer unfinished product and operational responsibility to
this project. It can be studied as a design reference and compared with the product schema, but it does
not change the build decision.

### HumanLayer Agent Control Plane

[HumanLayer ACP](https://github.com/humanlayer/agentcontrolplane) provides Kubernetes-native durable
tasks, tool-call approvals and multi-provider support. Its documented design deliberately uses the
rolling model context with “no separate execution state.” Its roadmap still marks tiered approvals,
token telemetry, UI and CLI support as planned or in progress. The Kubernetes requirement also conflicts
with the selected primary deployment boundary. It is not a foundation candidate.

## Required product core after the market scan

The market scan narrows the build. The product must own these records and transitions because no
qualified component owns them without also taking ownership of the runtime or product boundary:

```text
Task
  objective, owner, criteria, status and current intent

Attempt
  runtime/environment bindings, capability snapshot, lease and outcome

AttentionRequest -> Decision
  exact action or question, authority, expiry, revision and replay-safe consumption

Budget
  authoritative limits, reservations, measured consumption and enforcement decisions

Artifact + AuditEvent
  immutable evidence identity, provenance and ordered lifecycle history

ResultRevision -> ResultReview
  proposed outcome, verification evidence and accept/rework/reject decision
```

The core also owns idempotent commands, retry/continue/cancel intent, uncertain-effect reconciliation
and transactional event/outbox publication.

The product does **not** need to build:

- a model or inference provider;
- a new coding-agent reasoning loop;
- a general sandbox, VM or Kubernetes platform;
- a general workflow engine;
- a general enterprise identity, policy or MCP gateway; or
- a replacement for Git, object storage, PostgreSQL or observability systems.

## Integration position

The implementation boundary after this review is:

```text
Surface interface
  Web, chat, Slack, IDE, tickets, schedules and API

Product-owned lifecycle core
  Task, Attempt, AttentionRequest, Decision, Budget,
  Artifact, AuditEvent, ResultRevision and ResultReview

Runtime adapters
  Codex, Claude Code, Factory Droid, OpenCode, Cursor or another qualified runtime

Environment adapters
  Local process, container, managed sandbox, VM, Coder workspace or Kubernetes

Optional horizontal components
  Agent identity, policy/tool gateway, secret broker and telemetry platform

Infrastructure substrate
  PostgreSQL, transactional outbox, object storage and optional workflow engine
```

One layer may call another, but no integration may become a second authority for the same product
record. Runtime-native session state and environment-native compute state are mappings on an Attempt,
not replacements for the Task aggregate.

## Decision

The foundation search remains closed. No reviewed product or project qualifies as the primary
provider-neutral delegated-work lifecycle owner.

Proceed with the minimum product-owned lifecycle core defined above. Keep every runtime, environment,
policy and infrastructure choice behind an explicit adapter or implementation boundary. Reopen the
foundation decision only when a candidate demonstrates the complete CP1-CP9 contract through supported
interfaces and without making one agent runtime or deployment platform authoritative for product state.
