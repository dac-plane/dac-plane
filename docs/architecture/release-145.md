# Release evidence: v0.3.0

Evidence for [#145](https://github.com/dac-plane/dac-plane/issues/145). It records what was observed, including failures.

## Live runs before the first tag

Host: one arm64 machine, 2026-11-16. Images: local builds of the release preparation branch. Model: Camer's
`deepseek-v4-flash-0731` through Codex app-server 0.165.0.

| Scope | Result |
|---|---|
| `qualify-codex-connector`, first run | Failed. Both scenarios: the model never saw the connector tool. |
| `qualify-codex-connector`, after the starting-attempt fix | Failed. Both scenarios, for the second cause below. |
| `qualify-codex-connector`, after relaying tools as dynamic tools | Passed twice. An `always` grant ran without approval and an `ask` grant waited for one; each artifact held exactly the title the simulated connector returned for the issue number the model asked for. No credential appeared in the stack logs. |
| `qualify-codex-connector`, after review fixes | Passed. Each artifact also held a marker that the simulated connector derives from the connection's credential and that appears in no prompt, so the artifact proves the call ran. Calls made before the attempt executes now wait for it instead of being refused, and the adapter's gateway requests keep the no-redirect policy. |
| `qualify-codex-runtime` | Passed: approval and restart with an exact artifact, a delegated agent's instructions reaching the model, and denial with no effect. |

The two failures were in the path from the gateway to the model, which no deterministic test covered:

- **The gateway refused the thread's startup.** Codex lists a thread's tools when the thread starts, while the attempt is
  still `starting`. The gateway accepted only an executing or waiting attempt and answered 403, so the thread had no
  connector tools. A starting attempt may now list its tools; a call still needs an executing attempt.
  `TestConnectorGatewayServesAStartingAttempt` fails without the fix.
- **The model provider dropped Codex's MCP tools.** Codex 0.165.0 sends MCP tools inside a Responses API `namespace`
  tool, which Camer drops without an error. The Codex adapter now offers the gateway's tools as dynamic tools, which
  reach the model as plain functions, and relays their calls to the gateway
  ([ADR-0024](../adr/0024-relay-connector-tools-to-codex-as-dynamic-tools.md)).

Not run: the GitHub connector against a real GitHub App, which needs a dedicated consented fixture, and a check of the
runtime host's egress, which the reference stack does not confine. Both are known limits in the notes.

## Upgrade from v0.2.0

`just test-operations-upgrade` pulls the v0.2.0 image by digest, rolls both replicas with a task in flight, migrates to
the release schema and delegates a task to an agent afterwards. It runs on every pull request. It uses simulators, not
live providers.

## Rehearsal

To be recorded.

## Release

To be recorded.
