# Issue #72 — Carry ControlFrame over the wire and publish the adapter kit

Tracker: https://github.com/dac-plane/dac-plane/issues/72
Branch: `feature/72-adapter-transport`, from `main` at `0a8f1f9`, in the main checkout.
Parent: #65 (M2: Deployable control plane).

## Direction

Implement the boundary ADR-0008 decided: production adapters run out of process, dial the control
plane over HTTPS and upgrade to one WebSocket (`dac-plane.controlframe.v1`) carrying one JSON
ControlFrame per text message; an OAuth client-credentials token maps to (tenant, provider) only
through a stored binding; a connection lives no longer than its token; a replica claims a provider's
work only while it holds a live connection for it. A separate `adapterkit` module carries the client,
codec, handler interface and conformance suite. The simulators become reference adapters.

Confirmed with the maintainer on 2026-10-24:

- One PR closes #72.
- ADR-0010 adds `subscribe` and `unsubscribe` frame kinds to protocol v1 (`Port.Subscribe` had no
  wire form), set to Accepted in the PR.
- The M1 suites run over the real wire in the same test binary, simulator served by the kit in a
  goroutine, faults injected at frame level; one end-to-end test and the conformance recipe run the
  simulator as a separate OS process.

Settled by the ADRs and evidence, not reopened:

- ADR-0009: `serve`, listeners and TLS are #73; #72 delivers an `http.Handler` tested through
  `httptest.NewTLSServer`. The release binary must not link the simulators (the CI check is #73's).
- `architecture-explore`: Plane-Bot's daemon protocol (hello/ready/shutdown, request/cancel/reply/
  event, fail-fast version mismatch) and its watch stream opened from a cursor; SPEC §17 15 s
  heartbeat; SPEC §22 bounded backoff with jitter; no prototype transport to reuse.

Industry practice checked (Tavily and Context7, 2026-10-24):

- `github.com/coder/websocket`: maintained, context-aware, safe concurrent writes; gorilla is archived.
- Bearer token in the upgrade `Authorization` header (non-browser clients); refuse with 401 before
  upgrading; reconnect with a fresh token rather than in-band refresh.
- Exponential backoff with full jitter for reconnects.

## Design

- Liveness: ping every 15 s, 10 s pong timeout, configurable; a missed pong closes.
- Token lifetime: stop routing new requests at `exp - drain` (30 s, at most half the lifetime);
  `shutdown` and close at `exp`. The kit reconnects before the drain point.
- Close: a `shutdown` frame with the typed failure precedes every protocol close; status 1000 normal
  or expiry, 1008 auth or protocol violation, 1011 internal.
- Limits: 1 MiB frames by default; binary or oversize messages are `malformed_frame`; 64 in-flight
  requests per connection; Send with every connection full returns `transport_unavailable` before the
  durable send marker.
- Correlation: replies match (operation, delivery) on the same connection; unmatched replies are
  dropped and counted; invalid frames end the connection; a close with requests pending fails each
  with a transport error (the worker's uncertain path). Cancel returns once written.
- Subscriptions: routed by (operation, stream) on the opening connection; every event is checked
  against the connection's peer and the cursor's binding before `Stream.Next` yields it; events for
  no subscription are dropped; a dropped connection ends its streams with an error.
- Routing: Send picks the live connection with the fewest in-flight requests.
- Claim gating: `dispatchRow` already looks up the port before claiming; the registry returns none
  without a live, negotiated connection.
- Binding revocation: each connection re-resolves its binding on its ping tick and closes when the
  binding is disabled or missing.

## Tasks

1. ADR-0010; `FrameSubscribe`/`FrameUnsubscribe` in `internal/contracts` and `frame.schema.json`
   with agreement tests; link ADR-0010 from the architecture README; contract-freeze row.
2. `identity.ProviderRegistration` and `identity.AdapterBinding` with schemas and agreement tests;
   migration `00012_adapter_binding.sql`; store functions; provisioning `register-provider`,
   `bind-adapter`, `unbind-adapter`.
3. `authn.Verifier` returning issuer, subject and expiry; `adapter_issuers` in the identity config.
4. `adapterkit/` module: `wire`, `client`, handler interface, `conformance` harness and
   `cmd/adapter-conformance`; schema agreement tests against `../contracts/jsonschema/adapter`.
5. `internal/adapterwire`: `Endpoint` and `Registry`; connection audit logging without secrets.
6. `worker.Ports` replaces the four provider maps; bootstrap wires the registry.
7. Simulator bridge to the kit handler; `cmd/dac-sim-adapter`; `internal/testsupport/wiretest`;
   shared fixtures switched to the wire; lost-reply decorators become frame-level faults.
8. justfile recipes `test-adapterkit`, `test-conformance`, `test-adapter-wire`, adapterkit coverage
   in fmt-check, mod-check and vuln; CLAUDE.md working set; PR.

## Verification

    just test-contracts
    just test-identity
    just test-adapterkit
    just test-adapter-wire
    just test-conformance
    just test-recovery test-security test-migration test-hook-lifecycle test-surface
    just ci

Mutation checks: removing the pending-request failure on close, the wire stream's peer check, or the
drain must each make a named test fail.

## Deviations and follow-ups

- The `internal/worker` loopback suites stay in-process: they script the loopback port
  synchronously and assert on its ledger, and `TestDeliveryRejectsReplyForAnotherDelivery` must
  stay in-process because the wire endpoint rejects a mismatched reply before the worker sees it.
  The wire path is covered by the converted `internal/application` and `internal/httpapi`
  suites and by `internal/adapterwire`'s own suite.
- `cmd/dac-sim-adapter` serves runtime scenarios only; the other roles follow when a test needs
  them. Its conformance fixture and scenario live in `cmd/dac-sim-adapter/testdata/`.
- `test-adapterkit` runs vet and unit tests (plain and race) inside the kit module;
  `test-conformance` builds both binaries and runs the kit's conformance runner against the
  simulator as a separate OS process; both are in the `ci` chain, as is `test-adapter-wire`'s
  package (`internal/adapterwire` also runs in the integration partitions).
