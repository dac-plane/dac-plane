# Responsibility map: what OCC and the runtime cover vs. product gaps

Basis: OCE docs at `07590c64` (harness-execution.md, gateway-routing.md, api cheat sheet, first-agent.md)
and OpenClaw exec-approvals docs. Documented unless marked observed. Nothing below was observed end-to-end.

| Responsibility (spec §7) | OCC (OCE) | Runtime (OpenClaw Gateway + Codex) | Product gap (ours) |
|---|---|---|---|
| Agent deployment, immutable revisions, readiness | Yes: Agent, Configuration, AgentRevision, deploy/deployments/runtime APIs; model probe before ready | — | Map attempt -> exact AgentRevision (`occ_binding`) |
| Infra reconciliation, retries | Yes: controller worker, Postgres work queue | — | None (do not duplicate) |
| IAM / audit for resources | Yes: exact-resource roles/bindings, audit | — | Task-level authorization; responder identity mapping |
| Model credential | Yes: Secret + `harnessAuth` api_key; only Codex receives the key | Codex logs in via stdin | Funding/credential policy per tenant |
| Session / turn creation | **No public API** | Yes (gateway WS protocol, sessions) | Native session/turn correlation (`native_turn`); authenticated route for product |
| Progress events | No | Yes (gateway push events) | Normalize + per-task sequence + replay |
| Approvals | No (product-facing) | Yes: `exec.approval.requested/resolve`, deny terminal, late approval cannot restart | Durable inbox, responder authorization, request version, delivery/consumption status |
| Questions (non-exec) | No | Unclear for Codex path | Verify in Q2; may need product-side question channel |
| Cancel | Agent-level `stopAgent` only | Turn abort (native) | Task cancel -> native abort -> confirmed stop; `cancelling` until confirmed |
| Restart recovery | Revision continuity | **No** managed-remote resume after gateway restart (documented) | Restart-from-artifacts attempts; supersede old requests |
| Isolation | Pod/ServiceAccount per Agent; NetworkPolicies required | One Harness per revision shared by sessions | Per-task isolation not provided: one Agent per task or trust group, or a sandbox route |
| Product auth to the gateway | Envoy route authenticates the OCC **service key** and grants operator scopes (fixture requests `operator.admin`) | Gateway trusts authenticated operators | Broad service key is not per-user authority (spec §36 warning): need a scoped product principal |
| Task outcome, criteria, verification, review | No (resource-focused) | Turn result only | All of WP05/WP07 (as specified) |
| Budgets | No task budgets | `maxConcurrent` turns | Task budgets / usage ledger |

## Contract changes made from these findings
- Capability manifest gained `isolation_scope` (required) and `native_resume_after_gateway_restart`; non-`per_task` scopes are flagged.
- Approval decisions stay `approve_once | deny`: native "allow always" mints durable grants, which the spec requires to be a separate reviewable rule, so the product will not send it.
