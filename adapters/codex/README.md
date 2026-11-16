# Codex runtime adapter

`dac-codex-adapter` is the out-of-process runtime adapter for the pinned Codex app-server candidate.
It imports the published `adapterkit` module and does not import the control plane's `internal`
packages. Provider and model selection remain deployment configuration; neither appears in DAC task
or ControlFrame contracts.

The image pins Codex app-server `0.165.0` and verifies the official archive checksum during the
build. The adapter starts app-server over stdio and does not expose its native listener. It stores
operation mappings, ControlFrame sequence identities, approval decisions and artifact metadata in a
mode-`0600` bbolt database under the configured Codex home.

## Configuration

Every setting the executable reads is in the [configuration reference](../../docs/guides/reference/configuration.md#dac-codex-adapter), with the
[settings every adapter shares](../../docs/guides/reference/configuration.md#every-adapter).

Commands run beside the adapter unless `DAC_CODEX_EXEC_SERVER_URL` and `DAC_ENVIRONMENT_ATTACH_KEY_FILE` are both set,
and then only in the environment assigned to the Attempt
([ADR-0016](../../docs/adr/0016-run-agent-commands-inside-the-environment.md)); `DAC_CODEX_WORKSPACE` is then a path
inside the environment. Credentials are read from files, passed only in the child process environment
and redacted from app-server stderr.

The first qualification configuration is `camer`, `https://api.ai.camer.digital/v1`, Responses wire
protocol, and `deepseek-v4-flash-0731`. Those values are not defaults in a neutral DAC contract.

## Agent instructions

The adapter offers `agent.instructions`. A task delegated to an agent then arrives with the agent's role and standing
instructions beside the brief; the adapter passes them to `thread/start` as `developerInstructions`, and the brief stays
untrusted turn input ([ADR-0020](../../docs/adr/0020-agents-are-product-records.md)).

## Connector tools

The adapter offers `connector.gateway`. A `start_turn` that carries the gateway gives the adapter the attempt's MCP
endpoint and bearer token. Before `thread/start` the adapter lists the attempt's tools from the gateway and declares
each as a dynamic tool named `dac_<tool>_<digest>`, which the model sees as a plain function. Codex sends each call
back as `item/tool/call`; the adapter relays it to the gateway's `tools/call` with the token and answers with the
tool's text. The gateway authorizes every call. The token never enters a prompt, the workspace or `thread/resume`; the
journal keeps it to relay calls after a restart.

The gateway is not given to Codex as an MCP server: Codex 0.165.0 sends MCP tools to the model inside a Responses API
`namespace` tool, which Camer and other OpenAI-compatible providers drop without an error
([ADR-0024](../../docs/adr/0024-relay-connector-tools-to-codex-as-dynamic-tools.md)). `just qualify-codex-connector`
checks the whole path against a live model.

## Replay boundary

This adapter deliberately does not advertise `operation.replay`. It writes a `prepared` operation
before `thread/start`, but Codex app-server `0.165.0` supplies no caller correlation that can recover
a successful `thread/start` whose reply was lost before the returned thread identifier was journaled.
Redelivery therefore returns an unknown-outcome error instead of creating another native thread.
Mapped threads and turns are resumed from persistent Codex state and the adapter journal.

Approval delivery is narrower: the decision is journaled before its native response, and a prepared
decision is retried until it is durably marked delivered. Only `approve_once` and `deny` are mapped;
the adapter never promotes a native approval to standing authority.

## Verification

`just test-codex-adapter` runs the fake-app-server crash, protocol, redaction, artifact and published
ControlFrame conformance tests with and without the race detector. These deterministic tests do not
qualify Camer availability or a production artifact store. The secret-backed deployed gate remains
`just qualify-codex-runtime`; it requires `DAC_CODEX_API_KEY_FILE`, creates a disposable Compose
project, and removes that project's volumes after retaining logs under `.work/codex-qualification/`.
Its observed status and limits are recorded in the issue evidence note. `just qualify-codex-containment` is the
same kind of gate for commands running in a sandbox, recorded in
[its evidence note](../../docs/architecture/codex-containment-108.md).
