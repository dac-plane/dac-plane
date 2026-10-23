# Issue #70 — Expose Surface API v1 over the application commands

Tracker: https://github.com/dac-plane/dac-plane/issues/70
Branch: `feature/70-surface-api`, from `main` at `04eece8`, in the main checkout.
Parent: #65 (M2: Deployable control plane).

## Direction

Implement the Surface API ADR-0007 decided, as a thin HTTP layer over the existing
transport-neutral commands in `internal/application`, authenticated by #71's `authn.Authenticator`.
Handlers hold no lifecycle rules and never touch the store; a read the application layer lacks is
added there first.

Settled before this plan, and not reopened:

- ADR-0007: JSON over HTTPS; `/v1/tenants/{tenant}/…`; `Idempotency-Key` as the command nonce;
  `ETag`/`If-Match` as the expected version; SSE with generation-aware cursors and an explicit reset
  event; RFC 9457 problems; owner-scoped reads plus the tenant-reader permission; one uniform
  refusal for cross-tenant and hidden resources.
- ADR-0009: `serve`, TLS, the operations listener and process wiring are #73. #70 delivers an
  `http.Handler` and tests it end to end through `httptest`.
- `architecture-explore` evidence: the prototype contract
  (`packages/product-api/openapi.yaml`) is a hand-written OpenAPI 3.1 document referencing separate
  JSON Schemas, with no generator; spec §15 lists only lifecycle routes and returns 202 for
  cancellation; spec §10 requires a monotonic per-task sequence with no global order guarantee.
- Issue #70 scope: core lifecycle routes. Migration administration and authority revocation go to a
  follow-up issue.

Confirmed with the maintainer on 2026-10-23: the tenant-wide stream is ordered by a transaction
horizon (position plus writing transaction ID, read below `pg_snapshot_xmin`).

Industry practice checked (Tavily, 2026-10-23):

- RFC 9110 / RFC 6585: a missing `If-Match` on a conditional command is **428**, a stale one **412**
  (ADR-0007 said only "refused" and "conflict"; these are the standard codes for it).
- Idempotency-Key draft (expired at -07, still the common convention): missing key **400**, key reused
  with a different payload **422**; an in-flight duplicate would be 409, but the command record
  already makes a concurrent duplicate wait and replay, so 409 does not arise.
- PostgreSQL commit order differs from sequence order; readers that trust a plain sequence skip late
  commits. The standard non-blocking fix stores the writer's `xid8` and reads only below
  `pg_snapshot_xmin(pg_current_snapshot())`.
- SSE: `id:` per event, `Last-Event-ID` resume, comment heartbeats every 15–30 s,
  `Cache-Control: no-cache, no-transform`, `X-Accel-Buffering: no`, flush per event, stop on client
  disconnect, validate `Last-Event-ID`, authenticate at connection time, bounded replay batches.
- oapi-codegen labels OpenAPI 3.1 beta; a hand-written contract with conformance tests avoids it.

### Event ordering: commit-ordered positions (supersedes the transaction-horizon choice)

Industry model (Marten, EventStoreDB/Kurrent, Emmett, outbox guidance): one log with a global position
that catch-up subscriptions checkpoint on, filtered views of one entity on the same position, and a
gapless per-record revision for concurrency. dac-plane already has the per-record revision (`version`,
one durable event per record version).

The transaction-horizon read the maintainer chose (position plus `xid8`, read below
`pg_snapshot_xmin`) turned out to be insufficient during implementation: a transaction with an older
id can insert a *higher* position and commit while a younger one with a lower position is still
running, so a cursor ordered by position still skips; ordering by (`xid8`, position) avoids skips but
can deliver a record's version v+1 before v, because a command transaction obtains its id when it
writes its command record, before it locks the record it changes.

Adopted instead: **commit-time sequencing**. A deferred constraint trigger fires at `COMMIT`, takes a
per-tenant transaction advisory lock, and draws the next position into an append-only
`event_position` table. PostgreSQL publishes a commit before releasing the committing transaction's
locks, so within a tenant positions become visible strictly in commit order and `position > cursor`
never skips and never inverts a record's versions. The lock is the last one a transaction takes, so it
adds no lock-order inversion; it serializes only the commit tails of one tenant's writers.
`durable_event` stays immutable. Evidence: `TestStreamEventsArriveInCommitOrderWithoutSkips` (40
writers committing out of insert order, reader by cursor) sees all 40 in order; with insert-time
positions the same test misses 7 of 40.

