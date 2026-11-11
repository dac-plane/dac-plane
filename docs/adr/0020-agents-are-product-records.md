# ADR-0020: Agents are product records that tasks are delegated to

- Status: Proposed
- Date: 2026-11-11
- Issue: [#136](https://github.com/dac-plane/dac-plane/issues/136)

## Context

A surface that lets a person create agents, give each a role and work with them has nothing in dac-plane to store an
agent in. The Surface API has tasks, attempts, attention, reviews, budgets and events. A task names an owner, a runtime
provider and an environment provider, and carries an execution brief of an objective and acceptance criteria
([ADR-0013](0013-product-owned-execution-briefs.md)). An agent's name, role and standing instructions would have to
live in the surface's own database and be folded into each objective.

That has three costs. Two surfaces on one tenant cannot share an agent. The task record and its audit do not say which
agent the work was given to. Nothing can later be set per agent, such as authority, a budget or a connector grant
([#137](https://github.com/dac-plane/dac-plane/issues/137)).

ADR-0013 kept provider names, model identifiers and workspace configuration out of the brief and named them "future
product profile concerns". This decision defines that profile.

Three public designs were compared:

| Design | What an agent is | What a unit of work refers to |
|---|---|---|
| A hosted agent service | A named, versioned resource holding a system prompt, a model and tools. An update carries the version it was based on and is refused if stale. | A session names an agent, optionally at a version, and keeps a snapshot of it. |
| An agent-to-agent protocol's agent card | A published description: name, description, skills, capabilities and how to authenticate to it. | Nothing; the card is for discovery. |
| A directory's agent identity | A service principal created from a template. A token can name a user as its subject and the agent as its actor. | Nothing; it is identity only. |

The first separates a definition from an identity and pins work to a version. The third shows that identity can be
added beside a definition later. Today dac-plane's issuer gives tokens to people and to adapters only.

One constraint comes from the wire. The ControlFrame codec refuses unknown fields on both sides, so adding a field to
`start_turn` breaks an adapter built with an older kit unless the field is sent only when negotiated.

## Decision

**An agent is a tenant's record.** It holds a name, a role, standing instructions, a default runtime provider, a
default environment provider and a state of `active` or `retired`. Name, role and instructions are bounded text. The
name is unique among a tenant's active agents. It holds no model identifier, credential, tool list or vendor name;
those stay with provider registration and adapter configuration, as ADR-0013 decided for the brief.

**An agent has revisions.** Each change to its role, instructions or defaults produces the next revision, by a
compare-and-set on the revision the caller read. Earlier revisions are kept, because tasks refer to them.

**A task may name an agent.** Submission accepts an agent in place of the two providers, which then come from the
agent's defaults unless the request gives them. The task records the agent and the revision current at submission, and
that pair is immutable like the brief. Rework and migration-restart attempts use the same revision. A task submitted
without an agent stays valid, so existing clients keep working.

**The instructions reach the runtime with the brief.** `start_turn` carries the pinned revision's role and
instructions beside the brief, and only to an adapter that negotiated the `agent.instructions` capability. A
submission that names an agent whose runtime provider's adapter has not negotiated it is refused, so instructions are
never silently dropped.

**An agent is not a principal.** A task's owner is still the person or service that submitted it, and every command
is still authorised against that principal. The agent is what the work was given to, not who gave it. Audit entries
for the task carry the agent and revision.

**Every principal of a tenant may read its agents. The principal that created an agent may change or retire it.**
Retiring stops new submissions to the agent and leaves its tasks and revisions readable.

**Surface API v1 gains agent operations**: create, read, list, change and retire, with `Idempotency-Key` and
`If-Match` as on every other command. They are additions, so the API stays v1 and its document moves to 1.1.0.

**Deferred, with room left.** An agent's own identity, authority narrowed per agent, a budget per agent and connector
grants are not part of this decision. Each can attach to the agent record by its identifier without changing what is
decided here: an identity as a principal bound one-to-one to an agent, the others as records that name it.

## Alternatives considered

- **Keep agents in the surface:** rejected. It is the state this decision exists to end, with the three costs above.
- **Make an agent a principal now:** rejected for this step. It needs the issuer to mint agent tokens and a rule for
  an agent acting for a user, which is identity work with its own risks. A definition is useful without it, and the
  identity can be bound to the record later.
- **Let a task follow the agent's latest revision:** rejected. An instruction change would alter work already
  accepted for delegation, and a rework attempt would not repeat the conditions of the attempt it reworks.
- **Put the model or tools in the agent:** rejected. Both name a runtime's choices, and a harness migration moves a
  task between runtimes.
- **Fold the instructions into the objective at submission:** rejected. The objective is what the owner asked for and
  is shown back to them; mixing in standing instructions would hide which text came from where.
- **Send the new field to every adapter:** rejected. The codec refuses unknown fields, so an adapter on the current
  kit would drop its connection.

## Consequences

- A new schema version adds the agent and agent revision tables and two nullable task columns. Under
  [ADR-0011](0011-roll-compatible-binaries-before-schema-migrations.md) the new binary serves the previous schema
  first, where agent operations are refused as unavailable and tasks without an agent work as before.
- The adapter kit gains the capability and the field in a minor version. The Codex adapter negotiates it and places
  the instructions where its runtime takes standing instructions. The simulator negotiates it and records what it
  received.
- New records need JSON Schema contracts and agreement tests, and the OpenAPI document and user guide change with
  them.
- Agents are visible across a tenant. A tenant that needs private agents has no way to express that yet.
- Until an agent has an identity, an external system the agent reaches cannot tell agents of one tenant apart. #137
  must either accept that or bring the identity decision forward.
