# ADR-0024: Relay connector tools to Codex as dynamic tools, and let a starting attempt list them

- Status: Accepted
- Date: 2026-11-16
- Issue: [#137](https://github.com/dac-plane/dac-plane/issues/137)
- Refines: the Codex wiring in [ADR-0022](0022-govern-connectors-at-a-control-plane-gateway.md)

## Context

ADR-0022 has the Codex adapter add the connector gateway to each thread as an MCP server in `config.mcp_servers`, with
the attempt token as its `Authorization` header. The first live run of a Codex model against the gateway
(`just qualify-codex-connector`, Codex app-server 0.165.0 and Camer's `deepseek-v4-flash-0731`) found two faults that
deterministic tests could not show.

**The gateway refused the thread's own startup.** Codex initializes a thread's MCP servers and lists their tools when
the thread starts. The attempt is still `starting` then: it becomes `executing` only when the runtime reports it
started, after `thread/start` returns. The gateway accepted a token only for an executing or waiting attempt, so it
answered the startup with 403, Codex marked the server failed and the thread had no connector tools for its life.

**The model never saw MCP tools.** Codex 0.165.0 sends an MCP server's tools to the model inside a single Responses API
tool of type `namespace`. Camer drops tools of that type without an error: a request offering the same function once
as a plain function and once inside a namespace shows the model only the plain one. The same is reported for other
OpenAI-compatible backends, and Codex 0.165.0 has no setting to send MCP tools as plain functions. The model therefore
saw only Codex's MCP resource helpers and reported that no `get_issue` tool existed.

Codex 0.165.0 also accepts `dynamicTools` in `thread/start`: tools the app-server client declares, which reach the
model as plain functions, and whose calls Codex sends back to the client as an `item/tool/call` request. Observed on
0.165.0 with a recording model endpoint, a declared tool reaches the model as `{"type": "function", ...}`.

## Decision

**A starting attempt may open its gateway session and list its tools.** The gateway authenticates a token for an
attempt that is `starting`, `executing` or `waiting_input`. A tool call still needs an executing or waiting attempt,
which the call checks in its own transaction, so nothing can be called before the runtime has started.

**The Codex adapter is the gateway's MCP client and offers its tools as dynamic tools.** When a `start_turn` carries the
gateway, the adapter lists the attempt's tools from the gateway before `thread/start` and declares each as a dynamic
tool. A tool's function name is `dac_<tool>_<8 hex digits>`, the hex being a digest of the gateway's name for it, so
two connections offering one tool stay distinct and every name fits the Responses API's 64 characters. On
`item/tool/call` the adapter lists the tools again to resolve the name, sends `tools/call` to the gateway with the
attempt token, and answers Codex with the tool's text and whether it succeeded. A refusal, an approval that does not
arrive within the gateway's wait, or an unreachable gateway is answered as a failed call, so Codex never waits on
silence. The gateway still decides every call; the adapter only relays it.

**The gateway is no longer an MCP server of the thread.** The adapter does not set `config.mcp_servers`, so no
provider sees the tools twice. The token stays out of prompts, the workspace and `thread/resume`; the journal keeps it,
as before, to relay calls after a restart.

**A gateway that cannot be listed leaves the thread without connector tools.** The start goes ahead and the adapter
logs a warning. Omitting tools withholds access and grants none.

## Alternatives considered

- **Keep the MCP server and wait for Codex to flatten namespaces:** rejected. Connector access would depend on each
  model provider accepting a tool type outside the published Responses API, and fail without an error where it does
  not.
- **Pin a Codex build with a `namespace_tools = false` provider setting:** rejected for now. The setting is reported in
  unofficial builds only, and changing the pinned app-server needs its own qualification.
- **List the tools on every turn:** not needed. A thread is one attempt, and its grants are decided by the gateway at
  call time, so a grant revoked after the thread started is refused at the call and a tool granted later is listed by
  the next attempt.
- **Accept only an executing attempt and have the adapter connect later:** rejected. The adapter would have to delay
  `thread/start` past the start report the runtime makes after it, and other runtimes connect at startup as Codex does.

## Consequences

- `dynamicTools` is an experimental field of the pinned app-server. A Codex upgrade must keep it, or this decision is
  revisited; the live qualification fails if the tools stop reaching the model.
- A tool granted after a thread started reaches that thread only after a new attempt. A revoked grant is refused at the
  next call, as before.
- Connector calls reach the model through the adapter, so a runtime adapter other than Codex chooses its own way to
  offer the gateway's tools; the gateway contract does not change.
