# Use the Surface API

The Surface API is how a surface (a web app, a chat bot, an IDE extension, a scheduler) submits work, answers the
agent, reviews results and follows what happens. It is HTTPS and JSON, versioned under `/v1`. The
[API reference](../reference/api.html) lists every operation and field; this page explains the rules they share.
[Getting started](../getting-started.md) uses most of them against a local stack.

## Authentication

Every request carries an access token from an issuer listed under `issuers` in the
[identity configuration](../reference/configuration.md#identity-configuration-file):

```text
Authorization: Bearer <access token>
```

The token's issuer and subject must be bound to a principal of the tenant in the path
([`provision bind`](../reference/cli.md#bind)). A principal reads and acts on the tasks it submitted; one granted
[tenant reader](../reference/cli.md#grant-reader) reads every task in the tenant. A token issued for the adapter
endpoint's audience is refused here.

## Paths

Everything belongs to a tenant: `https://HOST:8443/v1/tenants/{tenant}/...`. Identifiers are UUIDv7 strings.

| Area | Operations |
|---|---|
| Agents | Create an agent; list, read, revise and retire agents; read an earlier revision of one. |
| Tasks | Submit a task, to an agent or to providers you name; read it with its attempt, open attention and revisions; read one attempt or revision; request cancellation. |
| Attention | List open attention requests; read one; inspect an owner-only connector proposal; decide it. |
| Results | Read an artifact of a revision and download its content; review a revision: accept, request changes or reject. |
| Budgets | Complete a submission or rework that is waiting on a spend challenge. |
| Events | Read or stream a task's events, or the whole tenant's. |
| Administration | Migration passes for moving work between runtime harnesses, and revoking a standing authority grant. |

## Commands

A request that changes something is a command, and two headers make commands safe to retry and to race.

**`Idempotency-Key`** is required on every `POST` and `PUT`: 1 to 128 bytes you choose, such as a UUID. Repeating a command
with the same key returns the original result instead of acting twice, so a client can retry after a timeout without
submitting a task twice. Reusing a key with a different body is refused with `422`.

**`If-Match`** is required on commands that change an existing record: deciding an attention request, reviewing,
cancelling, completing a spend challenge, and revising or retiring an agent. It carries the record version you based the command on, from the `ETag`
of the response you read it in (attention requests also carry it as `version`). If the record changed since, the
command is refused with `412` and you read it again; without the header, `428`.

## Errors

Errors are [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) problem documents, `application/problem+json`, with a
`type`, a `title` and sometimes a `detail`. The ones a client should handle:

| Status | Meaning | What to do |
|---|---|---|
| `400` | Malformed body or header, or `Idempotency-Key` missing. | Fix the request. |
| `401` | No valid token. | Get a new token. |
| `403` | The token's principal may not do this. | Check the binding and permissions. |
| `404` | No such record, or one this principal may not read: the two look the same, so a response never reveals what it hides. | |
| `409` | The lifecycle refused the command in the record's current state; `detail` names the reason. Or another active agent already has the name. | Read the record; the command may no longer apply. Choose another name. |
| `412` | The record changed since you read it. | Read it again and decide again. |
| `422` | Valid JSON that is not a valid command, a reused `Idempotency-Key`, a provider with no qualified harness or registration, or an agent that does not exist or is retired. | Fix the command or the provisioning. |
| `428` | `If-Match` missing. | Send it. |
| `503` | The operation needs a database migration that has not run yet, during an upgrade. | Retry after the upgrade completes. |

## Agents

An agent is a named role your tenant delegates work to: a name, a role, optional standing instructions, and the
runtime and environment providers its work goes to by default. Submit a task to it by giving `agent_id` instead of the
providers; the task keeps the agent's revision at submission. [Delegate work to agents](agents.md) covers creating,
revising and retiring agents and what the runtime receives.

## Events

Every change to the record is an event: a submission, a dispatch, an attention request decided, an artifact
committed, a review. Each names the record, the event, and the state before and after.

Read them as pages of JSON, up to `limit` (default 100, at most 500) at a time, and pass the last page's
`next_cursor` as `after` to continue:

```text
GET /v1/tenants/{tenant}/tasks/{task}/events?after=CURSOR&limit=100
```

Or send `Accept: text/event-stream` to receive them as [Server-Sent Events](https://html.spec.whatwg.org/multipage/server-sent-events.html)
as they happen. Each event's `id` is its cursor, so a client that reconnects with `Last-Event-ID` resumes where it
stopped. If the cursor can no longer be honoured, the server sends a reset carrying the cursor to restart from; the
client then reads the record again rather than assuming it missed nothing.

`/v1/tenants/{tenant}/events` is the same for every task in the tenant the principal can read.

## Artifacts

A result revision lists its artifacts by identifier. Read one with
`GET /tasks/{task}/artifacts/{artifact}`: its kind, media type, size, SHA-256 digest, producer and commit time. Download
its bytes with `GET /tasks/{task}/artifacts/{artifact}/content`. Both are readable by whoever may read the task
([ADR-0025](https://github.com/dac-plane/dac-plane/blob/main/docs/adr/0025-serve-artifacts-to-surfaces-through-the-control-plane.md)).

The control plane relays the content from the artifact store, which a surface never reaches, and checks it on the way:

- The response carries the record's media type and length, its digest as `Repr-Digest: sha-256=:...:` and as the
  `ETag`, and is sent as an attachment with `X-Content-Type-Options: nosniff` and a sandboxing
  `Content-Security-Policy`. The bytes are agent output; open them in a viewer, not in the surface's own page.
- A store that answers with another length is refused with `502 artifact-mismatch` before anything is sent.
- Bytes whose digest differs are found while relaying, and the response is aborted before its last bytes. A response
  that ends early did not deliver the artifact; read it again.
- `503 artifact-unavailable` means the content cannot be read now: the store is not connected to this replica, does not
  offer downloads, refused, or the deployment has not set `DAC_ARTIFACT_DOWNLOAD_ORIGINS`. The record stays readable.

## Connections and grants

A connection names an external account whose credential stays with its connector
adapter. Register one with `POST /connections`, specifying `provider_id` and
`name`. The response includes the connection and, when the adapter is reachable,
an `authorization` URL. Open that URL to consent at the connector, then send
`POST /connections/{connection}/complete` with the connection's ETag in
`If-Match` and a JSON `confirmation` proof received only by the callback browser.
The proof is never returned by polling. Pending consent returns `409 authorization-pending`.

If the initial response has no URL, request one with
`POST /connections/{connection}/authorize` and an empty JSON object. This command
requires `Idempotency-Key`: retrying it returns the same URL, while a new key starts
consent again. It also renews an active connection's consent. Consent URLs are
returned only to the connection's owner.

The tenant can read `/connections`, `/connections/{connection}` and
`/connections/{connection}/tools`. Only the owner can grant access with
`POST /connections/{connection}/grants`. Supply exactly one of `agent_id` and
`task_id`, distinct `tools`, `permission` (`ask` or `always`), `call_limit` per task
and `expires_at`. An agent must be active; a task must belong to the connection's
owner. Grants apply only to work owned by that same principal.

Read grants at `/connections/{connection}/grants` or
`/connection-grants/{grant}`. Revoke one with
`POST /connection-grants/{grant}/revoke`, an empty JSON object, an
`Idempotency-Key` and its `If-Match` ETag. Remove a connection with
`POST /connections/{connection}/remove` using the same command headers.
Removal revokes its grants atomically. A `202` response means the connection is
`removing`: the adapter has not confirmed credential revocation. Retry removal
until it returns `200` with state `removed`.

Connector writes return `503 not-migrated` until schema 18 is installed. Reads
on the previous schema return empty connection lists or `404` for a named record.
