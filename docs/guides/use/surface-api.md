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
| Tasks | Submit a task; read it with its attempt, open attention and revisions; read one attempt or revision; request cancellation. |
| Attention | List open attention requests; read one; decide it. |
| Results | Review a revision: accept, request changes or reject. |
| Budgets | Complete a submission or rework that is waiting on a spend challenge. |
| Events | Read or stream a task's events, or the whole tenant's. |
| Administration | Migration passes for moving work between runtime harnesses, and revoking a standing authority grant. |

## Commands

A request that changes something is a command, and two headers make commands safe to retry and to race.

**`Idempotency-Key`** is required on every `POST`: 1 to 128 bytes you choose, such as a UUID. Repeating a command
with the same key returns the original result instead of acting twice, so a client can retry after a timeout without
submitting a task twice. Reusing a key with a different body is refused with `422`.

**`If-Match`** is required on commands that change an existing record: deciding an attention request, reviewing,
cancelling and completing a spend challenge. It carries the record version you based the command on, from the `ETag`
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
| `409` | The lifecycle refused the command in the record's current state; `detail` names the reason. | Read the record; the command may no longer apply. |
| `412` | The record changed since you read it. | Read it again and decide again. |
| `422` | Valid JSON that is not a valid command, a reused `Idempotency-Key`, or a provider with no qualified harness or environment. | Fix the command or the provisioning. |
| `428` | `If-Match` missing. | Send it. |

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

A result revision lists its artifacts by identifier. Version 1 of the Surface API does not serve artifact bytes; they
stay in the artifact store.
