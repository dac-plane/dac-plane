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
| CP1 | Paperclip control-plane candidate | **CLOSED — REJECTED**; install/state worked, but both `test-drive` and normal `paperclipai run` delegated Codex calls still used OpenAI instead of the configured Camer Digital endpoint. This fails the hard provider-neutrality gate; no retest is scheduled. | [CP1](CP1-paperclip.md) |
| CP2 | Kelos control-plane candidate | **CLOSED — REJECTED FOR PRIMARY DEPLOYMENT**; Kelos requires Kubernetes/cert-manager, while the primary product deployment must not require Kubernetes. It is not on the active implementation path. | [candidate screen](control-plane-candidates.md#second-screen-kelos) |
| CP3 | AgentOven control-plane candidate | **CLOSED — REJECTED BY SOURCE SCREEN**; the public release is too immature to rely on as the product foundation: effectively single-maintainer ownership, red CI, failed/mutable release handling, large low-cohesion files, and production durability/security components in a private Pro repository. | [CP3 source audit](CP3-agentoven.md) |
| CP4 | OpenFlows control-plane candidate | **CLOSED — DISQUALIFIED BY CODER DEPENDENCY**; its controller, identity, model-routing and production workspace topology make Coder part of the control plane, failing CP8 portability. | [CP4](CP4-openflows.md) |
| CP5 | Temporal/Hatchet/Inngest as control planes | **CLOSED — ALL DISQUALIFIED BY SOURCE/API SCREEN**; they persist execution runs, attempts, waits, events and results but do not supply the authoritative delegated-work state required by CP7. | [CP5](CP5-workflow-substrates.md) |
| CP6 | Final market scan and CanDoItAll runtime | **CLOSED — REJECTED AS PRIMARY FOUNDATION**; provider routing, workflow authority, restart recovery and process cancellation passed, but the hard CP7 task/attempt budget mapping failed and current deployment fit failed CP9. | [CP6](CP6-final-market-scan.md) |
| CP7 | Prismor policy component | **NOT RUN**; proposed policy/tool-call enforcement fit has not been qualified. | [candidate plan](control-plane-candidates.md) |

Decision: [ADR-0012](../adr/0012-adoption-decision-occ.md) rejects adopting stock OCE for this phase. See also:
[responsibility map and gaps](responsibility-map.md). Template: `TEMPLATE.md`.

No control-plane qualification produced an adopted foundation. Paperclip failed provider
routing; Kelos failed deployment fit; AgentOven failed source/release/deployment maturity; OpenFlows
failed portability because Coder is part of its control topology; Temporal, Hatchet and Inngest failed
the required-state mapping; BoringOS and OCCP failed the final source screen. CanDoItAll passed CP2,
CP4, CP6 and CP8 but failed hard gate CP7 because it has no authoritative task/attempt budget contract;
its current release/security posture also failed CP9. Prismor remains untested only as a narrower policy
component, not as a control-plane candidate.
