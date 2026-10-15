# Deep dives 03 — control-port protocol and the Computer Use environment

Sources: `evidence/extracted-beautified/` (line citations below), helper binary strings,
`evidence/analysis/macho-*.json`. All static.

## 1. Control-port protocol (app ↔ coordinator)

`dist/node-agent-coordinator/main.cjs:35125-35230`. Versioned wire protocol,
`protocolVersion = 3`, one connection, fail-fast.

Frame kinds (validated, one of):

| Frame | Fields | Meaning |
|---|---|---|
| `lifecycle` | phase `hello`/`ready` + `protocolVersion`; or phase `shutdown` + reason `requested`/`protocol-error` (+detail naming the breach) | handshake and teardown; version mismatch = protocol-error shutdown |
| `request` | `requestId`, `method`, `args`, optional `demand` ∈ {`person`, `background`} | one RPC; demand classes let UI-critical calls preempt bookkeeping |
| `cancel` | `requestId` | in-flight cancellation |
| `reply` | `requestId`, outcome | outcome: `{status:"ok", value}` or `{status:"failed", failure{code, failureCode, failureMetadata, httpStatus, mcpConfigFailure, message, skillPublishRefusalKind, transportKind}}` |
| `event` | `family`, `payload` | unsolicited notifications |

Bootstrap carries `processConfig {appVersion, isPackaged, dataDir}`.

Rejection/error code taxonomy (same file, 35126-35132): `malformed-frame`,
`unknown-method`, `invalid-args`, `cancelled`, `gateway-malformed-reply`,
`gateway-command-failed`, `gateway-unreachable`, `gateway-transport-failed`.

Also present (separate connection, same file ~3233-3280): the **credential-ceremony
bridge** — frames `hello{computerId?, label?}`, `welcome{providerId}`,
`ceremony{get|create, origin, optionsJson}`, `cancel{requestId}`,
`stage{grant: ok/declined/failed | sign: ok/failed}`, `result{credentialJson}`,
`error{requestId, name, message, code?}`, `ping` — the coordinator fronts
WebAuthn ceremonies (origin-bound options) for consumers and gets credential JSON back.

## 2. Cloud → local machine enforcement stream (the receipt path)

`dist/node-agent-coordinator/main.cjs:21025-21427` — protobuf frames
`GrokBotUserComputerRequestFrame` (server-streamed to the machine) and
`GrokBotUserComputerResponseFrame` (machine → server).

Request frames (oneof): `exec` (server_message_json), `upload` (path, data),
`download` (path), `messages_op`, `local_tool` (tool_id, input_json,
conversation_id), `discover_ai_tool`, `retire_approval` (approval_id), `cancel`.
**Every action carries `approval_id?` plus `authorized_by_standing` /
`authorized_by_approval`** — authorization is always attributed to a source.

Response frames: `client`/`control` (message_json; control adds `cwd_state`),
`file` (data, seq, last) + `file_error`, `messages_result/error/accepted`,
`local_tool_accepted {permission_granted}`, `local_tool_permission_pending`,
`local_tool_result {status, output_json, error}`, `messages_consent_result
{verdict, reason}`.

Same protocol serves cloud environments too: `EnsureSandBoxResponse` (8923) carries
`execDaemonUrl` + `execDaemonAuthToken` (plus pod coordinates, `vncUrl`,
`terminalsFolder`, `gatewayUrl/token`, run state, `egress_tunnel_ownership`, image) —
the cloud pod runs the same exec-daemon image as the local machine.

## 3. Local authorization evaluator (the local half of enforcement)

`dist/electron-main/main-core.cjs:2150-2194, 11637-11700, 24880-24939`.

- **Governed actions**: `run-command`, `send-input`, `read-file`, `list-directory`,
  `write-file`, `read-ai-tool-setup`, `read-messages`, `send-imessage`,
  `run-phone-tool`, `use-app`.
- **Scope classes per action** (`ioe`): `global` (shell/input/file ops),
  `setting` (`read-messages` — allowed only when messagesEnabled),
  `subject` (`send-imessage`, `use-app` — per recipient), `call` (`run-phone-tool`).
