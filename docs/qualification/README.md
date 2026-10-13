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

Decision: [ADR-0012](../adr/0012-adoption-decision-occ.md) rejects adopting stock OCE for this phase. See also:
[responsibility map and gaps](responsibility-map.md). Template: `TEMPLATE.md`.

Next qualification track: [control-plane candidates](control-plane-candidates.md). This tests whether an
existing delegated-work control plane such as Paperclip, Kelos or AgentOven can replace the rejected OCE
foundation before the project builds more lifecycle machinery itself.