The prototype's task-row counter is not adopted: incrementing a task row on every event would add a
task lock to the M1 paths that lock attempts, attention requests and operations without the task.

## Implementation

1. **Event stream storage** (migration `00011_surface_events.sql`)
   - [x] Append-only `event_position (tenant_id, position, event_id, task_id)` filled at commit by a
     deferred constraint trigger under a per-tenant advisory lock; existing events backfilled in
     occurrence order; `durable_event` untouched and still immutable.
   - [x] Single-row `event_stream` holding the stream generation (starts at 1). A cursor whose
     generation differs, or whose position is beyond the current head, gets a reset. The restore
     runbook (#73/#74) advances the generation; nothing else does.
   - [x] `durable_event_task` resolves the task of task-scoped records (task, attempt, attention
     request, provider operation, budget, reservation, spend challenge, artifact, authority grant and
     use) by reading rows the transaction already wrote; no Go change and no new row lock.
   - [x] Downgrade refuses once surface events exist, following the existing pattern.
2. **Application reads** (`internal/application/read.go`)
   - [x] Authorize every read: the principal owns the task, or holds the tenant-reader permission
     under `LockTenantReader`. Anything else is `ErrNotFound`, identical to a missing record.
   - [x] `GetTask` (task, current attempt, open attention summary, budget and reservation, spend
     challenge, latest revision and review references), `GetAttempt`, `GetAttention`,
     `ListAttention` (open requests on readable tasks), `GetRevision` and its review.
   - [x] `ListEvents(scope task|tenant, cursor, limit)`: rows with `position > cursor` in position
     order, bounded batch; returns the next cursor, or a reset continuing from the tenant head.
3. **Contract** (`contracts/openapi/surface-v1.yaml`, `contracts/jsonschema/surface/`)
   - [x] Hand-written OpenAPI 3.1: paths, `Idempotency-Key`/`If-Match`/`ETag` headers, bearer security
     scheme, problem responses, `text/event-stream` and `application/json` for events.
   - [x] Request and response body schemas (submit, spend assertion, decision, cancel, review, task
     view, attention view, event envelope and page, problem) referencing existing record schemas. The
     event envelope carries cursor, record kind, record id, record version, event, from and to state,
     task id when task-scoped, and time.
   - [x] Agreement tests for every body schema; a conformance test that validates the document against
     the vendored OpenAPI 3.1 meta-schema with the existing JSON Schema library, and that every
     registered route and status is documented and every documented route is registered.
4. **HTTP layer** (`internal/httpapi`)
   - [x] Go standard `ServeMux` patterns; `authn.BearerToken` + `Authenticate` per request with the
     tenant from the path; body size limit; strict JSON decoding (unknown fields rejected); server
     clock supplies command time.
   - [x] Routes:

     | Method and path (under `/v1/tenants/{tenant}`) | Application call | Success |
     |---|---|---|
     | `POST /tasks` | `Submit` | 201 + `Location`, `ETag` |
     | `GET /tasks/{task}` | `GetTask` | 200 + `ETag` |
     | `POST /tasks/{task}/spend-assertions` (`If-Match` challenge version) | `Assert` | 200 |
     | `POST /tasks/{task}/cancel` (`If-Match` task version) | `Cancel` | 202 |
     | `POST /tasks/{task}/reviews` (`If-Match` task version) | `ReviewResult` | 201 |
     | `GET /tasks/{task}/attempts/{attempt}` | `GetAttempt` | 200 + `ETag` |
     | `GET /tasks/{task}/revisions/{revision}` | `GetRevision` | 200 |
     | `GET /attention` | `ListAttention` | 200 |
     | `GET /attention/{attention}` | `GetAttention` | 200 + `ETag` |
     | `POST /attention/{attention}/decisions` (`If-Match` attention version) | `Decide` | 201 |
     | `GET /tasks/{task}/events` | `ListEvents(task)` | 200 SSE or JSON page |
     | `GET /events` | `ListEvents(tenant)` | 200 SSE or JSON page |

   - [x] Error mapping, one table with a test per row: malformed JSON 400; missing
     `Idempotency-Key` 400; unauthenticated 401; principal not authorized 403; missing or hidden 404;
     lifecycle refusal 409; stale `If-Match` 412; invalid command or payload mismatch 422; missing
     `If-Match` 428; operational failure 500 with no detail. Bodies never echo tokens or inputs.
   - [x] SSE writer: cursor as `id:`, `Last-Event-ID` or `after` query resume, explicit `reset` event,
     heartbeat comments, flush per event, poll the read with backoff, stop on disconnect or token
     expiry, no-buffering headers.
5. **Shared test issuer**
   - [x] Move #71's in-test OIDC issuer into `internal/testsupport/oidctest` so `authn` and `httpapi`
     tests share it, unchanged in behaviour.
6. **Tests and CI**
   - [x] Integration (PostgreSQL, simulator, worker): the full lifecycle through HTTP — submit, worker
     dispatch, attention appears in the stream, decide, result, review accept and request changes,
     cancel — asserting statuses, `ETag`s and stream contents.
   - [x] Idempotent replay returns the original response; changed payload 422; stale and missing
     `If-Match` 412 and 428; another tenant's task and an unowned task 404 alike; tenant reader sees
     tenant stream and others' tasks, non-reader does not; revocation takes effect on the next request.
   - [x] Event ordering: concurrent transactions committing out of order are delivered in position
     order without skips; reset on foreign, future and other-generation cursors; resume from
     `Last-Event-ID`.
   - [x] `just test-surface` and `just test-surface-race`; contract tests named to run under
     `just test-contracts`.
7. **Follow-up**
   - [x] Open an issue for migration administration and authority revocation routes under distinct
     permissions (ADR-0007), linked from #65.

## Deviations and findings during implementation

- Event ordering uses commit-time sequencing (see above) instead of the transaction-horizon read, and
  positions live in `event_position`, so `durable_event` stays immutable (an in-place backfill would
  have been refused by its immutability trigger on any non-empty database).
- The OpenAPI document is JSON, not YAML, so no YAML dependency is added; it is checked structurally
  (3.1.x, bearer security, operation ids, documented 401/403/500, Idempotency-Key and If-Match with
  412/428, every reference resolves and every referenced schema compiles, routes match the server both
  ways) rather than against the official OpenAPI meta-schema, which would need a download.
- M1 contract bug fixed: `lifecycle/attempt.schema.json` tested `runtime_stopped`/`environment_stopped`
  in an `if` without requiring them, so the condition held whenever they were absent and every running
  attempt was rejected. Both branches now require the flag; a regression test covers an executing
  attempt. A scan of every schema found no other vacuous condition.
- Commands on a task or attention request the actor cannot read return the same 404 as a missing
  record (found by the end-to-end test: a review by an unrelated principal returned 409
  `unauthorized_actor`). A lifecycle `unauthorized_actor` refusal on a visible record is 403.
- Both stale-version cases (decision, cancellation) return exactly 412.
- #71's in-test OIDC issuer moved to `internal/testsupport/oidctest`; `authn` tests use it unchanged in
  behaviour.
- `CLAUDE.md` working set updated for `authn`, `provision`, `httpapi` and `contracts/openapi`.

## Review follow-ups (PR #84)

- Cursors carry a stream tag (a short digest of tenant and task, or tenant scope), so a cursor from
  another task's stream, the tenant stream or another tenant is reset instead of skipping history.
- The migration backfill orders existing events by the latest time among each record's versions up to
  them, then record and version, so a record's versions never replay backwards; the sequence continues
  after the backfill.
- The SSE loop clears a page once written, so a heartbeat between polls never re-sends events or a
  reset.
- Request bodies and the identity configuration must be exactly one JSON object: `null`, arrays and
  trailing tokens are malformed.
- The end-to-end setup rolls back its seeding transaction on failure.

## Verification

- `just test-contracts`, `just test-surface`, `just test-surface-race`, `just ci`
- `git diff --check`

## Remaining boundaries

No listener, TLS, `serve` subcommand or health endpoints (#73); no adapter endpoint (#72); no administrative routes (#83); no web
workspace or client SDK; no artifact byte download, message injection, retry or capabilities routes
from the historical spec, which are outside #70's scope; no retention policy for events.
