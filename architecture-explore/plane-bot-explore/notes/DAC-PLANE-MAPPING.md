# Grok Bot ↔ dac-plane concept mapping

Purpose: record how a shipped system (Anysphere/SpaceXAI's "Grok Bot" 0.71.2, internally
`sand`) solves the same problems dac-plane is designing for, as input for the baseline
discussion in `docs/architecture/` and the open questions in `review-2026-10-15.md`.

All evidence is static, from `evidence/rea-js-analysis.json` and `evidence/extracted/`
(bundle paths given as `extracted/...`). Nothing was executed.

## 1. Concept-by-concept mapping

| dac-plane concept | Grok Bot implementation | Evidence |
|---|---|---|
| **Task** | `GrokBotAgent` (the delegated worker) with per-agent `GrokBotAgentTask`/todos; task state `WORKING / NEEDS_INPUT / DONE` | `extracted/dist/node-agent-coordinator/main.cjs` — proto enums `GrokBotTaskState`, `GrokBotAgentKind` (AGENT/ROOM); methods `getAgentTasks`, `markAgentTaskDone`, `sendTask` |
| **Attempt** | Agent runs carry an `attemptSequence`; retry is a first-class method (`createAgentRetry`, `createAgentWithRetry`, `sendPromptAttempt`, `waitAttempt`); prompts are attempts of a session | coordinator strings: `attemptSequence`, `createAgentRetry`, `waitAttempt`, `sendPromptAttempt` |
| **Runtime-native session/turn correlated to Attempt** | Explicit `session` + `turn` split: `TurnRequest/TurnResponse/TurnResult/TurnMessage/TurnFailed`, `turnProbeDeadline`, `turnWatch`, `turnId`; sessions have kinds (MAIN, DM, GROUP, SLACK_DM, SLACK_THREAD, TEAMS_*) | proto enum `GrokBotAgentSessionKind`; turn symbols in coordinator |
| **AttentionRequest** | Task state `NEEDS_INPUT`; widget/user-form requests surface as interruptible prompts (`respondToWidget`, `dismissWidget`, `submitUserForm`, `promptAcceptanceStatus`) | `GrokBotTaskState`; widget/form methods in coordinator method map |
| **Decision / approval** | Durable approval requests with resolutions: tool permission card `ALLOW_ONCE / DENY / ALWAYS / NEVER`, auto-review `APPROVED / DENIED / ALWAYS`, connector grants, credential requests, cookie-origin approvals | proto enums `GrokBotLocalToolPermissionCardResolution`, `GrokBotAutoReviewApprovalResolution`; methods `resolveLocalToolPermission`, `resolveAutoReviewApproval`, `resolveCredentialRequest`, `resolveConnectorGrant`, `waitCookieOriginApproval` |
| **Budget** | Spend is enforced through virtual-card approvals: `beginSpendApproval`, `resolveVirtualCardApproval`, `setSpendLimit`, `SpendApprovalCeremonyMode: ASSERT / REGISTER` (WebAuthn-style ceremony), plus `xai-commerce-prepaid` and payment-method surfaces | coordinator: `beginSpendApproval`, `setSpendLimit`, `SpendApprovalCeremonyMode`; package `@anysphere/xai-commerce-prepaid` in `extracted/package.json` |
| **Enforcement path** | Layered: `GrokBotToolPermission` (ALWAYS_ALLOW/NEEDS_APPROVAL/BLOCKED) evaluated per tool; MCP tool permissions (`get/setGrokBotMcpToolPermissions`); egress policy (`getGrokBotEgressPolicy`, `egresstunnelenabled`, transparent proxy with fingerprinting); credential brokering via 1Password connector and vault | local-exec-daemon: `sandboxpolicy`, `sandboxworkspaceroot`, `proxyfingerprint`; coordinator: egress/vault/1Password methods |
| **Environment provider (workspace, process, isolation, cleanup)** | `SandBox` lifecycle: run state `ABSENT / HIBERNATED / RUNNING / STARTING`; migration phases `BACKING_UP / CREATING / MOVING / CLEANING_UP / WIPING / DONE / FAILED`; upgrade scheduling with claim semantics `SCHEDULED / CLAIMED / RUNNING / WAITING_FOR_IMAGE / COMPLETED / MISSED / FAILED / CANCELLED`; bulk operations `QUEUED / RUNNING / SUCCEEDED / SKIPPED / FAILED` and `PARTIALLY_SUCCEEDED` | proto enums `SandBoxRunState`, `SandBoxMigrationPhase`, `SandBoxUpgradeScheduleState`, `BulkTeamMemberSandBox*` |
| **Artifact storage** | Sandbox store with multipart writes, commit status `COMMITTED / RETRY / FENCED` and failure codes (CHECKSUM_MISMATCH, UPLOAD_NOT_FOUND, RESTART_REQUIRED); transcript entries are the durable progress record | proto enums `SandBoxStoreMultipartOperationFailureCode`, `SandBoxStoreManifestCommitStatus`; methods `completeSandBoxStoreMultipartWrites`, `listSandBoxStoreObjects` |
| **Event ingestion (observations until accepted)** | Transcript streaming with explicit sequence and reconciliation: `{agentId, sessionId, generation, streamAckUpdatedSeq, rows, windowFloorSeq, maxLiveSeq, needsRehydrate, epochBump}`; client ACKs updated seq, clears rows and bumps `epochBump` on resets; send durability is explicit: `GrokBotSendStatus: ACCEPTED / REJECTED / PENDING / NOT_FOUND / UNKNOWN_DURABILITY` | coordinator transcript stream code; proto enum `GrokBotSendStatus` |
| **Replaceable runtime (provider open)** | Harness abstraction: `GrokBotAgentHarnessKind: BOX / TEMPORAL`, with `GrokBotTemporalHarnessMode: OFF / SHADOW / LIVE` and migration tracking `GrokBotBoxHarnessMigrationPassState: DISABLED / DONE / PENDING` — they are running a BOX→TEMPORAL migration pass-by-pass, with a shadow mode for parity testing | proto enums in coordinator; `supportsTemporalHarness:!0` in `createAgent` args |
| **Leases** | `LeasePath` message type and `updateLeasePath`; upgrade schedules claim work via `CLAIMED` state | coordinator proto field `LeasePath` |
| **Multi-surface commands (API clients first)** | Same command surface reachable from the app, Slack, Teams, email inboxes, schedules (automations) — one method registry with per-method demand classes (`person` vs `background`) | `GrokBotAgentSessionKind`; automations methods; Slack/Teams install state enums |

