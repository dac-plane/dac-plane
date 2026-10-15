# Deep dives 01 — RPC surface, createAgent per harness, local-exec hooks

Companion to `notes/DAC-PLANE-MAPPING.md`. Raw digests live in `evidence/analysis/`;
full ASAR extraction in `evidence/extracted/`. All static, nothing executed.

## 1. Backend RPC surface (authority side)

Source: `extracted/dist/electron-main/proto.cjs` → digest `analysis/services.txt`.

The backend exposes **7 services in namespace `aiserver.v1`** (the Cursor protobuf
namespace — the backend IS Cursor's infrastructure), 485 methods total:

| Service | Methods | Role |
|---|---|---|
| `GrokBotService` | 308 | The control plane: agents, sessions, transcripts, approvals, spend, harness migration, egress, admin |
| `SandBoxService` | 55 | Environment provider: pods, store, egress, upgrades (fully embedded in GrokBotService) |
| `DashboardService` | 74 | Account/team/billing/MCP config/machines/canvases |
| `AgentStoreService` | 25 | Shared agent workspace: documents, locks, tombstones, sharing |
| `AiService` | 8 | Models, image gen, STT/TTS, process metrics |
| `AnalyticsService` | 8 | Events, logs, conversation ingestion, issue traces |
| `BackgroundComposerService` | 7 | Workers, diffs, artifacts, PR merge status |

Almost everything is **Unary**; streaming is used only for migrations
(`WatchSandBoxMigration`, `AdminWatchSandBoxMigration`) and transcripts
(`WatchGrokBotTranscripts`, `WatchGrokBotUserComputerRequests`,
`OpenGrokBotUserComputerRequest`), plus one `BiDiStreaming` (`StreamStt`).

Observations relevant to dac-plane's Surface API:

- **Artifact store API is explicit and fenced**: `presign...Writes/Reads`,
  `complete/abortSandBoxStoreMultipartWrites`, `commitSandBoxStoreManifest`,
  `stat/listSandBoxStoreObjects`, plus admin snapshot/restore with manifest versions.
  Upload is presigned (bytes bypass the API); manifest commit is the transactional
  admission point (`COMMITTED/RETRY/FENCED`).
- **Locks are a service**: AgentStoreService has `acquire/renew/release/getAgentStoreFileLock`
  — leases over shared workspace files, server-side.
- **Governed actions have named endpoints**: `resolveGrokBotVirtualCardApproval`,
  `beginGrokBotSpendApproval` + `finishGrokBotSpendApprovalAssertion` (two-phase
  ceremony: begin then assert), `approve/denyOnePasswordCredentialRequest`,
  `resolveGrokBotConnectorGrant` with `clearGrokBotConnectorAlwaysAllow` (policy
  revocation is a separate call).
- **Hard limits are server state**: `getHardLimit/setHardLimit`,
  `getDailySpendByCategory`, prepaid auto top-up, `getSandUsageStatus` — budgets live
  with the authority, not in the client.
- **Admin parity**: most lifecycle operations have an `admin*` twin (break-glass pod
  deletion, email throttles, migration holds) — operations tooling is part of the
  surface, not an afterthought.
- **Machine registration**: `registerSandMachine/listSandMachines/removeSandMachine`
  in DashboardService — the user's local machine is a registered, removable
  environment resource.
- **Egress policy is served**: `getGrokBotEgressPolicy` (Unary) — the enforcement
  gateway pulls its policy from the authority; `adminRefreshSandBoxEgress` pushes
  refreshes with outcome states (PUSHED/NO_BOX/ERROR, REFRESHED/SKIPPED/UNSUPPORTED_HOST/FAILED).

## 2. createAgent and the harness split

Sources: `extracted/dist/node-agent-coordinator/main.cjs`.

- `createAgentWithRetry` wraps `request("createAgent", ...)` with `clientNonce` (UUID),
  `supportsTemporalHarness:!0` (always true), a bounded retry loop (`maxAttempts`), and
  optional `requiredBaseUrl` pinned after a first failure. Result outcomes include
  `ACCEPTED_TEMPORAL / DUPLICATE / REFUSED` (and an accepted-BOX value) — creation is
  idempotent by nonce and explicit about which harness accepted it.
- Creation intents: `GrokBotAgentCreateIntent: FRESH / REGISTER_EXISTING_LOCAL`;
  create callers: `ROOM_MEMBERS / ENSURE_SERVER_BACKED / MINT / BACKFILL...`.
  An existing *local* agent can register with the backend — the local machine can be the
  environment for an agent whose records are server-side.
- Harness tracking: the coordinator keeps `harnessOf(agentId)` from two sources —
  server roster changes and live agent states. On seeing a live agent with unknown or
  `box` harness it registers it as `temporal` — this is the live tracking of the
  BOX→TEMPORAL migration (`GrokBotAgentHarnessKind: BOX/TEMPORAL`,
  `GrokBotTemporalHarnessMode: OFF/SHADOW/LIVE/BOX`,
  `GrokBotBoxHarnessMigrationPassState: DISABLED/DONE/PENDING`).
- Transcript sources are tagged `server` vs `gateway` — the same progress stream can be
  rehydrated from the server or pulled from the gateway connection, and the client
  reports which source it is reading (`reportTranscriptSource`).
- Internal migration tooling exists in the RPC surface itself:
  `ensureGrokBotBoxHarnessMigrationPass`, `getGrokBotHarnessMigrationStatusInternal`,
  `clearGrokBotHarnessMigrationHoldInternal`, `previewInternalPrimaryBotMigration`,
  `executeInternalPrimaryBotMigration` — preview/execute split, migration holds.

## 3. Local machine as environment (local-exec-daemon)

Sources: `extracted/dist/local-exec-daemon/main.cjs`, excerpts in
`analysis/local-exec-executors.txt`; wiring in `main-core.cjs`.

- Separate long-running daemon (not an Electron child), reached over **HTTP + SSE on
  localhost with a token** (`baseUrl`, `token`, `headers`; topics
  `local-exec-sse-connect`, `local-exec-control-post`, `local-exec-data-post`,
  `local-exec-response-post`, `local-exec-heartbeat`).
- **Local data root** `.grokbot-data-root-v1` with `host.lock`, `agents/`,
  `gateway.json`, `host-secrets.json`, `settings.json`; app-side state files:
  `local-exec-daemon-connection.json`, `local-exec-daemon-credential.json`,
  `local-exec-update-lease.json`, `local-exec-account-retire/generation.json` — a local
  control plane with lock/lease/generation files, mirroring the server's lease semantics.
- **41 tool executors** implement the tool surface: shell (shell, shell-stream,
  background-shell, force-background-shell), file tools (read/write/edit/delete/ls/grep/
  find/git-diff/redacted-read), MCP (mcp, mcp-state, list-mcp-resources,
  read-mcp-resource), subagents (subagent, subagent-await, force-background-subagent),
  computer-use and record-screen, conversation-search, diagnostics, agent-store conflict
  handling, plus alternate harness executors (`mini-swe-agent-bash`, `pi-*` — a second,
  compact agent harness family).
- **Enforcement is a pre-execution hook protocol**, executed synchronously inside the
  environment via `hook-executor` (`executeHookArgs`/`executeHookResult`):
  `beforeShellExecution`, `beforeMCPExecution`, `afterShellExecution`,
  `afterMCPExecution`, `beforeReadFile`, `afterFileEdit`, `beforeTabFileRead`,
  `afterTabFileEdit`, `stop`, `beforeSubmitPrompt`, `afterAgentResponse`,
  `afterAgentThought`, `sessionStart`, `sessionEnd`, `preCompact`, `subagentStart`,
  `subagentStop`, `preToolUse` (+ truncated tail) — hooks cover the full agent action
  lifecycle, before *and* after.
- Dedicated **precheck executors** run before tools: `shell-allowlist-precheck`,
  `mcp-allowlist-precheck`, `web-fetch-allowlist-precheck`.
- Sandbox policy model: `sandboxpolicy` (+type), `sandboxworkspaceroot`,
  `sandboxpreflight`, `sandbox-policies`, egress tunnel
  (`egresstunnelenabled`, `egresstunnelpreference`), transparent proxy with
  `proxyfingerprint`.
- Client settings that gate local actions: `toolPermissions`, `localToolPermission`,
  `localToolPermissionCeiling`, `localEgressAllowed`, `autoReviewInstructions`,
  `computerUseModel` (model + `maxMode` + parameters) — the ceiling is the durable
  guard: the user can loosen but not exceed it.

## 4. Corrected topology statement

The earlier `notes/ARCHITECTURE.md` implied agents might be coordinated by the desktop
app. The evidence now supports this topology:

```
Cloud backend (aiserver.v1, cursor infra)   <- authoritative records, harness runtime (BOX pods / Temporal)
        |  ConnectRPC (Unary + streaming)
Electron app (surface + session/turn UI)     <- approvals UI, spend ceremonies, transcript rehydration
        |  localhost HTTP+SSE + token         |  computer-use helper (separate app)
local-exec-daemon (local environment)        <- 41 executors, hook protocol, sandbox/egress policy
        |
user's local machine (workspace, PTYs)
```

- The desktop app is the user surface for a lifecycle whose authority is server-side.
- The user's machine appears as a **registered environment** whose process/isolation
  lifecycle the daemon owns; governed actions inside it pass the hook protocol.
- `REGISTER_EXISTING_LOCAL` creation shows local and cloud environments are peers under
  the same agent record model.
