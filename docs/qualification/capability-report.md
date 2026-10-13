# Capability report — OCE + dedicated Codex spike (12 Oct 2026)

Legend: **PASS / PARTIAL / FAIL / NOT RUN** are observed results on our deployment. "Documented"
cites OCE docs at `07590c64` or OpenClaw docs (docs.openclaw.ai, read 12 Oct 2026); it is evidence of
intent, not a pass.

| # | Capability | Result | Evidence / reason |
|---|------------|--------|-------------------|
| 1 | Install OCC (local k3d, Kubernetes-only profile) | **PARTIAL** | Mac Cilium/k3d shim passed OCC NetworkPolicy verification and brought OCC up; Q1 still fails later at dedicated Codex model probe |
| 2 | Namespace ready | PASS | Default OCC Namespace reached `ready` in the Mac run (`ns_9b9d0068-d45d-447b-a33f-a985048128d5`) |
| 3 | Agent (dedicated Codex, `api_key` Secret) deploy + runtime readiness | **FAIL** | Agent create, Secret binding, runtime credentials, pod startup, Codex login, node setup, and gateway connection succeed; deployment fails at `codex.model_probe` with `RUNTIME_AUTHENTICATION_FAILED` (`spike/out/q1-spike-codex-incluster3.json`) |
| 4 | Bounded real model task | NOT RUN | Blocked by failed dedicated Codex runtime readiness; manual pod-to-OpenCodex probes did return real model completions but are not a deployed OCC Agent turn |
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

## Exact credential path used on the Mac run

The Mac run used the Camer Digital key from the local OpenCodex configuration, placed it in the in-cluster OpenCodex Secret,
and gave OCC Agents only a separate OpenCodex admission token. The dedicated Codex configuration points at
`http://opencodex.<occ-namespace>:10100` with `SPIKE_MODEL=camer-digital/glm-5.3-flash`.

This proves OpenAI-compatible routing is not the conceptual blocker. The current blocker is that OCC's dedicated Codex startup
probe receives a failed Codex turn and records it as `RUNTIME_AUTHENTICATION_FAILED`; OpenCodex logs from the run family show
Camer upstream `502`/`429` responses. Q1 should stay failed until the deployed Agent itself reaches runtime readiness and
`03-slice.mjs` completes the native gateway scenarios.