## 2. Patterns worth studying for dac-plane

1. **Harness kind + shadow mode + migration passes.** The runtime is behind a small enum
   (`BOX`/`TEMPORAL`) plus a mode enum (`OFF`/`SHADOW`/`LIVE`) and a per-agent migration
   state. This is a concrete pattern for dac-plane's "qualified runtime behind a port": a
   second runtime can run in shadow for parity evidence before serving traffic, and
   migration progress is a durable record, not a deployment flag.
2. **Approval resolutions that persist policy, not just a decision.** `ALLOW_ONCE/ALWAYS/
   NEVER` means an approval doubles as a durable policy record for future identical
   requests. dac-plane's Decision records could adopt the same shape (resolution + scope +
   expiry).
3. **Spend approval with a cryptographic ceremony.** `SpendApprovalCeremonyMode: ASSERT/
   REGISTER` over virtual cards ties budget authority to an authenticated assertion
   ceremony — an enforcement-gateway analog where the receipt is the signed ceremony
   result.
4. **Transcript as the event log with explicit reconciliation.** Sequence numbers, ACKs,
   generations, `needsRehydrate`, `epochBump`, and a `UNKNOWN_DURABILITY` send status make
   the "events are observations until accepted" rule concrete: the producer exposes
   dedupable sequences and the consumer rehydrates explicitly instead of assuming order.
5. **Upgrade scheduling with claim semantics.** `SCHEDULED/CLAIMED/...` for sandbox
   upgrades is a lease pattern for environment maintenance, matching dac-plane's
   PostgreSQL-lease durability choice.
6. **Versioned control-port handshake.** The app↔coordinator protocol has an explicit
   `lifecycle` frame with `hello/ready/settled` phases and a `protocolVersion` number —
   small, versioned, and fail-fast, a good shape for dac-plane's runtime adapter contract.
7. **Fenced commits in the artifact store.** `COMMITTED/RETRY/FENCED` for store manifest
   commits gives exactly the "verify digest, fence stale writers" guarantee the baseline
   wants from artifact storage.

## 3. Divergences from the dac-plane baseline

- **Lifecycle authority location.** In Grok Bot, the durable product records live in the
  cloud backend (cursor.com infrastructure over ConnectRPC/protobuf); the desktop app and
  local coordinator are clients/brokers. dac-plane keeps lifecycle authority in the
  product core with providers never writing the product database. Grok Bot satisfies the
  same rule trivially because the provider IS the vendor's backend.
- **Trust boundary is vendor-shaped.** Enforcement (egress policy, tool permissions,
  spend) spans a single vendor's stack. dac-plane's requirement that enforcement works
  across replaceable providers has no direct analog here to copy — but the tool-permission
  enum shapes (ALWAYS/NEEDS_APPROVAL/BLOCKED and ALLOW_ONCE/ALWAYS/NEVER) transfer.
- **Word collision.** `getLatestAttemptForCurrentEpoch` in the coordinator is about
  *gateway connection attempts* (healthEpoch), not agent attempts. Do not cite that
  symbol for the Attempt concept; use `attemptSequence`/`createAgentRetry` instead.

## 4. Open threads

- Full ConnectRPC service list from `dist/electron-main/proto.cjs` and
  `@anysphere/proto` — the backend's authority surface.
- `node-agent-coordinator` internals: what `createAgent` actually creates per harness
  (BOX pod vs Temporal workflow).
- Computer Use helper (`CUGrokBotService`) and the provider frame protocol
  (`welcome`/`hello`/`stage`/`result`/`error`/`ping` with `providerId`, `computerId`) —
  the remote-computer environment provider.
- `hook-executor` in the local-exec-daemon — their enforcement-hook surface.
