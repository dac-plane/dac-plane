# Define agents in dac-plane and delegate work to them

Tracker: https://github.com/dac-plane/dac-plane/issues/136
Branch: `feat/136-agent-surface`, from `main` at `4a8485f`. This branch carries step 4, the application commands
and the Surface API. Step 2 merged in #139 and step 3 in #141. v0.2.0 is released (#134).

## Where the tree stands

Surveyed on 2026-11-11.

| Finding | Evidence | Consequence |
|---|---|---|
| Nothing names an agent | `contracts/openapi/surface-v1.json` has task, attention, event, migration and grant paths only | A new record and new operations. |
| The brief is objective and acceptance criteria, immutable | `internal/lifecycle/task.go`, ADR-0013 | Standing instructions travel beside the brief, not inside the objective. |
| ADR-0013 left a "product profile" for provider choices | `docs/adr/0013-product-owned-execution-briefs.md` | The agent is that profile: it holds the default providers. |
| Principals are `human` or `service` | `internal/identity/principal.go` | An agent is not a principal in this step. |
| Policy is per tenant; a budget is per task | `internal/identity/authority.go`, `internal/lifecycle/budget.go` | Per-agent authority and budgets are deferred, attachable by agent identifier. |
| The wire codec refuses unknown fields | `adapterkit/wire/codec.go`, `internal/adapterwire/connection.go` | The new `start_turn` field is sent only under a negotiated capability, as `operation.replay` is. |
| A new binary must serve the previous schema | ADR-0011 | Agent operations are unavailable until the migration runs; tasks without an agent are unaffected. |

## Decisions

Recorded in `docs/adr/0020-agents-are-product-records.md`, accepted by the project owner on 2026-11-11.

For step 2:

- The profile (name, role, instructions, default providers) is revisioned as a whole. `Revision` counts profile
  changes and `Version` every change, so retiring advances `Version` alone.
- Only the creating principal revises or retires; another actor is refused as `unauthorized_actor`, and the refusal
  is auditable like any other.
- A task carries an optional agent reference. Nothing sets it until step 4.
- `agent` joins the audit record kinds here. The SQL check constraints that list record kinds change in step 3, and
  nothing writes the kind before then.

For step 3:

- Migration 17 adds `agent` and `agent_revision`, the task's `agent_id` and `agent_revision` (both or neither), and
  `agent` to the record kinds of `audit_event`, `durable_event` and `outbox`. Its down migration refuses while any agent
  exists. The operations probe moves to 18.
- The database enforces what it can without the application: profile bounds, an active name unique per tenant,
  default providers registered in the runtime and environment roles, revisions append-only, and a task's reference
  naming an existing revision.
- On schema 16 an agent write or a task naming an agent is `ErrSchemaUnavailable`, and an agent read is `ErrNotFound`.
  Task reads take the new columns through the row's JSON form, as they do the brief, so one query serves both.
- Creating an agent writes no audit row of its own. Like a task's creation, it will be recorded by the command record
  in step 4. Revising and retiring are audited transitions with durable events outside any task.
- A name another active agent holds is `ErrAgentNameTaken`, checked before the update so the transaction stays usable;
  the unique index still decides between concurrent writers.

For step 4:

- Commands `create_agent`, `revise_agent` and `retire_agent`, idempotent like every other command. Revise and retire
  take the agent's version in `If-Match`; revise replaces the whole profile, so it is `PUT /agents/{agent}`, and
  retire is `POST /agents/{agent}/retire`, as cancel is.
- Every principal of the tenant reads its agents: `GET /agents`, `GET /agents/{agent}` and
  `GET /agents/{agent}/revisions/{revision}`, the last so a surface can show the profile a task was delegated to. The
  list is the tenant's agents, retired ones included, without paging, as the attention list is.
- `POST /tasks` takes `agent_id`. With it the providers are optional and default to the agent's current revision;
  without it both stay required. The agent row is read `for share`, so a concurrent retire either waits or is
  waited on, and a retired or missing agent is `no-agent` (`422`). The fingerprint carries the request as given, and
  the submission record carries the agent, the pinned revision and the resolved providers.
- Rework and migration restarts keep the task's pinned revision; nothing in them changes.
- New problems: `agent-name-taken` (`409`), `no-agent` and `no-runtime-provider` (`422`), and `not-migrated`
  (`503`, for `ErrSchemaUnavailable` during an ADR-0011 rollout).
- The runtime does not receive the role and instructions until step 5 adds the negotiated field, so until then the
  worker refuses a delegated task's `start_turn` before sending it (`agent_instructions_unsupported`), which fails the
  task, and holds a delegated migration restart unsent. `main` stays releasable; step 5 lets the send through to an
  adapter that negotiated `agent.instructions`.
- The guide's Surface API page lists the new operations and problems now; the agents guide is step 6.
- A task's audit entries carry its agent and revision: migration 17 adds the columns to `audit_event`, and a trigger
  fills them from the task row for every task entry, applied or refused, whichever binary writes it. Migration 17 is
  unreleased (v0.2.0 ends at 16), so it is amended rather than followed by another.

## Steps

1. ADR-0020. Done in #138.
2. Contracts: the agent and revision records, their transitions and audit, JSON Schemas and agreement tests. Done in #139.
   Verify: `just test-contracts`, `just ci-fast`, `just docs-check`.
3. Persistence: the migration, the store, the rollout probe moved up one version. Done in #141.
   Verify: `just test-migration`, `just test-operations-fixture`, `just test-integration`, `just ci-fast`.
4. Application and Surface API: the agent operations, submission to an agent, OpenAPI 1.1.0.
   Verify: `just test-integration`, `just test-contracts`, `just docs-check`, `just ci-fast`.
5. Wire and adapters: the capability, the kit field, the Codex adapter and the simulator.
6. The user guide, and the upgrade scenario exercising the new schema.
