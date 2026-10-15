# Grok Bot — architecture overview

Derived from REA static analysis (`evidence/rea-js-analysis.json`) plus direct inspection
of the extracted ASAR (`evidence/extracted/`). Every claim below is anchored to an artifact
path; nothing was inferred from runtime observation.

## 1. Identity

| Fact | Value | Evidence |
|------|-------|----------|
| Display name | Grok Bot 0.71.2 | `Info.plist` |
| Internal package name | `sand` | extracted `package.json` (`"name": "sand"`) |
| productName / desktopName | `Grok Bot` / `grok-bot.desktop` | extracted `package.json` |
| Author / homepage | `SpaceXAI` / `https://cursor.com` | extracted `package.json` |
| Bundle identifier | `com.anysphere.sand` | `Info.plist` |
| URL schemes | `grokbot`, `sand` | `Info.plist` |
| Build date / track | 2026-10-14, `sandTrack: stable` | extracted `package.json` |
| Computer-use helper | `Grok Bot Computer Use.app`, id `co.anysphere.grok-bot-computer-use` | `Contents/Helpers/` |

Interpretation: the app is Anysphere's "sand" desktop agent platform with the Grok Bot
product identity layered on top. Dependency names confirm the lineage: `@anysphere/grok-bot`,
`@anysphere/grok-bot-harness`, `@anysphere/xai-commerce-prepaid`, plus many Cursor-family
packages (`@anysphere/cursor-config`, `cursor-proclist`).

## 2. Process architecture

```
Electron
├── main process            dist/electron-main/main.cjs
│   ├── main-core.cjs       core wiring (IPC, windows, sentry, debugger use)
│   ├── main-app.cjs        application surface
│   ├── proto.cjs           protobuf/ConnectRPC glue
│   ├── onepassword-connection-service.cjs
│   └── chrome-import-worker.cjs
├── preload scripts         dist/electron-preload/
│   ├── preload.cjs                     main window
│   ├── preload-webview.cjs
│   ├── preload-vnc.cjs                 VNC viewer surfaces
│   ├── preload-avatar-source.cjs
│   └── preload-desktop-mount-panel.cjs desktop-mount panel
├── renderers
│   ├── dist/renderer/index.html                    (main UI, 2.2 MB index bundle)
│   └── dist/renderer/src/electron-renderer/desktop-mount/panel/panel.html
├── utility / node services
│   ├── dist/node-agent-coordinator/main.cjs        agent lifecycle coordinator (createAgent)
│   └── dist/local-exec-daemon/main.cjs + messages-worker.cjs   PTY/shell/sandbox execution
└── helper app              Grok Bot Computer Use.app (CUGrokBotService)
```

Evidence: `loads` edges in the REA graph (package.json → main.cjs; main-core.cjs → utility
entrypoints), `electron-main`/`electron-renderer`/`electron-utility` nodes in
`electron-boundaries-digest.json`.

## 3. IPC and trust boundary

- Preload sends on literal channels: `sand:renderer-alive`,
  `sand:renderer-boot-gate-opened`, `sand:renderer-boot-gate-landed`,
  `sand:boot-snapshot-sync`, `sand:coordinator-port` (coordinator port handed to the
  renderer — renderer connects to the local agent coordinator over that port).
- Dynamic keyed channels via `t.createKey("start" | "status" | "scope" |
  "structured-log" | "envelope")` — 13 dynamic-channel operations vs 5 literal.
- Context bridge APIs exposed to renderers: `desktop`, `desktopMount` (two surfaces),
  `sandRenderTrace`, `coordinatorPort`.
- REA observed sender-validation checks in main handlers (2 observations).
- Storage: 3 local-storage persistence points found.

Evidence: `sends`/`handles` edges, `ipc-channel` and `context-bridge-api` nodes in
`electron-boundaries-digest.json`; summary block of the REA report (18 IPC operations,
6 context-bridge APIs).

## 4. Native modules

| Module | Role (from name/wiring) | Note |
|--------|------------------------|------|
| `cursor-proclist` (`cursor_proclist.node`) | process listing | ASAR integrity mismatch on the unpacked binary (recorded untrusted) |
| `sand-notification-settings` (`sand_notification_settings.node`) | notification settings | loads from `dist/deps/...` |
| `tree-sitter` + `tree-sitter-bash` | bash parsing (likely for shell command analysis) | prebuilt for darwin-arm64 |
| `dist/native/sand-op-launcher` | launcher helper | |
| `dist/native/sand-webauthn-signer` | WebAuthn signing helper | |

## 5. Backend and network

Hosts found in bundles (static strings): `api2.cursor.sh`, `api.origin.cursor.com`,
`prod.authentication.cursor.sh` (backend), `console.statsig.com` / `statsigapi.net`
(feature flags), `gtm-api.corp.anysphere.co` (internal metrics), Sentry (crash), plus
Grok-side references (`x.ai`, `grok.com`, `docs.x.ai`). Transport is ConnectRPC over
protobuf (`@connectrpc/connect`, `@bufbuild/protobuf`, `@anysphere/proto`).
Request headers observed in code: `x-cursor-team-id`, `x-statsig-region`, `x-request-id`.

## 6. Frontend stack

React (catalog version), TanStack Query, Tiptap editor, StyleX, Base UI, Lingui i18n,
react-markdown + remark-gfm, mermaid, cytoscape, katex, pdf.js, xlsx, mammoth (docx),
`@dnd-kit`. Notable renderer chunks: `chunk-cynefin-*`, `chunk-team-bot-line-render-*`,
`index.eager-app`, `index.eager-platform`, `index.eager-vendor`.

## 7. Workspace-package decomposition (backend logic lives outside the bundle)

The `@anysphere/*` workspace dependencies in `package.json` outline the agent runtime:

- agent lifecycle: `@anysphere/agent`, `agent-core`, `agent-client`, `agent-exec`,
  `agent-kv`, `agent-store-ids`, `agent-store-sync`, `agent-summarization`,
  `agent-transcript`
- execution: `@anysphere/local-exec`, `shell-exec`, `mcp-agent-exec`, `mcp-core`
- model/chat: `@anysphere/chat-inference`, `chat-inference-proto`, `model-selection`
- platform: `@anysphere/context`, `context-rpc`, `@sand/client`, `@sand/shared`,
  `orbit-client`, `proto`, `constants`, `utils`
- product surfaces: `voice-call-harness`, `browser-smoke`, `link-preview`,
  `marketplace-category`, `canvas-shared`, `file-kind-icons`, `xai-commerce-prepaid`,
  `messages-mac`

These are bundled into `main.cjs`/`main-core.cjs` at build time; the on-disk ASAR only
contains the compiled output.

## 8. Open threads (for follow-up REA sessions)

- What exactly does `node-agent-coordinator` coordinate (agent processes, tools)?
- The `desktopMount` panel and `preload-vnc` — remote-computer mounting surface?
- The dynamic keyed IPC channels (`start`/`status`/`scope`/`envelope`) — session protocol
  between renderer and coordinator?
- Computer Use helper (`CUGrokBotService`) capabilities.
- Backend surface: full ConnectRPC service list from `proto.cjs` / `@anysphere/proto`.
- Native analysis of the 4 `.node` addons via Ghidra/Hopper if needed.
