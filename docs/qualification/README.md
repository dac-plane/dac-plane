# Qualification reports (WP01, spec §42)

Result values: PASS, PARTIAL, FAIL, NOT RUN. "Documented" = upstream docs at the pinned commit say so;
it is never counted as PASS.

| Exp | Subject | Result | Report |
|-----|---------|--------|--------|
| Q1 | OCC real deployment | **FAIL (environment)**; model execution NOT RUN | [Q1](Q1-occ-deployment.md) |
| Q2 | Native attention (question + approval, approve/deny/cancel) | NOT RUN (blocked by Q1) | [capability report](capability-report.md) |
| Q3 | Restart and expiry | NOT RUN (blocked by Q1) | [capability report](capability-report.md) |
| Q4 | Trust boundary | FAIL on this host (no NetworkPolicy enforcement); design gap documented | [capability report](capability-report.md) |
| Q5 | Lifecycle ownership | NOT RUN (blocked by Q1) | [capability report](capability-report.md) |
| Q6 | Sandbox alternative (E2B) | NOT RUN | |
| Q7 | Upgrade / license | PARTIAL (pins recorded; no upgrade test) | [Q1](Q1-occ-deployment.md) |
| Q8 | Product portability | NOT RUN | |

See also: [responsibility map and gaps](responsibility-map.md). Template: `TEMPLATE.md`.
