# Grok Bot — architecture exploration (REA)

Static reverse-engineering of the locally installed `Grok Bot.app` using
[REA (morluto/rea)](https://github.com/morluto/rea), stored here so it stays with the
architecture-explore evidence.

## Target

- Path: `/Applications/Grok Bot.app`
- Version: 0.71.2, Electron app, ASAR container (`Contents/Resources/app.asar`, 42 MB)
- Analysis date: 2026-10-15

## Method

```sh
# analysis (read-only, static)
PATH="/opt/homebrew/opt/node@25/bin:$PATH" npx -y rea-agents@latest \
  analyze-javascript-application "/Applications/Grok Bot.app/Contents/Resources/app.asar" \
  --json --integrity-policy record-and-continue

# extraction for manual inspection
npx -y @electron/asar extract <app.asar> evidence/extracted
```

Notes and caveats:

- REA 6.5.0 requires Node `^22.19 || ^24.11 || >=26`; system node is 22.16.0, so the
  Homebrew node@25 binary was used (engines warning only, run succeeded).
- First run failed with `artifact_integrity_mismatch`: the ASAR's declared SHA256 for the
  unpacked native addon `dist/deps/cursor-proclist/build/Release/cursor_proclist.node`
  contradicts the observed bytes (macOS packaging commonly re-signs unpacked native
  binaries). Rerun used `--integrity-policy record-and-continue`, so those bytes are
  recorded as untrusted. There are 6 recorded integrity contradictions in total.
- REA truncated some application-relationship expansion under its result budget
  (`resource_constraint`); per-file electron facts are complete, cross-file expansion is
  partial.
- All analysis is static; nothing in the app was executed or modified.

## Evidence index

| File | What it is |
|------|------------|
| `evidence/rea-js-analysis.json` | Full REA evidence document (253 MB). `normalized_result.graph` is the usable graph. |
| `evidence/rea-js-analysis.stderr.log` | Run stderr (engine warning, integrity progress). |
| `evidence/electron-boundaries-digest.json` | Extracted Electron-process, IPC, bridge, native-addon, package, endpoint nodes. |
| `evidence/extracted/` | Full extracted ASAR for manual grep/read (package.json, dist/). |
| `evidence/extracted-beautified/` | All 561 JS files reformatted (js-beautify, readable); mirrors extracted paths so facts can be cited `extracted-beautified/<path>:<line>`. |
| `evidence/analysis/` | Derived digests: `services.txt` (485 backend RPC methods), `local-exec-executors.txt` (41 executors + HookKind enum), `macho-*.json` (Mach-O layout + symbols/imports/exports for the two N-API addons and the two native executables), `rust-sand-webauthn-signer.json` (inferred rustc commit; no auditable crate list), thin arm64 slices under `native-thin/`. |
| `notes/ARCHITECTURE.md` | The written-up architecture with evidence pointers. |
| `notes/DAC-PLANE-MAPPING.md` | Concept-by-concept mapping to `docs/architecture/` baseline (task/attempt/approval/budget/enforcement/environment), with transferable patterns and divergences. |
| `notes/DEEP-DIVES-01.md` | Deep dives: backend RPC surface, createAgent/harness migration, local machine as environment (executors, hooks, leases). |
| `notes/DEEP-DIVES-02.md` | Beautified codebase reading map + native addon API boundaries (Mach-O evidence, unstripped symbols, decompilation status). |
| `notes/DEEP-DIVES-03.md` | Control-port protocol (v3 frames, demand classes, failure taxonomy), cloud→local enforcement stream (approval ids + standing/authorized-by attribution + retirements ledger), local authorization evaluator (action taxonomy, scope classes, permission lattice + ceiling), Computer Use helper (Swift MCP stdio server, 16 tools, app/screen scopes, staleness tokens). |
| `notes/DEEP-DIVES-04.md` | Lifecycle protobuf message fields (transcript watch frames, agent/session/box, turns, spend ceremony), full hook contract (10 hook kinds, request/response shapes, permission + updated_input interception), exec-daemon tool envelope. |
| `notes/MASTER-RECORD.md` | Consolidated record: methodology, extraction inventory, corrections log, Grok Bot architecture facts, direction for dac-plane (validated/adoption/divergence), queued open threads. Read this first. |
