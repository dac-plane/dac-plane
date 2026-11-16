# Delegate work to agents

An agent is a named role your tenant gives work to. It holds a name, a role, optional standing instructions, and the
runtime and environment providers its work goes to by default. A surface creates agents once and then submits tasks to
them, so two surfaces on one tenant share the same agents, and every task records which agent it was given to.

An agent is not a principal. A task delegated to an agent is still owned by whoever submitted it, and every command is
authorised against that principal. The agent holds no model, credential or tool list: those stay with the runtime's
adapter configuration ([ADR-0020](https://github.com/dac-plane/dac-plane/blob/main/docs/adr/0020-agents-are-product-records.md)).

The examples continue from [Getting started](../getting-started.md) and reuse its `api`, `auth`, `base`, `runtime`,
`environment` and `deadline` variables.

## Create an agent

```bash
agent=$(curl -fsS "${api[@]}" "${auth[@]}" -X POST "$base/agents" \
  -H "Idempotency-Key: $(uuidgen)" -H 'Content-Type: application/json' \
  -d '{
    "name": "Release notes writer",
    "role": "Writes release notes for the team",
    "instructions": "Write for operators. Lead with what changed and what they must do.",
    "provider_id": "'"$runtime"'",
    "environment_provider_id": "'"$environment"'"
  }' | jq -r .agent_id)
```

| Field | Rules |
|---|---|
| `name` | One line, 1 to 100 characters. Unique among the tenant's active agents; a taken name is `409 agent-name-taken`. |
| `role` | What the agent is for, 1 to 1000 characters. |
| `instructions` | Optional standing instructions, up to 20000 characters. |
| `provider_id`, `environment_provider_id` | The default runtime and environment. Both must be registered for the tenant (`422 no-runtime-provider`, `no-environment-provider`). |

The response is the agent with `revision` 1 and `version` 1, and its `ETag` is the version.

## Read, revise and retire

Every principal of the tenant can read its agents. `GET /agents` lists them all, retired ones included;
`GET /agents/{agent}` reads one.

Only the principal that created an agent can change it. A revision replaces the whole profile and carries the version
you read in `If-Match`:

```bash
curl -fsS "${api[@]}" "${auth[@]}" -X PUT "$base/agents/$agent" \
  -H "Idempotency-Key: $(uuidgen)" -H 'If-Match: "1"' -H 'Content-Type: application/json' \
  -d '{ "name": "Release notes writer", "role": "Writes release notes for the team",
        "instructions": "Write for operators. Link every issue you mention.",
        "provider_id": "'"$runtime"'", "environment_provider_id": "'"$environment"'" }'
```

Each revision is kept: `GET /agents/{agent}/revisions/{revision}` reads the profile a task was delegated to.
`revision` counts profile changes and `version` counts every change, so retiring advances `version` alone.

`POST /agents/{agent}/retire`, with `If-Match`, stops new tasks from being delegated to the agent. Its revisions and
its tasks stay readable, and its name is free for a new agent.

## Delegate a task

Give `agent_id` in place of the providers. A provider you do give overrides the agent's default.

```bash
task=$(curl -fsS "${api[@]}" "${auth[@]}" -X POST "$base/tasks" \
  -H "Idempotency-Key: $(uuidgen)" -H 'Content-Type: application/json' \
  -d '{
    "agent_id": "'"$agent"'",
    "objective": "Write the release notes for v0.3.0",
    "acceptance_criteria": ["Every merged change is listed"],
    "demand": "person",
    "deadline": "'"$deadline"'",
    "unit": "tokens", "ceiling": 1000, "amount": 100
  }' | jq -r .task.task_id)
```

The task records the agent and the revision it had at submission, as `agent` on the task:
`{"agent_id": "...", "revision": 2}`. That revision is kept through rework and harness migration, so revising the agent
later never changes work already delegated. The task's audit entries carry the same agent and revision. A missing or
retired agent is `422 no-agent`.

## What the runtime receives

The objective and acceptance criteria stay the owner's request. The agent's role and instructions travel beside them
in `start_turn`, as an `agent` object, so the runtime can tell one from the other. The
[Codex adapter](https://github.com/dac-plane/dac-plane/tree/main/adapters/codex) places them in the thread's developer
instructions and keeps the brief as untrusted turn input.

Only a runtime adapter that negotiated the `agent.instructions` capability is sent a delegated task's turn. An adapter
built with an earlier kit never receives the field:

- A task delegated to an agent whose runtime has no adapter connection that negotiated it fails before its turn
  starts, with the dispatch reason `agent_instructions_unsupported`, rather than running without its instructions.
  The check is made when the turn is sent, not at submission, because adapters connect and upgrade independently
  ([ADR-0021](https://github.com/dac-plane/dac-plane/blob/main/docs/adr/0021-refuse-delegated-turns-at-dispatch.md)).
- A harness migration that restarts a delegated task waits until the target runtime has such a connection.

Tasks submitted without an agent are unaffected. The `start_turn` arguments, brief and instructions together, must fit
in 64 KiB once encoded; a larger start fails the task with the dispatch reason `invalid_dispatch_body`. A harness
migration's restart also carries its recovery context, so a task that only just fits may not move: its migration item
aborts before the restart is sent and the task's attempt fails, as for any aborted migration.

## During an upgrade

Agents need schema version 17. A binary rolled ahead of its migration answers agent writes and delegation with
`503 not-migrated` and reads of an agent with `404`, while tasks without an agent work as before. Run the migration,
then retry. See [Upgrade and roll back](../operate/index.md#10-upgrade-and-roll-back) for the order.
