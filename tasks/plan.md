# List tasks and providers

Tracker: https://github.com/dac-plane/dac-plane/issues/155, https://github.com/dac-plane/dac-plane/issues/156
Branch: `feat/155-156-task-and-provider-lists`, from `main` at `dae26d7`.
Decision: `docs/adr/0028-list-tasks-by-creation-and-providers-by-role.md`.
Needed by: the reference application (#97), which today replays the event stream to list tasks and takes provider
identifiers from its own configuration.

## Where the tree stands

| Finding | Evidence | Consequence |
|---|---|---|
| No task list; one task is read by identifier | `GET /tasks/{task}`; `POST /tasks` is the only collection route | `GET /tasks` is added beside it. |
| A reader's visibility is already one filter | `reader.owner()`, `reader.task`, `ListOpenAttention` | The owner filter goes in the query, so a page never holds an unreadable task. |
| A task's creation time never changes | `task.created_at` | A keyset on `(created_at, task_id)` is stable under submissions; an offset or `updated_at` order is not. |
| A registration has no name | `provider_registration`: tenant, provider, role, artifact store, update time | A person cannot choose between two runtimes; an optional name is added, set by provisioning. |
| Connection state lives in one replica | `adapterwire` registry | "Connected" is not listed; the harness mode is the readiness a person needs. |
| The probe migration holds the next version | `operationstest/00020_rollout_probe.sql` | It moves to 00021, with the version constants and tests that follow it. |

## Stages (one PR)

1. ADR-0028, migration 00020 (`display_name`, `task_listing` index), `-name` on `provision register-provider`.
2. Store: `ListTaskPositions`, `ListProviderListings`, name on `PutProviderRegistration`; schema-19 behaviour.
3. Application and API: `GET /tasks`, `GET /providers`; schemas, OpenAPI, guide, CLI reference.
4. Tests: stability under submissions, scoping, filters, provider fields, schema 19, contract agreement.

## Acceptance

- A surface lists the tasks a principal can read, newest first, page by page.
- Another principal's tasks are absent unless it is a tenant reader.
- A page boundary does not skip or repeat a task when tasks are submitted between pages.
- A surface lists the tenant's runtime and environment providers with what a person needs to choose one.
- Nothing operator-only appears in the provider list, shown by a test.
