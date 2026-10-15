# Grok Bot exploration — consolidated record

One place for what we did, what we extracted, what Grok Bot's architecture shows, what
corrected itself along the way, and the direction this supports for dac-plane. Detailed
write-ups: `ARCHITECTURE.md`, `DAC-PLANE-MAPPING.md`, `DEEP-DIVES-01.md` (same folder).
Analysis inputs: `../evidence/`.

Date: 2026-10-15. Target: `/Applications/Grok Bot.app` 0.71.2 (Electron, ASAR 42 MB).

## 1. Method and tool chain (what we did, in order)

1. Identified the target: Electron app, `app.asar`, bundle id `com.anysphere.sand`,
   URL schemes `grokbot` + `sand`, helper app "Grok Bot Computer Use"
   (`co.anysphere.grok-bot-computer-use`).
2. Static analysis with [REA](https://github.com/morluto/rea) 6.5.0
   (`npx -y rea-agents@latest analyze-javascript-application <app.asar> --json`).
   First run **failed** on `artifact_integrity_mismatch` (unpacked native binary
   `cursor_proclist.node` re-signed after packaging). Rerun with
   `--integrity-policy record-and-continue` — bytes kept, marked untrusted.
   6 integrity contradictions recorded in the evidence document.
3. REA evidence document (253 MB JSON) inspected via node scripts; node-kind/edge
   aggregation produced `evidence/electron-boundaries-digest.json`.
4. Full ASAR extraction with `npx -y @electron/asar extract` into
   `evidence/extracted/` for manual grep/read (root `package.json`, `dist/`).
5. Three deep dives with targeted greps over the extracted bundles:
   backend RPC services, createAgent/harness flow, local-exec-daemon executors/hooks.
   Digests in `evidence/analysis/`.

Environment note: REA needs Node `^22.19 || ^24.11 || >=26`; system node is 22.16.0,
so Homebrew node@25 (`/opt/homebrew/opt/node@25/bin`) was used. Everything static;
the app was never executed and the app bundle was never modified.

## 2. Extraction inventory (where everything lives)

| Artifact | Content |
|---|---|
| `../evidence/rea-js-analysis.json` | Full REA evidence document: ASAR inventory, integrity contradictions, graph (2846 nodes / 3854 edges), Electron boundary summary, 1031 recorded limitations. Re-greppable; parse with node. |
| `../evidence/electron-boundaries-digest.json` | Digest: browser windows, electron-main/preload/renderer/utility nodes, 18 IPC channels, 10 handlers, 6 context bridges, 4 native addons, packages, storage, workers. |
| `../evidence/extracted/` | Full extracted ASAR: `package.json`, `dist/electron-main/`, `dist/electron-preload/`, `dist/node-agent-coordinator/`, `dist/local-exec-daemon/`, `dist/renderer/`, `dist/deps/`, `dist/native/`. |
| `../evidence/extracted-beautified/` | Same tree reformatted with js-beautify (561 files) — the readable working copy; cite as `extracted-beautified/<path>:<line>`. |
| `../evidence/analysis/services.txt` | All 7 backend services (namespace `aiserver.v1`), 485 methods with kinds (Unary/ServerStreaming/BiDi). |
| `../evidence/analysis/local-exec-executors.txt` | 41 tool executors + full HookKind enum of the local-exec-daemon. |
| `../evidence/analysis/macho-*.json` | REA `inspect-macho` evidence: native addons (unstripped symbols, imports/exports) and the two `dist/native/` executables (`sand-op-launcher`: Security.framework launcher; `sand-webauthn-signer`: Rust + IOKit FIDO signer, no auditable metadata). Thin arm64 slices in `analysis/native-thin/`. |
| `../evidence/analysis/raw-type-names.txt` | (empty — typeName pattern absent in proto.cjs; superseded by services.txt) |

## 3. Three views of "their architecture" (do not conflate)

1. **Runtime topology** — the tree in §4: processes and communication channels,
   inferred from evidence (loads edges, handshake protocols, connection files). This
   is how the app behaves at run time, not a folder layout.
2. **Built output** — `evidence/extracted/dist/`: what actually ships in app.asar.
   Large minified bundles, one or a few per process: `electron-main/main{,-core,-app}.cjs`
   (+ `proto.cjs`, `onepassword-connection-service.cjs`, `chrome-import-worker.cjs`),
   `node-agent-coordinator/main.cjs`, `local-exec-daemon/{main,messages-worker}.cjs`,
   `renderer/assets/*.js`, `deps/` native modules. Their real source file structure is
   stripped; nothing about their repo layout survives in the bundles.
3. **Source monorepo (inferred only)** — from `package.json` workspace deps
   (`@anysphere/agent*`, `agent-core`, `agent-exec`, `grok-bot`, `grok-bot-harness`,
   etc., ~40 packages) and the two leaked relative paths
   (`"file:../vscode/native-modules/cursor-proclist"`,
   `"file:native/notification-settings"`). So their repo is an Anysphere/Cursor-family
   monorepo where sand/grok-bot native modules live under `vscode/native-modules/` and
   `native/`. The package names map to our dist bundles: e.g. `agent-*` and `grok-bot*`
   compile into the coordinator/main bundles, `@sand/*` into renderer/shared.

When citing "their architecture", say which of the three you mean.

## 4. Corrections log (what we revised and why)

Keep these; they prevent repeating mistakes:

1. **Node version.** REA rejects unsupported engines; use node@25 from Homebrew, not
   system node 22.16.0.
2. **Integrity policy.** Unpacked native binaries in macOS-signed apps commonly fail
   ASAR-declared hashes; use `record-and-continue` for analysis-only work, and cite
   those bytes as untrusted.
3. **Symbol collision — attempts.** `getLatestAttemptForCurrentEpoch` in the
   coordinator is about *gateway connection attempts* (healthEpoch), not agent
   attempts. The Attempt analog is `attemptSequence` / `createAgentRetry` /
   `sendPromptAttempt` / `waitAttempt`.
4. **Topology.** First write-up implied the desktop app coordinates agents. Evidence
   corrected this: **agent runtimes run cloud-side** (BOX pods or Temporal workflows);
   the desktop app is a user surface + session/turn UI; the user's local machine is a
   registered *environment* owned by `local-exec-daemon`; the coordinator is the
   gateway-connection and transcript-rehydration broker. See corrected topology in
   `DEEP-DIVES-01.md` §4.
5. **Namespace.** The backend is Cursor's `aiserver.v1` protobuf namespace — Grok Bot
   is Anysphere/SpaceXAI product identity over the sand/Cursor platform; internal
   package name `sand`, homepage cursor.com.

## 5. Grok Bot architecture — consolidated facts

Topology (validated against `aiserver.v1` services, coordinator protocol, daemon):

```
Cloud backend (aiserver.v1, cursor infra)
  - authoritative records: agents, sessions, transcripts, approvals, spend, store
  - harness runtimes: BOX pods | Temporal workflows (kind enum; shadow-mode migration)
  - 485-method ConnectRPC surface, almost all Unary; streaming only for
    migrations/transcripts/user-computer requests
      | ConnectRPC/protobuf
Electron app ("Grok Bot", internal name `sand`)
  - surface + session/turn UI; approval cards; spend ceremonies; transcript rehydration
  - coordinator (utility process): gateway connection management, transcript stream
    reconciliation (sessionId, generation, streamAckUpdatedSeq, epochBump,
    needsRehydrate), per-agent harness tracking
      | localhost HTTP+SSE + token          | computer-use helper app
local-exec-daemon (registered local environment)
  - local data root .grokbot-data-root-v1: host.lock, agents/, gateway.json,
    host-secrets.json, local-exec-update-lease.json, account-retire/generation files
  - 41 tool executors (shell/file/MCP/subagent/computer-use/prechecks, alt harnesses
    pi-*, mini-swe-agent-bash)
  - synchronous hook protocol (before*/after* Shell, MCP, File, Tab, Prompt,
    session, compact, subagent, tool)
  - sandbox policy types, workspace root, egress tunnel, transparent proxy with
    fingerprinting, preflight checks
```

Key mechanisms (evidence in `DAC-PLANE-MAPPING.md` §1):

- Lifecycle enums: task `WORKING/NEEDS_INPUT/DONE`; turn `TurnRequest/Response/Result/
  Failed` with `turnProbeDeadline`; session kinds MAIN/DM/GROUP/SLACK_*/TEAMS_*.
- Approvals persist policy: `ALLOW_ONCE/ALWAYS/NEVER` cards; auto-review
  `APPROVED/DENIED/ALWAYS`; policy revocation is its own call
  (`clearGrokBotConnectorAlwaysAllow`).
- Spend/budget: server-side hard limits and daily spend by category; two-phase
  ceremony `beginGrokBotSpendApproval` → `finishGrokBotSpendApprovalAssertion`
  (WebAuthn-style ASSERT/REGISTER); prepaid top-up; virtual cards.
- Event ingestion: transcript entries carry sequence + ACK state; send durability
  explicit `ACCEPTED/REJECTED/PENDING/UNKNOWN_DURABILITY`; sources tagged server vs
  gateway; rehydration is explicit (`needsRehydrate`, `epochBump`).
- Environment: `SandBoxRunState ABSENT/HIBERNATED/RUNNING/STARTING`; migration phases
  BACKING_UP→CREATING→MOVING→CLEANING_UP→WIPING→DONE/FAILED; upgrade schedules with
  claim semantics `SCHEDULED/CLAIMED/...`; store commits `COMMITTED/RETRY/FENCED`;
  bulk ops with `PARTIALLY_SUCCEEDED`.
- Runtime replacement: harness kind enum + `TemporalHarnessMode OFF/SHADOW/LIVE/BOX` +
  `BoxHarnessMigrationPassState` + internal migration endpoints (preview/execute, holds)
  — second runtime proven in shadow before serving; migration progress is durable.
- Enforcement: hook protocol executed **inside the environment**, synchronous, before
  and after actions; plus allowlist precheck executors; egress policy pulled from the
  authority (`getGrokBotEgressPolicy`), admin push-refresh with outcome states.
- Control port: versioned lifecycle handshake (`hello/ready/settled`,
  `protocolVersion`) between app and coordinator; computer-provider frames
  (`welcome/hello/stage/result/error/ping`, `providerId`, `computerId`).

## 6. Direction this supports for dac-plane (no more guessing)

Mapped against `docs/architecture/` (container view, responsibility map) and the
ownership rules:

**Validated by a shipped system — keep in the baseline:**

1. One authoritative owner for lifecycle records; runtimes own sessions/turns. Grok Bot
   puts this authority in the backend; we put it in the control-plane application over
   PostgreSQL. Same separation, different trust shape.
2. Server/authority-side budget limits (`hardLimit`, daily spend) with a two-phase
   approval ceremony for governed spend. Adopt the shape: budget = server state;
   approval = begin/assert ceremony with a signed receipt.
3. Approval resolutions that persist policy (`ALWAYS/NEVER` + separate revocation
   call). Decision records in dac-plane should carry resolution + scope + a revocation
   path, not just a yes/no.
4. Presigned artifact bytes + manifest commit as the admission point with fencing
   (`COMMITTED/RETRY/FENCED`). Matches "store or authorize upload; verify digest" in
   the container view; adopt the fence status.
5. Events as observations with explicit reconciliation: sequence numbers, ACK cursor,
   epoch bumps, `UNKNOWN_DURABILITY` send status. The outbox/ingestion design should
   assume replays and make durability status explicit end-to-end.
6. Upgrade/maintenance schedules with CLAIMED semantics — lease pattern for
   environment maintenance, consistent with PostgreSQL-lease durability (ADR-0007).
7. Hook protocol executed inside the environment, covering before/after of every
   action class, plus precheck executors. This is a concrete instantiation of the
   "qualified runtime hook or policy/tool/secret gateway" cell in the container view —
   and it argues for defining the dac-plane hook contract early, in the simulator.
8. Versioned control-port handshake with protocolVersion and fail-fast phases — good
   shape for the runtime/environment adapter contract.

**Adoptable patterns (design deltas to consider):**

9. Harness-kind + mode enums with shadow mode and migration-pass tracking: how to
   qualify a second runtime without a big-bang switch. Worth an ADR note under
   "runtime provider open".
10. `NEEDS_INPUT` as a task state rather than a side channel — attention is
    first-class in the state machine.
11. Admin twins for lifecycle operations (break-glass, holds, preview/execute
    migrations) — operations tooling belongs in the same authority surface.
12. Local-tool-permission **ceiling**: user can loosen permissions below a durable
    ceiling, never above. Fits dac-plane's budget/policy model for local adapters.
13. Machine registration (`registerSandMachine`) — even the user's own machine is a
    registered, removable environment resource with its own settings. Supports the
    "local adapter: development only" cell: treat the local adapter as a full
    environment resource with registration and cleanup, from day one.

**Where we diverge deliberately (recorded so it does not look like an oversight):**

14. Lifecycle authority location: Grok Bot's backend is single-vendor; dac-plane keeps
    the lifecycle core product-owned so providers stay replaceable. Grok Bot cannot be
    copied here; their *enums and contracts* transfer, their trust topology does not.
15. Grok Bot's RPC surface is 485 methods mostly Unary — the Surface API should stay
    command-oriented (verbs + idempotency nonces, like their `clientNonce`) rather than
    event-broker-shaped; streaming only where genuinely needed (transcripts, migrations).

**Concrete contract vocabulary worth reusing in the simulator/adapter contracts:**

- attempt sequence numbers; session/turn split; turn result kinds
- send status incl. UNKNOWN_DURABILITY
- run state (ABSENT/STARTING/RUNNING/HIBERNATED) + migration phases + claim states
- commit fence status (COMMITTED/RETRY/FENCED)
- approval resolutions (once/always/never + revoke)
- hook kinds covering the action lifecycle
- control-port frames: lifecycle(hello/ready/shutdown, protocolVersion) /
  request(requestId, method, args, demand person|background) / cancel / reply(ok |
  failed with typed failure) / event(family, payload) — `DEEP-DIVES-03.md` §1
- governed-action taxonomy with scope classes global/setting/subject/call, permission
  lattice never/ask/always clamped by a ceiling, generation-counter invalidation,
  single-use approvals retired via a retirements ledger, standing grants scoped
  action/subject, `serverAuthoritative` override — `DEEP-DIVES-03.md` §2-3
- computer-provider tools with app/screen scopes and staleness tokens
  (coordinate_token, snapshot_id), refusal-with-remediation — `DEEP-DIVES-03.md` §4

## 7. Open threads (queued, in priority order)

1. ~~Coordinator control-port protocol~~ — DONE, `DEEP-DIVES-03.md` §1.
2. ~~Computer Use helper as second environment provider~~ — DONE, `DEEP-DIVES-03.md` §4.
3. ~~ConnectRPC message fields for lifecycle messages~~ — DONE, `DEEP-DIVES-04.md` §1
   (transcript watch frames, agent/session/box binding, turns, two-phase spend
   ceremony contract).
4. ~~Hook-executor payload shapes~~ — DONE, `DEEP-DIVES-04.md` §2 (all 10 hook kinds
   with request/response schemas; hooks decide permission and rewrite tool input).
5. Native analysis: boundary evidence DONE (`DEEP-DIVES-02.md` §2, `analysis/macho-*.json`).
   Full pseudocode requires installing Ghidra 12.1.x (`GHIDRA_INSTALL_DIR`) — only
   if WebAuthn-signer internals matter.

Static analysis of the target is now substantially complete: process topology,
authority surface, enforcement path (both halves), event contracts, hook contract
and native boundaries all have evidence-backed records. Remaining work is synthesis
into dac-plane decisions (ADR drafts) rather than further extraction.
