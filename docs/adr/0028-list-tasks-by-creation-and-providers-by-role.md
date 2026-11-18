# ADR-0028: List tasks newest first by creation, and list providers a surface may name

- Status: Accepted
- Date: 2026-11-18
- Issues: [#155](https://github.com/dac-plane/dac-plane/issues/155), [#156](https://github.com/dac-plane/dac-plane/issues/156)

## Context

The Surface API reads one task by identifier and lists open attention requests, but cannot list tasks, so the reference
application replays the tenant's event stream from the start to learn which tasks exist. It also has no read of provider
registrations, so a surface is configured with provider identifiers out of band. A registration holds a tenant, a
provider, a role, the artifact store a runtime exports to and an update time; nothing a person could recognise.

## Decision

**`GET /tasks` lists tasks newest first by creation and pages by a keyset on `(created_at, task_id)`.** A creation time
never changes, so a page boundary names a fixed position: a task submitted between two pages sorts before the first
page's start and is never met while walking older ones, and a task whose state changes does not move. The response
carries `next_cursor`, an opaque value the client sends back as `cursor`, and omits it on the last page. `limit` is 1 to
200 and defaults to 50. Filters are `state` and `agent`. An offset or an `updated_at` order was rejected: both move
rows between pages as the data changes. "Updated since" is what the event stream already answers.

**Visibility is the task read's.** A principal lists the tasks it owns; a tenant reader lists the tenant's. The owner
filter is part of the query, so a page never carries a task the principal may not read and a cursor reveals nothing.
Each task is the same record `GET /tasks/{task}` returns for the task itself, without its attempt or revisions.

**`GET /providers` lists the runtime and environment registrations a surface may name**, optionally by `role`. Each
entry has `provider_id`, `role`, an optional `name` and, for a runtime, `harness_mode` (`off`, `shadow` or `live`),
the readiness a person needs: work is steered only through a live harness. Every principal of the tenant may read it,
as for agents. Adapter bindings (issuer and subject), the artifact store a runtime exports to, and whether an adapter
is connected stay operator-only: a connection lives in one replica's memory, so no replica can answer for the others.

**A registration gains an optional `name`**, set with `provision register-provider -name`: one line of at most 100
characters. Registering a provider again without `-name` keeps its name. Without a name a surface can offer only
identifiers, which does not let a person choose between two runtimes.

## Consequences

- Migration 00020 adds `provider_registration.display_name` and an index for the task listing. A binary on schema 19
  lists tasks (the index only speeds it), lists providers without names, and refuses `-name` until the migration runs
  (ADR-0011).
- A page can hold up to 200 task reads. The listing reuses the single-task read per row; a join is the next step if a
  tenant's pages prove slow.
- A surface learns changes from the event stream and the list is a starting point, not a feed.
