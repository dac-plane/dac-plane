# Concepts

dac-plane sits between the places people ask for work and the agents that do it. People and their tools submit work
through the Surface API. Agent runtimes, the environments they run in, and the stores that keep their output connect
through adapters. dac-plane keeps the record of what was asked, what happened, who decided what, and what came back.

## The record

dac-plane owns these records. No provider writes them; a provider reports what it observed, and dac-plane decides
whether that changes the record.

| Record | What it is |
|---|---|
| Task | One piece of delegated work: its objective, acceptance criteria, deadline and budget. |
| Attempt | One execution of a task, through one runtime in one environment. A task can have several, for example after a retry or rework. |
| Attention request | A question or approval an attempt is waiting on, such as permission to run a command or write a file. |
| Decision | The answer to one attention request: approve once, approve standing, deny, or an answer. |
| Budget | The spending limit of a task in one unit, such as tokens, with what has been reserved and spent. |
| Artifact | A file the work produced, committed to the artifact store with its digest and where it came from. |
| Result revision | One proposed result of a task: its artifacts and verifications, never changed once proposed. |
| Result review | A judgement of one revision: accept, request changes, or reject. |
| Audit event | An append-only entry for each change: who asked, under what authority, and what changed. |

## How a task moves

![A task is queued, then running. It waits for input while an attention request is open, and moves to verifying when the agent proposes a result. If the checks pass it is ready for review; if they fail it goes back to running. A reviewer accepts it, which ends it as succeeded, rejects it, or requests changes, which queues it for a new attempt. Work in progress can fail, or be cancelled, which waits for the runtime and environment to confirm the stop. A queued task, or one waiting for review, is cancelled at once.](assets/task-lifecycle.png)

1. A surface submits a task, naming the runtime and the environment provider and setting a budget.
2. dac-plane creates an attempt and dispatches it: the environment adapter provisions a workspace, and the runtime
   adapter starts the agent in it.
3. When the agent needs permission or input, the attempt waits and an attention request is opened. A person, or a
   service acting for one, decides it through the Surface API, and the agent continues.
4. The agent's output is committed to the artifact store and proposed as a result revision. The task is
   `ready_for_review`.
5. A reviewer accepts the revision, rejects it, or requests changes, which queues the task for a new attempt.

A task can be cancelled until it ends. While work is running, dac-plane asks the runtime and the environment to stop,
and records the task cancelled once they confirm.

## Providers and adapters

A provider is anything that does part of the work. Each connects through its own adapter process, which opens a TLS
WebSocket to dac-plane; dac-plane never connects out to an adapter. There are four roles:

| Role | Does | Example |
|---|---|---|
| `runtime` | Runs the agent: its reasoning, sessions and turns. | Codex |
| `environment` | Provides the workspace the agent's commands run in, and stops it. | E2B sandboxes |
| `artifact_store` | Holds the bytes of committed artifacts. | An S3-compatible store such as SeaweedFS |
| `enforcement` | Enforces policy on governed actions on the execution path. | A policy or tool gateway |

Any of them can be replaced without changing the record. To connect a new one, [write an adapter](extend/adapters.md).

## Identity

dac-plane does not manage users. It accepts access tokens from OpenID Connect issuers you configure, and maps each
token's issuer and subject to a record it does keep:

- A **principal** is a person or a service in a tenant. A Surface API caller's identity is bound to a principal, and
  can read and act on the tasks it submitted; the tenant-reader permission widens that to the whole tenant.
- An **adapter binding** ties an adapter's client identity to one provider registration, so an adapter can only act
  as the provider it was bound to.

Surface API tokens and adapter tokens carry different audiences, so neither can be used in place of the other. The
[identity configuration file](reference/configuration.md#identity-configuration-file) lists the trusted issuers, and
[provisioning](reference/cli.md#provision) creates tenants, principals and bindings.

## Further reading

- [Architecture](https://github.com/dac-plane/dac-plane/blob/main/docs/architecture/README.md): the views, ownership
  rules and provider contract semantics.
- [Decision records](https://github.com/dac-plane/dac-plane/tree/main/docs/adr): why it is built this way.
