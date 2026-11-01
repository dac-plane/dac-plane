# ADR-0016: Run an agent's commands inside its environment

- Status: Accepted
- Date: 2026-11-01
- Issues: [#108](https://github.com/dac-plane/dac-plane/issues/108)

## Context

[#101](../architecture/task-cancellation-101.md) proved that the runtime and environment stops converge, but
Codex ran its commands in its adapter container. The sandbox sat idle, so stopping it ended no work, and no
containment claim followed.

The sandbox denies all outbound traffic, and the model credential must stay out of the workload's reach. Those
two facts rule out moving the whole runtime into the sandbox: the runtime would need both model egress and the
credential beside the workload.

Static analysis of a shipped agent product found the same split. Its sandbox response carries an
`execDaemonUrl` and an `execDaemonAuthToken`, and its cloud pod runs an exec daemon
(`architecture-explore/plane-bot-explore/notes/DEEP-DIVES-03.md`). That is evidence the shape is workable, not
evidence that it works here. The observations below are.

## Decision

**The runtime stays with its adapter and only commands run in the environment.** `codex-app-server` and the
model credential remain in `dac-codex-adapter`. `codex exec-server` runs in the sandbox, and the app-server
reaches it inbound through the provider's public port host. The sandbox keeps `allow_internet_access: false`
and `denyOut`, and gains no outbound rule.

**A shared key derives each sandbox's token.** Both adapters hold `DAC_ENVIRONMENT_ATTACH_KEY_FILE`. The token
is HMAC-SHA256 of the resource reference, and the sandbox receives only its SHA-256 hash. No secret crosses the
control plane or reaches PostgreSQL, the runtime adapter never holds the E2B credential, and nothing needs
storing to attach again after a restart.

**The environment adapter starts the command server.** A template start command is captured at build time, so
it cannot hold a per-sandbox hash. The E2B adapter starts exec-server after create and reports the provision
only once it is listening.

**The wire contract does not change.** `resource_ref` stays opaque to the product. The endpoint shape is
deployment configuration on the runtime adapter, as model and provider already are, so no vendor name reaches a
schema. With that endpoint configured, a start without an environment is refused, and local execution is off.

**A lost environment is not a finished turn.** When the link is lost and does not return, the adapter interrupts
the turn. A turn that ended after the link was lost is never reported as completed.

**Codex keeps its own sandbox inside the environment.** Commands run under `workspace-write` with `on-request`
approvals, so the template carries bubblewrap. Full access would skip the approvals that #91 qualified.

## Observations

Observed on 2026-11-01 with Codex 0.165.0, E2B envd 0.10.0 and a template built from
[`template.mjs`](../../adapters/e2b/template/template.mjs), with `allow_internet_access: false` and
`denyOut: ["0.0.0.0/0"]` active.

| Question | Observed |
|---|---|
| Does inbound reach exec-server under the deny? | Yes. WebSocket upgrade returned 101, while `curl https://1.1.1.1` from inside failed with exit 35. |
| Is the token enforced? | Yes. No token and a wrong token each returned 401. |
| Do commands run in the sandbox? | Yes. Output named `e2b.local`, and the written file existed only there. |
| Is the model key visible to a command? | No. Its length read 0 under the default, `none` and `core` shell environment policies. `core` is used, because bubblewrap was not found under `none`. That run also had `CODEX_HOME` in `/tmp`, so the two causes were not separated. |
| Does `workspace-write` work under Firecracker? | Yes, with bubblewrap installed and `CODEX_HOME` outside `/tmp`. A write outside the workspace failed with `Read-only file system`, and the approval request reached the client. |
| Does exec-server outlive the envd stream that started it? | Yes. It still answered 101 after the stream closed. |
| Can a second client read results? | Yes. A second connection listed and read the workspace with `fs/readDirectory` and `fs/readFile`. |
| What happens when the sandbox is deleted mid-command? | The command died with the sandbox. `thread/environment/disconnected` arrived 26 seconds later. The model then retried for 39 seconds and the turn ended `completed`. |
| What happens when the app-server dies mid-command? | The command was still alive 21 seconds later and gone by 40. `thread/resume` reported the turn `interrupted` while it may still have been alive. |

## Consequences

- Stopping the environment ends the work, because the work is in it.
- The token is the only access control on the port. `environment/add` cannot send E2B's traffic access header,
  so `allowPublicTraffic` stays on. E2B terminates TLS and can see the token, but E2B already hosts the workload.
- envd is reachable from outside, so sandboxes are now created with `secure: true`, which the idle sandboxes
  never needed.
- A runtime restart can report a turn `interrupted` while its command runs on for up to about 40 seconds. That
  report alone does not prove the command ended, which is another reason cancellation requires both stops.
- `environment/add`, `thread/start.environments` and exec-server are experimental Codex APIs, so the version pin
  stays exact and an upgrade repeats these observations.
- Provision now depends on envd and the port host, and takes longer. A failed start deletes the sandbox.
- Both adapters must share a key, which is one more secret to rotate. Rotating it breaks attachment to running
  sandboxes.
