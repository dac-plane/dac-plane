# ADR-0023: Recover connector outcomes by lookup and credential removal by durable intent

- Status: Accepted
- Date: 2026-11-16
- Issue: [#137](https://github.com/dac-plane/dac-plane/issues/137)
- Refines: [ADR-0022](0022-govern-connectors-at-a-control-plane-gateway.md)

## Context

A connector call returns output to its runtime. The existing provider event stream
can settle a generic operation but has no connector-output record. Retrying a tool
with an unknown external effect would risk repeating it. Credential removal also
needs recovery after the serving process dies, but a provider operation belongs to
an attempt and connection administration does not.

The implementation exposed two additional serialization constraints: frames bound
JSON objects to 64 KiB, and JSON encoding compacts and HTML-escapes raw messages.
Digests must cover bytes that survive that encoding.

## Decision

Add `read_call` to the connector role. It is an attempt-bound, read-only lookup of
an operation's durably recorded result, returning the same `ConnectorResult` as
`call_tool`. It never initiates or retries an external request. An adapter without
a recorded outcome reports unknown durability. The worker leaves that operation
uncertain, repeats cancellation when its authority has ended, and continues
looking for evidence. A completed result settles the operation without another
`call_tool` request.

The connection's `removing` state is the durable credential-revocation intent.
Removal atomically revokes its grants. The command attempts revocation immediately;
the reconciliation loop also polls removing connections after process restart.
`revoke_connection` is idempotent by connection and succeeds only after remote
revocation and local credential destruction are confirmed. A lost reply is safe to
query by repeating this revocation. This replaces ADR-0022's proposal to represent
connection removal as an attempt-bound provider operation. A removal has no
attempt, authorization use, or fabricated runtime identity.

Connector output, like input, uses compact JSON with the HTML escaping performed
by `encoding/json` before its digest is computed. Tool output must fit within the
existing frame bound, including the result envelope. Credential-bearing adapter
errors are projected to fixed messages and never copied into product audit.

The gateway's signing key is an explicit deployment secret, shared across
replicas. The attempt bearer token is reconstructed at dispatch and never stored
in product records. The Codex adapter retains its gateway access in its existing
mode-0600 journal so it can configure the same MCP endpoint on thread resume.
That is a runtime transport credential, bound to the attempt, and is checked
against current product state on each use. External application credentials stay
only in the connector's sealed store. A runtime adapter host and its journal
therefore remain inside the runtime trust boundary.

Consent completion additionally requires a short-lived confirmation proof given
only to the callback browser, in the fixed surface return URL fragment or callback
response. The authenticated connection owner must present it to the Surface API.
A forwarded authorization URL alone cannot activate the recipient's account under
the sender's connection: the sender never receives the callback proof, while the
recipient cannot complete a connection owned by another principal. The proof is
bound to the pending connection and its latest consent, stored by digest at the
connector, and included only by digest in the product command fingerprint.

Renewal retains prior credential generations until remote access revocation is
confirmed, and the newly exchanged token is sealed before account discovery.
This prevents losing a live credential when account discovery or a process fails.

## Consequences

Recovery has an explicit observation path and never guesses that an unknown call
had no effect. An adapter must durably record both completed calls and cancellation
before effect. A crash between sending a non-idempotent remote request and recording
its response remains uncertain until the adapter obtains external evidence.
Connection removal converges without a surface keeping a request open or retrying.
The simulator and reference connector must test result lookup, lost replies,
revocation and cancellation separately from ordinary successful calls.
