# Capability report — OCE + dedicated Codex spike (12 Oct 2026)

Legend: **PASS / PARTIAL / FAIL / NOT RUN** are observed results on our deployment. "Documented"
cites OCE docs at `07590c64` or OpenClaw docs (docs.openclaw.ai, read 12 Oct 2026); it is evidence of
intent, not a pass.

| # | Capability | Result | Evidence / reason |
|---|------------|--------|-------------------|
| 1 | Install OCC (local k3d, Kubernetes-only profile) | **FAIL (environment)** | Stops at NetworkPolicy verification; host kernel lacks `ip_set` (Q1 step 9). 2026-10-13: Cilium CNI enforced NetworkPolicy on this box in a separate probe; OCC on it NOT RUN |
| 2 | Namespace ready | NOT RUN | Blocked by 1 |
| 3 | Agent (dedicated Codex, `api_key` Secret) deploy + runtime readiness | NOT RUN | Blocked by 1; script ready (`02-deploy-codex-agent.mjs`) |
| 4 | Bounded real model task | NOT RUN | Blocked by 1 and **no credential** |
| 5 | Task submission -> native session/turn | NOT RUN | Documented: OCC public API has **no** session/turn/message endpoints; turns go to the native OpenClaw Gateway (WSS route via Envoy, or gateway `chatCompletions`) |
| 6 | Progress events normalized | NOT RUN | Documented: gateway push events over WS |
| 7 | Attention request raised by runtime | NOT RUN | Documented: `exec.approval.requested` to operator clients declaring `approvals` capability; Codex app-server command approvals go to a human when no enforceable resolved executable exists |
| 8 | Response reaches the exact native request | NOT RUN | Documented: `exec.approval.resolve` by approval id |
| 9 | Denied action does not execute | NOT RUN | Documented: "Denied exec approvals are terminal for the host command: the command does not run" |
| 10 | Continued execution after answer | NOT RUN | Documented: ordinary runs wait in the same turn and receive the exec result |
| 11 | Cancel actually stops execution | NOT RUN | OCC: only Agent-level `stopAgent`; turn abort is native-only. Documented: cancelling the turn invalidates pending approval authority |
| 12 | Backend (product) restart preserves product state | **PARTIAL** | Product schema + outbox exist and pass constraint tests (CI); not exercised against a live runtime |
| 13 | Stale approval cannot revive a dead/closed turn | NOT RUN | Documented: "A late approval cannot restart it"; product side enforced by state machine (`attention` terminal states, `checkDecision` stale_attempt) — unit-tested only |
| 14 | Gateway restart resumes native session | NOT RUN; documented **unsupported** | OCE harness-execution.md: pinned Codex plugin lacks managed-remote resume after gateway restart |
| 15 | Cleanup and uncertain outcomes visible | NOT RUN | OCC deployment/runtime status APIs exist (documented); product `action_operation.execution_status='uncertain'` exists in schema |
| 16 | Per-task isolation | **FAIL by design (documented)** | One Harness per AgentRevision shared by all sessions; no per-session VM isolation |
| 17 | Image/commit pinning | PASS | Digests recorded in Q1 |

## Exact credential needed
`OPENAI_API_KEY_FILE=/path/to/private/file` (preferred) or `OPENAI_API_KEY` — an OpenAI API key with access to the
Codex model chosen via `SPIKE_MODEL` (default `gpt-6-astra`). Stored only as an OCC Secret bound with
`harnessAuth: {method: "api_key"}`; never committed. Plus a host with `ip_set` kernel support (see Q1).
