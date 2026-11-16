# Govern an agent's access to external applications

Tracker: https://github.com/dac-plane/dac-plane/issues/137
Branch: `feat/137-connectors`, from `main` at `2802e12`. One branch carries the decision and the whole implementation.
Decision: `docs/adr/0022-govern-connectors-at-a-control-plane-gateway.md`.

## Where the tree stands

Surveyed on 2026-11-16.

| Finding | Evidence | Consequence |
|---|---|---|
| Governed actions exist but nothing reaches them | `internal/application/governed.go` is called only from tests, for `resource.mutate` | The gateway is the first caller; it gets its own command rather than bending this one. |
| Every allowed use needs a recorded approval | `EnforceGovernedAction` refuses a grant without a decision | An `always` connection grant is the owner's approval; the call issues a standing attempt grant attributed to the granter, and the call row names the connection grant. |
| Tenant policy defaults to `never` for an unknown class | `Policy.Effective` | A connector class absent from tenant policy takes the connection grant's permission; a class the tenant names is capped by it, and a never rule still wins. |
| No hook without an enforcement provider | `evaluateActionHook` returns `HookMissing`, which denies | Connector calls record a new hook outcome, `not_required`: the gateway is the enforcement point. Tenant hooks on connector calls are deferred. |
| `execute_action` returns only a digest receipt | `worker/governed.go` | A connector call needs its output back: a call row holds input and output, the worker fills it, the gateway waits on it. |
| Attention decisions are delivered to the runtime by `steer_turn` | `worker/decision.go` | A gateway-raised approval has no runtime request to steer: its decision is not delivered, the gateway consumes it. |
| Provider events are attempt-bound observations | `contracts.Event` | Consent completion is a request, `complete_authorization`, not an event. |
| One Codex app-server serves every thread | `adapters/codex/appserver/client.go` | The gateway goes in per-thread `config.mcp_servers` (spike on 0.165.0: header sent, not persisted). |
| The worker polls every 5 s | `worker.Config.PollInterval` | The gateway wakes the worker after enqueueing a call. |

## Stages

Each stage is one commit; each runs its narrowest checks before the next starts.

1. **Contracts.** `connector` role and its methods (`list_tools`, `begin_authorization`, `complete_authorization`,
   `revoke_connection`, `execute_action`); tool catalogue, authorization, connector call and receipt records; the
   connection and connection grant lifecycle records; `RuntimeStart.gateway` under `connector.gateway`; hook outcome
   `not_required`; JSON Schemas and agreement tests; adapter kit mirror.
2. **Storage.** Migration 18 (probe moves to 19): `connection`, `connection_grant`, `connector_call`, record kinds,
   hook outcome. Store functions with integration tests.
3. **Commands and Surface API.** Register, authorize, complete, list, read and remove connections; grant, list and
   revoke grants; OpenAPI, problems and the schema-unavailable path.
4. **Gateway.** MCP `2025-06-18` over Streamable HTTP on the adapter listener; attempt tokens (HMAC over tenant and
   attempt, checked against the attempt's state on every call); `tools/list` from granted tools; `tools/call` as a
   governed action with `ask` waits; call limits.
5. **Worker.** Connector call dispatch and its result, connection revocation, token delivery in `start_turn`,
   cancellation of in-flight calls when the attempt ends.
6. **Simulator and conformance.** A deterministic connector simulator, the `dac-sim-adapter` connector role and the
   conformance suite for the role.
7. **Codex adapter.** Negotiate `connector.gateway`, configure the gateway per thread and on resume.
8. **GitHub connector adapter.** A separate module: GitHub App web flow with PKCE, sealed credential store, three tools,
   a fake GitHub for deterministic tests, and a live qualification recipe.
9. **Docs and acceptance.** Connectors guide, Surface API and adapter pages, configuration reference, the egress rule,
   the canary-secret test, the ADR accepted.

## Decisions

Recorded as each stage settles them.

For stage 1:

- Connectors call tools with their own method, `call_tool`. `execute_action` stays the enforcement role's, because the
  catalogue gives every method one role and a connector call carries its input, which an enforcement execution never
  does.
- A connector execution carries its provenance directly: the attempt grant, the connection grant and, for a one-time
  approval, the decision. `ActionExecution` requires a decision, and an `always` grant has none.
- Connector calls record no hook evidence, rather than a new hook outcome: the call row says the gateway enforced it.
- The scope of a connector action is the subject `connection:<id>`.
- A connection records its account only after consent; the grant names the tools as a sorted set and holds `ask` or
  `always`, never `never`, which the tenant policy expresses.

For stage 2:

- Migration 18 adds `connector` to the registration roles, `connection`, `connection_grant` and `connector_call`, and the
  two record kinds. Its down migration refuses while a connection or a connector registration exists. The operations
  probe moves to 19.
- A connector call's execution and output are `text`, not `jsonb`: their digests cover exact bytes, which `jsonb`
  rewrites. Tool input is canonical JSON, the form `encoding/json` writes a raw message in, so re-encoding a record
  that carries it never changes its digest.
- A connection's requests outside an attempt (`list_tools`, `begin_authorization`, `complete_authorization`,
  `revoke_connection`) name no attempt and never become provider operations; the frame schema requires an attempt on
  every other request. They are synchronous, idempotent by connection, and repeated by the command that needs them.
