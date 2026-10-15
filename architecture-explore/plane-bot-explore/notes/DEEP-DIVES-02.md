# Deep dives 02 — beautified codebase and native addon boundaries

## 1. Beautified bundle tree (`evidence/extracted-beautified/`)

Every `.cjs`/`.js` in the extracted ASAR reformatted with js-beautify (561 files,
38 MB minified → 59 MB formatted; non-JS files copied verbatim; `locales/` skipped).
Line-level traceability is preserved: beautified files mirror the extracted paths, so
any fact can be cited as `extracted-beautified/<path>:<line>`.

Reading map (the files that matter, with sizes as formatted):

| File | Content |
|---|---|
| `dist/electron-main/main.cjs` | Thin entry (12 KB) → loads main-core |
| `dist/electron-main/main-core.cjs` | Core wiring: IPC, windows, sentry, daemon/credential state files, egress tunnel controller, local tool permission settings |
| `dist/electron-main/main-app.cjs` | Application surface (2.5 M): settings stores, roster, transcripts, agent commands |
| `dist/electron-main/proto.cjs` | Complete `aiserver.v1` protobuf definitions (messages with field numbers + 7 services) |
| `dist/electron-main/onepassword-connection-service.cjs` | 1Password credential brokering |
| `dist/electron-main/chrome-import-worker.cjs` | Browser import |
| `dist/electron-preload/preload*.cjs` | The 5 preload scripts (bridge surfaces) |
| `dist/node-agent-coordinator/main.cjs` | Gateway connection manager + transcript reconciliation + harness tracking + method registry (381 methods) |
| `dist/local-exec-daemon/main.cjs` | Local environment: 41 executors, hook protocol, sandbox policy, egress tunnel, proxy |
| `dist/local-exec-daemon/messages-worker.cjs` | Messages processing worker |
| `dist/renderer/assets/*.js` | Renderer chunks (index-wlkGG389 2.2 MB is the main UI bundle) |

Example of the readability gain: the transcript reconciliation state machine that was
one minified line is now inspectable at
`dist/node-agent-coordinator/main.cjs:30860-30923` (`generation`, `epochBump`,
`streamAckUpdatedSeq`, `needsRehydrate` resets).

## 2. Native addons — Mach-O evidence (`evidence/analysis/macho-*.json`)

REA `inspect-macho` (built-in macOS native inspection, no Hopper/Ghidra needed) on both
Anysphere-authored addons. Symbols are **unstripped**, so the API boundary is exact:

### `cursor_proclist.node` (arm64, 103 KB, min macOS 11, SDK 26.2)

Real exports (C++ mangled, demangled):

- `getDescendantPids(const std::vector<int>&, bool)` — process-tree enumeration
- `getProcessInformation(int, ProcessInfo&)` — per-pid info (pid argument)
- `getSystemMemoryInfo(SystemMemoryInfo&)` — memory stats
- `Init(Napi::Env, Napi::Object)` + `napi_register_module_v1` — N-API registration

Dependencies: only `libc++.1.dylib` + `libSystem.B.dylib` — no app or framework
imports; a self-contained process enumerator. Purpose in the app: the agent's process
tooling (list/inspect local processes from the user's machine environment).

### `sand_notification_settings.node` (arm64, 101 KB)

Real exports:

- `getNotificationSettings(NotificationSettingsInfo&)`
- N-API registration (`napi_register_module_v1`)

Dependencies: Foundation, UserNotifications, CoreFoundation, libobjc — reads macOS
notification permission settings via `UNUserNotificationCenter` /
`CFBundleGetVersionNumber`.

### Two standalone native executables (`dist/native/`)

- `sand-op-launcher` (universal x86_64/arm64, 136 KB): Mach-O executable linking
  Security.framework — launcher for the 1Password `op` CLI with security checks.
- `sand-webauthn-signer` (universal, 5.0 MB): **Rust** executable (inferred rustc commit
  `48a229ce...`, evidence kind inferred) linking IOKit — FIDO/WebAuthn hardware-device
  access for spend/passkey ceremonies. No cargo-auditable metadata embedded, so the
  crate list is not statically recoverable; decompilation would be required for more
  than the boundary. Thin arm64 slices kept at `analysis/native-thin/` for that.
  Evidence: `analysis/macho-sand-op-launcher.json`, `analysis/macho-sand-webauthn-signer.json`,
  `analysis/rust-sand-webauthn-signer.json`.

### Assessment

Both addons are narrow, well-bounded N-API wrappers around OS capabilities — clean
precedent for dac-plane's "environment provider owns process workspace" port: native
surface kept minimal (one function per concern, typed structs, async workers), with
the policy logic on the JS side.

Tree-sitter addons (`tree_sitter_runtime_binding.node`, `tree-sitter-bash`) are
third-party upstream builds — not inspected (out of scope for architecture learning).

### Decompilation status

No decompiler provider is configured on this machine (Hopper absent, Ghidra
`GHIDRA_INSTALL_DIR` unset, IDA unregistered). Current native evidence = full Mach-O
layout + symbols + imports/exports, which is sufficient for boundary facts. Full
pseudocode of these 100 KB addons only matters if we later need the internal
algorithms (e.g. exactly how memory/permission values are computed); if so, install
Ghidra 12.1.x and set `GHIDRA_INSTALL_DIR`, then use REA native commands — recorded
here so the step is known, not guessed.
