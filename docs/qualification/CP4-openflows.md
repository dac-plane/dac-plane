# CP4: OpenFlows control-plane qualification

- Screened: 2026-10-14
- Source: https://github.com/The-AgenticFlow/openflows
- Pinned release: `openflows-1.7.1`
- Pinned commit: `02de659b105fef077f1fc60d85df3b1a7aa3d730`
- License: MIT
- Result: **CLOSED — DISQUALIFIED BY CODER DEPENDENCY**
- Runtime/provider test: not run

## Decision

Do not install or integrate OpenFlows as the universal product control plane. Its current architecture is
deliberately built on Coder rather than exposing Coder as one replaceable environment adapter. That
fails the hard CP8 portability gate before a runtime test would add useful evidence.

This is a product-fit rejection, not a claim that OpenFlows lacks lifecycle machinery. It has ticket
state, retries, recovery, human escalation, reviews and workspace cleanup. The problem is that those
semantics are embedded in a Coder- and GitHub-specific autonomous software-delivery pipeline.

## Evidence

OpenFlows describes itself as a self-hosted orchestration layer for autonomous development teams “on
top of Coder”:

- https://openflows.dev/docs/
- https://openflows.dev/docs/architecture/system-design/
- https://openflows.dev/docs/architecture/orchestrator/
- https://openflows.dev/docs/reference/environment-variables/

The documented production path has these dependencies:

- NEXUS runs inside a dedicated per-tenant Coder workspace;
- worker execution provisions ephemeral Coder workspaces through the Coder API;
- model access uses Coder AI Gateway and GitHub identity uses Coder external auth;
- `CODER_URL` and `CODER_SESSION_TOKEN` are required orchestrator configuration;
- the native work lifecycle is GitHub issue to plan, implementation, review, pull request and merge.

The source documents a `WorkspaceTransport` abstraction and a local worktree mode. That does not remove
the product dependency: the documented controller, credentials, production workspace fleet and identity
topology still make Coder part of the control plane rather than one optional environment implementation.

## Gate result

| Gate | Result | Evidence |
|------|--------|----------|
| CP1 install | NOT RUN | Installation cannot repair the architectural mismatch. |
| CP2 provider routing | NOT RUN | Coder AI Gateway/LiteLLM routing exists, but CP8 already fails. |
| CP3 delegated task | DOCUMENTED | Ticket lifecycle and agent roles are documented; not counted as runtime PASS. |
| CP4 approval authority | DOCUMENTED | Planning gates and human escalation exist; authority binding was not run. |
| CP5 result evidence | DOCUMENTED | Plans, reviews and pull requests are native to the software-delivery flow. |
| CP6 cancellation/cleanup | DOCUMENTED | Workspace teardown and reconciliation are documented. |
| CP7 product mapping | FAIL | The lifecycle is specialized around GitHub tickets, coding roles, pull requests and merge. |
| CP8 adapter portability | **FAIL** | Current control, identity, model-routing and production workspace topology require Coder. |
| CP9 deployment fit | FAIL | Adopting it makes Coder a primary product dependency rather than an optional environment adapter. |

CP8 is a hard disqualification gate. No OpenFlows runtime qualification is scheduled.
