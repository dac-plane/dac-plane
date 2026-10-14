# ADR-0011: Provisional first integration experiment, and confirmed state interpretations

- Status: Superseded experiment — completed with negative adoption result
- Spec: §16, §29 phase 0, §31, §40 Option A, §42 Q1-Q5

## (a) First experiment (Option A, provisional)
| Layer | Choice |
|-------|--------|
| Resource foundation | OpenClaw Enterprise (OCE/OCC), pinned commit (see docs/qualification/Q1-occ-deployment.md) |
| Runtime | Dedicated Codex via the supported OCE path: separate Gateway and Harness Pods, `harnessAuth` `api_key` with an OCC Secret ([harness-execution.md](https://github.com/openclaw/openclaw-enterprise/blob/main/docs/reference/harness-execution.md)). This topology does **not** establish per-session VM isolation: one Harness per AgentRevision is shared by all its sessions. |
| Compute | Disposable local Kubernetes (k3d, via `occ dev up`) on the agent box |
| Sandbox | None. No E2B / OpenShell. E2B is the fallback (Option B) if this route fails or costs too much. |
| Auth | Supported API-key path with a securely supplied test credential (`OPENAI_API_KEY`, never committed) |
| Surface | Minimal API/test client (`spike/`) |

Outcome: Q1 reached real Agent deployment on Mac/k3d+Cilium and proved NetworkPolicy enforcement, but
the dedicated Codex readiness probe ignored the configured custom provider base URL. ADR-0012 therefore
rejected this foundation/runtime path. The table above records the experiment that was run; it is not
the current implementation plan.

## (b) Confirmed interpretations (state machines updated)
1. **Deny** closes that approval request as `denied` and blocks that action only. The task returns to
   `running` (`attention_denied`). A new action needs a new attention request (new native request ref;
   denied is terminal).
2. **Cancel** is allowed while `waiting_attention` and `verifying`. From any state with possibly live
   execution (`running`, `waiting_attention`, `verifying`) the task records `cancelling` until stop is
   confirmed (`stop_reconciled`); `queued` and `ready_for_review` cancel directly.
3. **Late results:** a verification/output/accept observation arriving in `cancelling` or `cancelled`
   is retained for audit only (`routeTaskObservation` -> `audit_only`) and can never produce `succeeded`.
