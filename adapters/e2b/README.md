# E2B Embed environment adapter

`dac-e2b-adapter` is an independent environment adapter. It imports `adapterkit` and its own durable journal; it does not import DAC internals. Provider API keys are read from a file and are never placed in errors or event payloads.

## Provider pins

The planned release `2026.30` at `f32ee8a2a50052f32e3632ceb451111a98dd5104` has no `embed/` tree. The Embed package and its Compose configuration are pinned at `7a69f45621f864495695e698dc5a2b93a56d2d52`. That package's `.env` pins the runtime source as `7278c2a380767c9989da73cf4c04b1af1b32da18` and pins each E2B service image by version tag. The qualification recipe records the pulled registry digests.

## Safety and replay boundary

The adapter persists operation arguments and resource mappings before acknowledging work. It adds the stable DAC operation ID to the E2B sandbox metadata. If the create response is lost, it queries `/v2/sandboxes` by that metadata before returning success. If no sandbox is visible, it leaves the operation outcome unknown and does not issue another create. This avoids duplicate effects but can require operator reconciliation if Embed remains unable to reveal the first request. The adapter does not advertise `operation.replay`: the pinned offline Embed configuration does not enable E2B's opt-in idempotency middleware.

New sandboxes default to `allow_internet_access=false`. Keep DAC's E2B API URL on loopback or HTTPS. The upstream Compose stack uses host networking; run it only on an isolated disposable Linux/KVM host. Its 5008 unauthenticated orchestrator endpoint and its other non-client host ports must be blocked from external networks. Do not publish or forward port 5008.

## Configuration

| Setting | Purpose |
|---|---|
| `DAC_ADAPTER_URL` | DAC adapter WebSocket endpoint |
| `DAC_PROVIDER_ID` | registered environment provider UUID |
| `DAC_ADAPTER_TOKEN_FILE` or client-credentials settings | adapter authentication |
| `DAC_E2B_BASE_URL` | E2B API origin; HTTP is accepted only on loopback |
| `DAC_E2B_API_KEY_FILE` | file-mounted Embed team key |
| `DAC_E2B_TEMPLATE_ID` | provisioned template, normally `base` |
| `DAC_E2B_STATE` | persistent bbolt state path |

## Verification

The live isolation probe uses the official E2B CLI, pinned to `@e2b/cli` 2.23.0 and SDK 2.61.0 in `qualification/package.json` and its lockfile. The qualification recipe installs this isolated toolchain under `.work/`; it is not part of the adapter image or product runtime.

`just test-e2b-adapter` runs fake-provider crash/reconciliation tests and the published adapter-kit ControlFrame conformance suite. `just qualify-e2b-environment` fetches the pinned Embed package and is gated on a disposable Linux/KVM host with Docker Compose and an isolated host firewall. It records image digests and runs the provider-backed lifecycle test. A passing deterministic suite is not a live E2B qualification; provider disposition stays off/shadow until the live evidence note contains all required isolation, egress, restart, and cleanup observations.
