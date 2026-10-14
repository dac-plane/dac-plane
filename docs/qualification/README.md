# Qualification reports (WP01, spec §42)

Result values: PASS, PARTIAL, FAIL, NOT RUN. "Documented" = upstream docs at the pinned commit say so;
it is never counted as PASS.

| Exp | Subject | Result | Report |
|-----|---------|--------|--------|
| Q1 | OCC real deployment | **FAIL**; Mac/k3d+Cilium reached Agent deployment, but dedicated Codex probe ignored custom base URL | [Q1](Q1-occ-deployment.md) |
| Q2 | Native attention (question + approval, approve/deny/cancel) | NOT RUN (blocked by Q1) | [capability report](capability-report.md) |
| Q3 | Restart and expiry | NOT RUN (blocked by Q1) | [capability report](capability-report.md) |
| Q4 | Trust boundary | PARTIAL; Cilium enforced NetworkPolicy on Mac, but stock OCE runtime path still failed before live task evidence | [capability report](capability-report.md) |
| Q5 | Lifecycle ownership | NOT RUN (blocked by Q1) | [capability report](capability-report.md) |
| Q6 | Sandbox alternative (E2B) | NOT RUN | |
| Q7 | Upgrade / license | PARTIAL (pins recorded; no upgrade test) | [Q1](Q1-occ-deployment.md) |
| Q8 | Product portability | NOT RUN | |
| CP1 | Paperclip control-plane candidate | **FAIL / WATCH**; install/state worked, but both `test-drive` and normal `paperclipai run` delegated Codex calls still used OpenAI instead of the configured Camer Digital endpoint. Upstream has merged/active gateway work, so retest only after a runtime propagation fix or local patch. | [CP1](CP1-paperclip.md) |
| CP2 | Kelos control-plane candidate | **NOT RUN — DISQUALIFIED BY DEPLOYMENT CONSTRAINT**; Kelos requires Kubernetes/cert-manager, while the primary product deployment must not require Kubernetes. Kelos remains eligible only as a future optional Kubernetes environment adapter. | [candidate screen](control-plane-candidates.md#second-screen-kelos) |
| CP3 | AgentOven control-plane candidate | **NEXT**; qualify the standalone server/container path, custom Camer Digital routing, durable lifecycle state, bound human approval, restart recovery and cancellation before considering adoption. | [candidate plan](control-plane-candidates.md#third-run-agentoven) |

Decision: [ADR-0012](../adr/0012-adoption-decision-occ.md) rejects adopting stock OCE for this phase. See also:
[responsibility map and gaps](responsibility-map.md). Template: `TEMPLATE.md`.

Next qualification: [AgentOven](control-plane-candidates.md#third-run-agentoven). This tests whether its
non-Kubernetes server/container path can provide enough delegated-work lifecycle control before the
project builds more lifecycle machinery itself.
