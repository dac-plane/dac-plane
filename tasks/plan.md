# Define agents in dac-plane and delegate work to them

Tracker: https://github.com/dac-plane/dac-plane/issues/136
Branch: `feat/136-agent-wire`, from `main` at `1a74751`. This branch carries steps 5 and 6 together: the wire
capability with the adapters, then the guide and the upgrade scenario. Step 2 merged in #139, step 3 in #141 and step 4
in #142. v0.2.0 is released (#134).

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

For step 5:

- `RuntimeStart` gains an optional `agent` object of `role` and `instructions`, the pinned revision's text and nothing
  else: no agent identifier, revision or name reaches the runtime, as ADR-0020 states. Its JSON Schema reuses the
  profile's `agentRole` and `agentInstructions` definitions, and `lifecycle.ValidateAgentDirection` holds the bounds
  for both.
- The capability is `agent.instructions`. The control plane's hello offers it beside `operation.replay`; the kit
  exports `wire.CapabilityAgentInstructions` and types no `start_turn` arguments, so the kit change is the constant
  and its documentation only.
- Capabilities are per connection, so a registration can mix kits during a roll. `contracts.CapabilityScoped` lets the
  worker restrict a port to connections that selected a capability; the registry's restricted port sends only there,
  and replays only through a connection that selected both. A draining connection counts, so a token renewal releases
  the send instead of failing the task.
- The recorded outbox arguments stay as submitted. The worker adds the field per send, toward a negotiated connection
  only, from the pinned revision, which is immutable, so every delivery of an operation carries the same bytes.
- With no connection that negotiated it, a delegated `start_turn` is still refused as `agent_instructions_unsupported`
  and a delegated migration restart is still held. This keeps step 4's dispatch-time refusal instead of ADR-0020's
  refusal at submission, because adapter connections come and go and submission cannot see which will carry the turn.
- The Codex adapter offers the capability and passes the text to `thread/start` as `developerInstructions` (present in
  app-server 0.165.0), keeping the brief as untrusted turn input. The simulator's runtime offers it by default, its
  ledger shows what it received, and `dac-sim-adapter` offers it; the in-process wire harness advertises a simulator's
  hello so tests choose either side.
- The conformance suite is role-generic and stays as it is: that an adapter without the capability never receives the
  field is the control plane's guarantee, proven by the registry and worker tests.
- The 64 KiB arguments limit is unchanged. A brief and instructions that together exceed it fail the task as
  `invalid_dispatch_body`, which the guide states.

For step 6:

- `docs/guides/use/agents.md` is the agents guide; the Surface API page points to it, and the adapter guide documents
  the capability.
- The rolling-upgrade scenario from v0.2.0 checks `503 not-migrated` for an agent write while both replicas run the
  new binary on schema 16, then creates an agent on schema 18, delegates a task and reads from the simulator's control
  ledger that the reconnected runtime received the instructions.

## Steps

1. ADR-0020. Done in #138.
2. Contracts: the agent and revision records, their transitions and audit, JSON Schemas and agreement tests. Done in #139.
   Verify: `just test-contracts`, `just ci-fast`, `just docs-check`.
3. Persistence: the migration, the store, the rollout probe moved up one version. Done in #141.
   Verify: `just test-migration`, `just test-operations-fixture`, `just test-integration`, `just ci-fast`.
4. Application and Surface API: the agent operations, submission to an agent, OpenAPI 1.1.0. Done in #142.
   Verify: `just test-integration`, `just test-contracts`, `just docs-check`, `just ci-fast`.
5. Wire and adapters: the capability, the kit field, the Codex adapter and the simulator.
   Verify: `just test-contracts`, `just test-conformance`, `just test-codex-adapter`, `just test-integration`.
6. The user guide, and the upgrade scenario exercising the new schema.
   Verify: `just docs-check`, `just test-operations-upgrade`, `just ci-fast`.
