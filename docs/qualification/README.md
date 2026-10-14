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
| CP3 | AgentOven control-plane candidate | **NOT RUN — DISQUALIFIED BY SOURCE SCREEN**; the public release is too immature to rely on as the product foundation: effectively single-maintainer ownership, red CI, failed/mutable release handling, large low-cohesion files, and production durability/security components in a private Pro repository. | [CP3 source audit](CP3-agentoven.md) |

Decision: [ADR-0012](../adr/0012-adoption-decision-occ.md) rejects adopting stock OCE for this phase. See also:
[responsibility map and gaps](responsibility-map.md). Template: `TEMPLATE.md`.

No full third-party control-plane candidate is currently selected. Paperclip failed the provider-neutral
runtime gate; Kelos conflicts with the non-Kubernetes deployment constraint; AgentOven failed the source,
release and deployment-fit screen before installation. The next decision is therefore the smallest
product-owned control-plane core plus one replaceable runtime/environment backend, not another assumed
all-in-one foundation.
