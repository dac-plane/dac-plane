# Connect external applications

A connection lets work use an external account while its credential stays with a
connector adapter. The surface and the runtime receive references and safe
results. The control plane owns each grant, approval, limit and audit entry.

## Register and authorize

Register a provider in the `connector` role and bind its adapter identity using
the normal provisioning commands. Create a connection with the
[Surface API](surface-api.md#connections-and-grants), open its authorization URL,
and complete consent through the connector. Complete the connection in the
Surface API with the `confirmation` proof returned only to the callback browser.
Forwarding an authorization URL does not give its sender this proof. The response identifies the external
account and its reported permissions.

Only the connection's owner grants or removes it. A grant names one task or one
active agent, its tools, `ask` or `always`, an expiry and a maximum number of calls
per task. Agent grants cover only tasks owned by the connection's owner; delegating
a task as another principal does not grant access to that owner's account.

## Calls and approvals

Configure the [gateway URL and signing key](../reference/configuration.md#connector-gateway).
A runtime that negotiates `connector.gateway` receives an attempt bearer outside
its prompt. The Codex adapter installs it as the authorization header of a
per-thread MCP server and configures it again when that thread resumes.
Adapters that do not negotiate the capability receive no gateway field.

The runtime sees tools belonging to active connections granted to its work. Each
name combines its connection ID and tool name. Calling a tool rechecks the
connection, ownership, grant, expiry, tenant policy and task call ceiling.
An unavailable connector is omitted from `tools/list`; healthy connections remain
available and operators receive a warning with connection and provider IDs.
Explicit tenant denials always win. Authorized calls record `AuthorityUse`, audit,
a durable dispatch and the exact input digest in one transaction.

`ask` creates an action-approval attention request. Approve or deny it through the
normal Surface API attention decision. Before deciding, the task owner reads
`GET /v1/tenants/{tenant}/attention/{attention}/connector-call` to inspect the
connection, tool and exact proposed arguments. Its input digest matches the
attention request; use its ETag as the decision's `If-Match`. Other principals,
including tenant readers, cannot read these arguments. Approval covers the exact tool, connection
and input; changed input needs another approval. The gateway waits up to two
minutes, sending progress when requested. If it times out, check the task's pending
approval and retry the same tool and arguments. Expired or revoked pending waits
are closed by the worker. Denying a waiting request ends that request even if
another covering grant becomes available.

Revoking a grant blocks further calls during a running attempt. Removing a
connection also revokes all its grants atomically and records `removing` until
credential destruction is confirmed. The reconciliation loop finishes removals
after a process restart. Retry batches rotate through the backlog so unavailable
adapters do not block later removals. Already dispatched calls receive cancellation requests;
requests that may have reached the external service remain uncertain until there
is evidence of their outcome. The worker reads recorded results with `read_call`
and never repeats an unknown `call_tool` effect.

## Reference GitHub connector

Build `adapters/github/cmd/dac-github-adapter` from its Go module, or build the
Docker target `dac-github-adapter`. Configure a GitHub App with the exact callback
URL, expiring user access tokens, and only the repository permissions needed for
its tools. The connector offers `get_issue`, `create_comment` and
`list_repositories`; each returns selected fields rather than an unrestricted
GitHub response. Repository listing returns the first 30 accessible repositories.

The App OAuth web flow uses a single-use state and PKCE. Its user access and refresh
tokens live in the connector's encrypted vault. Renewal retains prior credentials
until removal confirms their revocation; it never drops a still-live credential.
Removal revokes each stored access token and destroys its local refresh token.
An ambiguous remote write remains uncertain after restart and is never resent.

See [connector configuration](../reference/configuration.md#dac-github-adapter).
Create the vault key as exactly 32 random bytes and mount it separately from the
vault. Keep encrypted backups of the vault and protect the key through the
substrate's secret mechanism. Losing the key loses access to the sealed records;
rotating it requires an explicit decrypt-and-reseal maintenance operation, which
this adapter does not automate. Do not delete the vault as a substitute for
remote token revocation.

Run `just test-github-adapter` for deterministic OAuth, refresh, encryption,
revocation, cancellation, projection and unknown-effect checks against a fake
GitHub server. `just test-conformance` also exercises the connector role over the
published adapter protocol. These checks require no live credential. The live
adapter recipe is described in the module README.

## Runtime network boundary

Restrict the runtime adapter host's egress outside the agent process to the
configured model provider and dac-plane. Connector adapters have separate egress
to their external applications. Keep sandbox commands without internet access.
The reference Compose network does not enforce a hostname allowlist for the
runtime host: enforce it in the deployment firewall or egress proxy, then verify
that a direct runtime-host request to GitHub is blocked while model and gateway
requests work. A simulator pass is not evidence of that deployment boundary.