- **Permission lattice**: `never(0) < ask(1) < always(2)` with min/max clamping;
  a **ceiling** is settable separately (`setLocalToolPermissionCeiling`), user can
  loosen below it, never above.
- **Durable stores** (state files at the data root): `local-tool-approvals.json`
  + a **retirements ledger** (`retirementsPath` — approvals are looked up by
  `approvalId` and retired after use, matching `retire_approval` in the stream);
  `local-tool-standing-grants.v1.json` with grants shaped `{scope: "action"|`"subject"`,
  accountScope, action, recipient?}`.
- **Cache invalidation by generation**: permission changes bump a
  `localToolPermissionGeneration`; grants/approvals are cleared on permission→`never`,
  messages-disable, and account-scope changes (24924-24937).
- **`serverAuthoritative` flag**: when set, the server's decision replaces local
  evaluation — the cloud can be the source of truth for this machine's actions.

## 4. Computer Use helper — the second environment provider

Binary: `Contents/Helpers/Grok Bot Computer Use.app/Contents/MacOS/CUGrokBotService`
(6.5 MB, universal). It is a **Swift MCP server** (`CUCore`: `CUMcpServer`,
`CUMcpToolset`, `CUMcpServiceTransport`, launched with `--mcp-stdio`,
"computer-use-mcp" naming) — the agent's toolset for controlling the user's Mac.

16 tools: `computer_apps`, `computer_resolve_app`, `computer_screenshot`,
`computer_app_state` (accessibility tree as text), `computer_app_action` (press
element by id), `computer_click/move/drag/scroll/key/type/set_value/wait`,
`computer_check_permissions`, `computer_start_control`, `computer_release_control`.

Authority model, two scopes:
- **app-scope**: accessibility-tree actions on one app — precise, preferred;
  `set_value` replaces text without keystrokes.
- **screen-scope**: fixed 1280-pixel coordinate space clicks/drags/keys —
  restricted; e.g. "screen-scope typing is refused"; needs Accessibility +
  Screen Recording permissions; actions needing a missing permission are refused
  with remediation instead of pre-checking.
- **Staleness tokens**: `coordinate_token` (valid until next screenshot or window
  move/resize), `snapshot_id` (valid until tree changes) — every action validates
  its handle, refusals say the token is stale and to re-observe.
- Discipline rules embedded in tool descriptions: no parallel calls (one machine,
  one action at a time), observe-after-act cadence.

Three variant families in main-core (15243-15264): prod / lab / dev
(`co.anysphere.grok-bot-computer-use` and -lab- / -dev-), per build track.

## 5. What this changes for dac-plane

1. **The receipt/denial leg is fully specified here.** Every governed action carries
   its authorization source (`standing` vs `approval` id) and approvals are *retired*
   through a dedicated ledger — dac-plane's Decision records should assume
   single-use approval consumption plus standing grants as durable policy, with the
   retirement event in the audit trail.
2. **Scope classes are part of the contract.** Their action taxonomy maps actions to
   `global/setting/subject/call` scopes, and grant records are scoped the same way.
   dac-plane's enforcement port should type actions with a scope class instead of
   treating "action" as one flat string.
3. **Permission lattice + ceiling** (never/ask/always, clamped, generation counter)
   is a minimal, complete model for local adapters — adopt the generation-counter
   invalidation trick for any cached policy.
4. **Same daemon, two placements.** Local machine and cloud pod run the same
   exec-daemon protocol — strong validation that dac-plane's environment port can
   have one contract with both a local dev adapter and a cloud production provider.
5. **Versioned control port (v3) with demand classes and fail-fast lifecycle** is a
   directly reusable adapter-contract shape: hello/ready/shutdown + request/cancel/
   reply/event + demand hint is ~5 frame kinds covering a whole RPC surface.
6. **Computer Use as MCP-over-stdio** shows their agent toolsets are MCP servers
   with embedded schema-bearing descriptions, staleness tokens, and refusal-with-
   remediation — a good shape for dac-plane's tool/secret gateway: tools self-describe
   their authority needs and refusals are actionable.