- A connector call is recorded when the gateway decides it; one awaiting approval has no provider operation yet.

For stage 3:

- Consent initiation requires an idempotency key and records its returned URL;
  retrying that key returns the same URL and a new key renews consent.
- Completion replays its recorded result without requiring an online connector.
  Removal first revokes all grants atomically, then reports `removing` until the
  connector confirms credential destruction; repeating removal retries it.
- Verified connection ownership, replay, stale versions, pending consent, outage
  and schema-17 behavior through real HTTP and PostgreSQL tests.

For stages 4–7 (in progress):

- ADR-0023 adds read-only `read_call` recovery and makes `removing` a durable,
  automatically reconciled credential-destruction intent without an attempt.
- Gateway tokens are HMAC-derived under an explicit shared deployment key; no
  token is stored in product records. Codex retains its gateway access in the
  adapter's restricted journal for reconnect and configures it outside prompts.
- The gateway approval path consumes decisions without runtime steering. Expired
  or revoked pending approvals are closed by the worker rather than wedging work.
- The real-wire governed-call integration test passes, including a credential
  canary scan over every public-schema database row and sent connector frames.

## Delivery status (2026-11-16)

Stages 1–9 are implemented on this branch. Final hosted checks and live provider
qualification are separate from implementation completion.

- Contracts and schema 18 storage: committed, with schema agreement and PostgreSQL coverage.
- Surface API: connection/grant commands, idempotent consent initiation, browser-only
  completion proof, schema-17 rollout behavior and pending-removal semantics tested.
- Gateway and worker: MCP tools/list and tools/call over the real adapter wire;
  ask decisions, exact-input binding, overlapping grants, per-task ceilings,
  expired waits, revocation, output recovery and durable removal implemented.
- Dispatch authorization is rechecked after budget and operation locks. Regression
  coverage holds each lock past an allow-once expiry and proves the call is refused.
- Simulator and kit: connector role has its own 14-scenario process conformance run.
- Codex: negotiated per-thread gateway configuration, restart configuration and
  migrated target-attempt tokens covered. Old adapters receive no gateway field.
- GitHub: separate module and buildable image; deterministic fake-service tests
  cover PKCE, callback-browser proof, sealed credentials, retained renewal generations,
  refresh, revocation, cancellation, restart, unknown effects and secret projection.
  Removing the browser-proof predicate experimentally makes the security test fail.
- Guide, configuration, adapter documentation, ADR-0023, CI recipes and module checks
  include the new behavior and reference connector.

Observed checks: `just ci-fast`, `just test-integration ./...`,
`just test-integration-race ./internal/httpapi/ -run 'Test(Connector|SurfaceConnections)'`,
`just test-github-adapter`, `just test-codex-adapter`, `just test-contracts`,
`just test-conformance`, `just docs-check`, `just vuln`, `git diff --check`,
and a Docker build of target `dac-github-adapter` pass. The first vulnerability
lookup had a DNS failure; the retry completed with no vulnerabilities found.

Not verified locally: a real GitHub App OAuth session/API qualification, a live
model making a connector call, runtime-host egress enforcement, and the full
image/stack/operations CI suites. The tagged live GitHub qualification runner
builds and has a documented dedicated-fixture procedure; it was not executed
without a consented fixture connection. No final release publication is included.
