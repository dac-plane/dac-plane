# ADR-0022: Govern connectors at a control-plane gateway, with credentials held by connector adapters

- Status: Proposed
- Date: 2026-11-16
- Issue: [#137](https://github.com/dac-plane/dac-plane/issues/137)

## Context

An agent's work reaches only what its runtime and sandbox allow. dac-plane has no record of a connection to an
external application, no way to grant one to an agent, and no control over a call made with it. The reference
application ([#97](https://github.com/dac-plane/dac-plane/issues/97)) needs agents that work with other systems, with
dac-plane governing that access rather than the application.

The retained research set a direction without deciding it: a broker holds OAuth and API secrets, the runtime gets a
connection reference and never the secret, and connector policy is not enough without narrow credentials and network
limits (`architecture-explore/docs/spec/SPEC.md`, "Connector credentials").

### What exists

- **Governed actions.** `EnforceGovernedAction` evaluates a pre-action hook, then in one transaction revalidates
  authority, consumes one-time authority, records `AuthorityUse` and audit, and enqueues `execute_action` to an
  executor; post-action and failure hooks, an `EnforcementReceipt` with an output digest and reconciliation of an
  indeterminate outcome follow ([ADR-0003](0003-provider-contract-semantics.md)). It is reachable only as an
  application function, for one simulator action, `resource.mutate`; no wire or API leads to it.
- **Authority.** Tenant policy of `never`, `ask` or `always` per action class, with a ceiling and a generation;
  allow-once grants bound to an operation and input digest; standing grants bound to a scope; revocation; typed denial
  reasons (`internal/identity/authority.go`). Grants belong to one attempt.
- **Approval.** An attention request of kind `action_approval`, decided by a person, issues an allow-once or standing
  grant. The Codex adapter already routes Codex's native command and file approvals into it.
- **Enforcement role.** A provider role whose adapter answers pre-action and post-action hooks. Without one, a
  governed action fails closed (`HookMissing`).
- **Network.** The E2B sandbox has no internet access and agent commands run inside it
  ([ADR-0016](0016-run-agent-commands-inside-the-environment.md)). The Codex runtime itself runs on the adapter host,
  which reaches the model provider and is not otherwise limited.

### What the standards require

The Model Context Protocol is the interface runtimes already use for tools. Its security best practices say servers
"MUST NOT accept any tokens that were not explicitly issued for the MCP server", and its authorization specification
requires a server to validate that a token was issued for it as audience (RFC 8707). A server that calls a third-party
API must not reuse the client's credential for it, and must not hand credentials it obtained to the client. A proxy
that holds one OAuth client for many users must obtain consent per client, match redirect URIs exactly and use a
single-use, short-lived `state`. Sensitive input such as a sign-in goes through a URL the user opens, never a form in
the client.

### Comparable products

| | Nango | Composio | Arcade |
|---|---|---|---|
| Where the secret lives | In the platform; the agent gets a session token | In the platform; reads are redacted | In the engine; injected at execution |
| How an agent calls | One MCP endpoint per session | A hosted MCP URL per session, SDKs, or a proxy | MCP servers built on its framework |
| Self-hosting | Elastic License 2.0; the free edition covers API authorization only, the MCP server needs Enterprise | Enterprise contract; SDKs are MIT, the backend is closed | Framework MIT; the engine is commercial |

All three keep the credential away from the agent and give it a session-scoped endpoint. None knows dac-plane's policy,
approvals, budgets or audit, so a runtime that called one directly would bypass governed actions. None can be
self-hosted in full without a commercial agreement.

### What the pinned runtime does

Observed on Codex app-server 0.165.0, the version the adapter pins:

- `thread/start` and `thread/resume` accept a `config` object. An `mcp_servers` entry in it adds a Streamable HTTP
  MCP server to that thread only, with its own `http_headers` and `tool_timeout_sec`.
- Codex sent the configured `Authorization` header on `initialize`, `notifications/initialized` and `tools/list`, and
  offered protocol version `2025-06-18` with form and URL elicitation.
- After a turn, neither the session transcript nor Codex's state and log databases contained the header value.
- The global `mcpServerStatus/list` did not list the per-thread server.

## Decision

**A connector is a new provider role, `connector`, with its own adapter.** The adapter holds the credentials of the
connections it serves, completes their OAuth consent, exposes a catalogue of tools, and executes a tool call when
dac-plane dispatches one. Its credentials never leave it: not to dac-plane's database, a frame, an event, a log or an
artifact. A hosted platform such as Nango or Composio may sit behind a connector adapter; the provider-neutral
contracts name no vendor and no connector-specific field.

**dac-plane is the only way a runtime reaches a connector.** dac-plane serves an MCP endpoint, the connector gateway,
on the adapter endpoint's listener. A runtime calls connector tools there and never talks to a connector adapter.
Each `tools/call` becomes a governed action and runs through the existing pipeline: policy, grant, pre-action hook
where the tenant has an enforcement provider, `AuthorityUse`, audit, outbox `execute_action` to the connector adapter,
receipt, post-action hook and reconciliation. The gateway is the policy and tool gateway that the baseline's ownership
rule 4 requires on the execution path, so a connector action does not need an enforcement provider; when the tenant
has one, its hook runs as for any governed action. The gateway pins MCP `2025-06-18`, the version the pinned Codex
offers, assigns no MCP session, and adds later revisions by negotiation.

**A runtime reaches the gateway with an attempt token.** When a delegated or granted task's attempt starts, dac-plane
mints an opaque, random token bound to the tenant, attempt and runtime provider, with the gateway as its only
audience, and stores only its digest. It is carried in `start_turn` beside the brief, only to an adapter that
negotiated the `connector.gateway` capability, as `agent.instructions` is
([ADR-0021](0021-refuse-delegated-turns-at-dispatch.md)). The token is checked against the database on every call, so
revocation takes effect at once, and it ends with the attempt in the same transaction. It is never a credential for the
external application, and the gateway never forwards it. The Codex adapter places the gateway in the thread's
`config.mcp_servers` with the token as an `Authorization` header, again on `thread/resume`, and sets the server's
tool timeout above the approval wait below.

**Records.** dac-plane adds two product records and reuses the existing ones for each use:

- A **connection**: tenant, connector provider, the owning principal, a display name, the external account and the
  permissions the external application reports, and a state of `authorizing`, `active`, `refresh_needed`, `revoked`,
  `expired` or `error`. It refers to the credential the adapter holds; it holds no secret.
- A **connection grant**: a connection, the agent or the task it applies to, the tools it allows, a permission of
  `ask` or `always`, a call ceiling per task, an expiry, and who granted and revoked it. Only the connection's owner may
  grant it. A grant is used only by a task whose owner is the connection's owner; shared connections, used by tasks of
  other owners, are not part of this decision.
- **Each use** is a governed action with action class `connector.<tool>` and a subject scope naming the connection.
  Its `AuthorityUse`, audit entries and receipt carry digests and safe projections, never tool output bytes or
  credentials.

**Grants become attempt authority at call time.** The gateway resolves the attempt to its task and agent and looks for
an active grant naming the connection and tool. Without one the call is refused with `no_grant` and recorded. The
effective permission is the lower of the grant's and the tenant policy's ceiling. With `always`, the gateway issues
the attempt a standing grant for the connection's scope and the call proceeds. With `ask`, it raises an
`action_approval` attention request bound to the call's input digest. The call ceiling is checked in the same
transaction that records the use.

**An `ask` call waits for its decision, bounded.** The gateway holds the `tools/call` open while the attention request
is pending, sending MCP progress notifications when the client supplied a progress token, up to a configured wait below
the runtime's tool timeout. If the decision lands in time, the call proceeds or is refused. If it does not, the call
returns a tool error saying approval is pending and the attention request stays open. Its allow-once grant is bound to
the input digest, so the same call made again after approval consumes it, and a changed input needs a new approval.

**Consent completes at the connector adapter.** A surface registers a connection through the Surface API. dac-plane
asks the connector adapter to begin authorization, and the adapter returns an authorization URL with PKCE and a
single-use `state` of short lifetime, bound to that pending connection. The response gives the URL to the
authenticated principal only. The external application redirects to the adapter's exact callback, the adapter
exchanges and stores the credential, and it reports the account and permissions to dac-plane, which makes the
connection `active`. The surface, the runtime and the model never see the credential. Removing a connection is a
provider operation: the adapter revokes the credential at the external application and destroys it, and an
indeterminate outcome is reconciled, so deleting a record is never mistaken for revoking access.

**Revocation and cancellation stop use.** Revoking a grant, removing a connection or ending an attempt invalidates the
authority it gave in the same transaction, so the next call is refused. A call already dispatched receives a cancel
frame; the adapter reports whether the external request was sent, and an indeterminate outcome enters reconciliation
and is never repeated.

**The runtime host's egress is a deployment rule.** The runtime adapter's host may reach the model provider and
dac-plane and nothing else, so the gateway cannot be bypassed by a direct call. The reference deployment documents
it, and qualification probes it where the deployment can enforce it. Agent commands keep running in a sandbox without
internet access.

**The reference connector is GitHub, through a GitHub App.** Its user access tokens last eight hours and refresh, and
are limited to permissions both the app and the user hold. A deterministic connector simulator and conformance tests
come with it, and a test plants a canary credential in the simulator and checks that it reaches no frame, event, log,
artifact, database row or runtime.

## Alternatives considered

- **A broker inside the control plane holding the secrets:** rejected. It would put OAuth secrets in dac-plane's
  database and process, and every external application's quirks in the product core. Providers are replaceable
  integrations everywhere else.
- **Adopt Nango, Composio or Arcade as the architecture:** rejected. None enforces dac-plane's policy, approvals,
  budgets or audit, and none is fully self-hostable without a commercial agreement. Any of them can sit behind a
  connector adapter.
- **Let the runtime call the connector adapter directly, with the adapter asking dac-plane for authority:** rejected.
  Enforcement would depend on every connector adapter asking, and the connector would have to accept runtime traffic.
  Through the gateway a call cannot skip policy, whatever the runtime or connector does.
- **Rely on the runtime's own tool approval:** rejected as the enforcement point. Codex can prompt before an MCP tool
  call, but that is the runtime's decision, not dac-plane's, and another runtime may not ask at all.
- **A signed, self-contained attempt token:** rejected. It could not be revoked before it expired without the same
  database check an opaque token needs.
- **Return at once for `ask` and make the model retry:** rejected as the only behaviour. Models retry unreliably; a
  bounded hold covers the common case, and the retry remains for a decision that takes longer.
- **Hold an `ask` call open without bound:** rejected. MCP clients time tool calls out, often at 60 seconds, and
  progress notifications do not reliably reset that timer.
- **Pin the newer MCP revision:** rejected for now. The pinned Codex offers `2025-06-18`, and the `2026-07-28`
  revision removes the initialize handshake and sessions that older clients use.

## Consequences

- A new schema version adds the connection, connection grant and attempt token tables. Under
  [ADR-0011](0011-roll-compatible-binaries-before-schema-migrations.md) the new binary serves the previous schema
  first, refusing connector operations as unavailable.
- The adapter kit gains the `connector` role, its methods and the `connector.gateway` capability in a minor version.
  The Codex adapter negotiates the capability and configures the gateway per thread. Adapters on earlier kits never
  receive the token, and a task with connection grants dispatched to one runs without connector access.
- Surface API v1 gains operations to register, list, read, authorize, grant, revoke and remove connections. They are
  additions, so the API stays v1.
- The connector adapter becomes a holder of user credentials and needs its own sealed storage, key handling and
  backup guidance.
- A connection acts as its owner at the external application. The external application cannot tell which agent made a
  call; dac-plane's audit can. An agent identity
  ([ADR-0020](0020-agents-are-product-records.md)) remains deferred.
- The gateway adds a public-facing MCP endpoint to the adapter listener, with its own authentication and rate limits.
- Until the runtime host's egress is limited, a runtime could still reach an external API directly with credentials it
  obtained elsewhere. The gateway governs connector credentials; it does not by itself confine the runtime.
