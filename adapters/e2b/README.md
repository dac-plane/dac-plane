# E2B Cloud environment adapter

`dac-e2b-adapter` is an independent environment adapter. It imports `adapterkit` and its own durable journal; it does not import DAC internals. It uses E2B Cloud's managed control plane at `https://api.e2b.app`. E2B operates the Linux hosts and Firecracker microVMs; this deployment requires no customer-owned KVM machine.

## Managed provider surface

The adapter uses E2B control API v2 to create, inspect, list by metadata, and delete sandboxes. Its provider-specific request and response fields stay inside this module. The sandbox-create response uses `sandboxId`; sandbox lookup/list responses use `sandboxID`. The adapter accepts each documented form at its respective boundary.

E2B Cloud is managed software: E2B does not expose a customer-pinnable server build or source revision. Qualification evidence pins the API surface, E2B CLI and SDK package versions, DAC adapter commit and observed provider response metadata. That is not a claim that the E2B Cloud backend is immutable.

## Safety and replay boundary

The adapter persists operation fingerprints and sandbox mappings in bbolt, stores the stable DAC operation ID as E2B sandbox metadata, and looks up that metadata before retrying an uncertain create. If the provider cannot establish whether the first create succeeded, the operation remains uncertain and the adapter does not issue a second create. It does not advertise `operation.replay` without a provider-side guarantee.

Sandbox creation sets `allow_internet_access=false` and denies outbound IPv4 (`0.0.0.0/0`) through the v2 network policy. E2B Cloud rejects an IPv6 deny CIDR (`::/0`); the qualification claims only the IPv4 behavior it probes. It verifies host connectivity first and rejects unexpected error classes. E2B API keys are read from `APIKEY_E2B` by the qualification runner and are not persisted or logged. The adapter service itself reads `DAC_E2B_API_KEY_FILE` at runtime.

## Configuration

| Setting | Purpose |
|---|---|
| `DAC_ADAPTER_URL` | DAC adapter WebSocket endpoint |
| `DAC_PROVIDER_ID` | registered environment provider UUID |
| `DAC_ADAPTER_TOKEN_FILE` or client-credentials settings | adapter authentication |
| `DAC_E2B_BASE_URL` | E2B Cloud API origin; defaults to `https://api.e2b.app` in qualification |
| `DAC_E2B_API_KEY_FILE` | file-mounted E2B Cloud team key for the adapter process |
| `DAC_E2B_TEMPLATE_ID` | E2B Cloud template ID; qualification defaults to `base` |
| `DAC_E2B_STATE` | persistent bbolt state path |

## Verification

The live isolation probe uses the official E2B CLI, pinned to `@e2b/cli` 2.23.0 and SDK 2.61.0 in `qualification/package.json` and its lockfile. The qualification runner installs this isolated toolchain under `.work/` and requires `APIKEY_E2B`; it creates two short-lived managed sandboxes, probes filesystem/process isolation and denied egress, verifies observation and cleanup, and reopens the durable journal to check mapping recovery. E2B Cloud usage is billable beyond account credits; the run is bounded to two default sandboxes and cleans them up after failure as well as success.

`just test-e2b-adapter` runs deterministic fake-provider tests and adapter-kit ControlFrame conformance. `just qualify-e2b-environment` runs against E2B Cloud; GitHub Actions exposes the same command as a manual workflow. Deterministic tests do not establish provider isolation. Keep the provider off/unqualified until the live evidence contains passing observations for every required live gate.
