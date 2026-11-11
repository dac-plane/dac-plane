# Define agents in dac-plane and delegate work to them

Tracker: https://github.com/dac-plane/dac-plane/issues/136
Branch: `feat/136-agent-contracts`, from `main` at `aa2de04`. This branch carries step 2, the contracts.

The v0.2.0 release (#134) is still in progress beside this; its remaining steps are in `docs/releases/README.md`.

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

## Steps

1. ADR-0020. Done in #138.
2. Contracts: the agent and revision records, their transitions and audit, JSON Schemas and agreement tests.
   Verify: `just test-contracts`, `just ci-fast`, `just docs-check`.
3. Persistence: the migration, the store, the rollout probe moved up one version.
4. Application and Surface API: the agent operations, submission to an agent, OpenAPI 1.1.0.
5. Wire and adapters: the capability, the kit field, the Codex adapter and the simulator.
6. The user guide, and the upgrade scenario exercising the new schema.
