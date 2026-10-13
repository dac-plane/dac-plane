# ADR-0012: Do not adopt OCC/OCE as the project runtime dependency now

- Status: **Rejected for current adoption**
- Spec: §29 phase 0 exit gate, §40, §42; ADR-0011

## Decision

Do **not** adopt OpenClaw Enterprise / OCC as the runtime dependency for the delegated-agent control plane in its current
form.

This is not a rejection of every OpenClaw idea. It is a qualification decision for this project requirement: deploy real
agents in a real environment where the runtime may have only our custom provider key / OpenAI-compatible endpoint, not an
OpenAI account key.

## Evidence

- The initial Linux/k3d run failed before Agent deployment because the host kernel lacked NetworkPolicy enforcement support
  (`ip_set`/`xt_set`). That was a valid negative environment result, not an OCE adoption result.
- A Mac/k3d+Cilium path passed OCC's NetworkPolicy verification and reached real dedicated Codex Agent deployment.
- An in-cluster OpenCodex service was deployed with the Camer Digital key in a Kubernetes Secret; Agents received only a
  separate OpenCodex admission token.
- Manual pod-to-OpenCodex checks returned successful model responses, including `/v1/chat/completions` and `/v1/responses`.
- OCC Agent deployment still failed at the dedicated Codex startup model probe with `RUNTIME_AUTHENTICATION_FAILED`
  (`spike/out/q1-spike-codex-incluster3.json`, `spike/out/q1-spike-codex-incluster5.json`).
- In-pod repro showed the root cause:
  - The generated dedicated Codex startup probe invokes Codex with `--ignore-user-config` and no `openai_base_url`.
  - Codex therefore calls OpenAI endpoints with the OpenCodex admission token and fails authentication.
  - The same probe succeeds when manually run with `-c openai_base_url=http://opencodex:10100/v1`.

## Why this fails the adoption gate

The project needs a supported deployment path for custom provider keys and OpenAI-compatible endpoints. In the tested OCE
path, the custom provider `baseUrl` configured in OCC does not reach the dedicated Codex readiness probe that gates runtime
availability. A local OCE controller patch can be sketched, but needing to patch and roll a custom controller image means OCE
is not qualified as an off-the-shelf dependency for this phase.

Additional documented constraints of Option A with dedicated Codex remain:

  - OCC has no public session/turn/approval API; the product must speak the native gateway protocol behind an OCC service-key route.
  - One Harness per AgentRevision serves all sessions: per-task isolation requires one Agent (revision) per task/trust group.
  - No native resume after gateway restart on the pinned Codex plugin.
  - Kubernetes + Envoy Gateway + cert-manager + enforcing CNI are hard prerequisites; local setup needed 5 workarounds here.

## Consequences

- Stop spending qualification time trying to make stock OCE pass Q1.
- Keep the Cilium and OpenCodex evidence because it is useful for a project-owned runtime/control-plane path.
- Do not run Q2–Q5 against OCE unless the OCE dedicated Codex probe/base URL issue is fixed upstream or the project
  explicitly chooses to maintain an OCE fork.
- Continue with the lighter project-owned route for the next implementation slice, comparing against OCE only as prior
  evidence.

## Reversal condition

This decision can be revisited only if an unpatched OCE build can deploy a dedicated Codex Agent using a custom
OpenAI-compatible endpoint and reach runtime readiness, followed by the smoke/approval/deny/cancel/stale/history slice.
