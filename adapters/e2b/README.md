# E2B Cloud environment adapter

`dac-e2b-adapter` is an independent environment adapter. It imports `adapterkit` and its own durable journal; it does not import DAC internals. It uses E2B Cloud's managed control plane at `https://api.e2b.app`. E2B operates the Linux hosts and Firecracker microVMs; this deployment requires no customer-owned KVM machine.

## Managed provider surface

The adapter uses E2B control API v2 to create, inspect, list by metadata, and delete sandboxes. Its provider-specific request and response fields stay inside this module. The sandbox-create response uses `sandboxId`; sandbox lookup/list responses use `sandboxID`. The adapter accepts each documented form at its respective boundary.

E2B Cloud is managed software: E2B does not expose a customer-pinnable server build or source revision. Qualification evidence pins the API surface, E2B CLI and SDK package versions, DAC adapter commit and observed provider response metadata. That is not a claim that the E2B Cloud backend is immutable.

## Safety and replay boundary

The adapter persists operation fingerprints and sandbox mappings in bbolt, stores the stable DAC operation ID as E2B sandbox metadata, and looks up that metadata before retrying an uncertain create. If the provider cannot establish whether the first create succeeded, the operation remains uncertain and the adapter does not issue a second create. It does not advertise `operation.replay` without a provider-side guarantee.

Sandbox creation sets `allow_internet_access=false` and denies outbound IPv4 (`0.0.0.0/0`) through the v2 network policy. The qualification verifies host HTTPS connectivity, then performs an HTTPS request from the sandbox and requires a network-denial result; a bare TCP handshake is not treated as an Internet request. E2B Cloud rejects an IPv6 deny CIDR (`::/0`), so this qualification claims only the IPv4 behavior it probes. E2B API keys are read from `APIKEY_E2B` by the qualification runner and are not persisted or logged. The adapter service itself reads `DAC_E2B_API_KEY_FILE` at runtime.

For task attempts, DAC sends an empty object to `provision` and the stable environment-operation ID is used as E2B metadata. The adapter records the returned opaque `resource_ref` and emits `environment.observed` with the observed running state. A `stop` request names the original provision operation; the adapter resolves its journal mapping, deletes the matching sandbox, and emits an absent observation. If stop reaches the adapter before provision, the journal records cancellation against that provision ID so a later create is refused. Migration-stage requests continue using their separate migration receipt contract.

## Sandbox lifetime

E2B kills a sandbox created through control API v2 after 300 seconds unless the request says otherwise, and running commands or file operations do not extend that. The adapter therefore sets `timeout` on every create, from `DAC_E2B_SANDBOX_TIMEOUT`. The default of one hour is the longest lifetime the Hobby plan accepts; the Pro plan accepts up to 24 hours. A value above the plan's limit is a definite create rejection, which fails the provision and leaves it safe to retry after the setting is corrected.

The timeout is a fixed backstop, not a keep-alive. DAC stops the sandbox when its Attempt ends (ADR-0014); the timeout bounds the cost of a stop that never lands. Set it longer than the longest expected Attempt, because the adapter does not renew it and an Attempt outliving it loses its sandbox. `observe` stays read-only and never extends the lifetime.

Creates set `autoPause=false`, so an expired sandbox is killed. A paused sandbox has no lifetime of its own, and stop semantics do not retain workspaces, so pausing would turn a lost stop into storage kept indefinitely.

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
| `DAC_E2B_SANDBOX_TIMEOUT` | sandbox lifetime as a Go duration such as `1h`; defaults to `1h` |
| `DAC_ENVIRONMENT_ATTACH_KEY_FILE` | key shared with the runtime adapter; set, each task sandbox starts exec-server, which needs the template below |

`just build-e2b-codex-template` builds [`template/e2b.Dockerfile`](template/e2b.Dockerfile) with the pinned CLI. See
[ADR-0016](../../docs/adr/0016-run-agent-commands-inside-the-environment.md).

## Verification

The live isolation probe uses the official E2B CLI, pinned to `@e2b/cli` 2.23.0 and SDK 2.61.0 in `qualification/package.json` and its lockfile. The qualification runner installs this isolated toolchain under `.work/` and requires `APIKEY_E2B`; it creates two short-lived managed sandboxes, probes filesystem/process isolation and denied egress, verifies observation and cleanup, and reopens the durable journal to check mapping recovery. E2B Cloud usage is billable beyond account credits; the run is bounded to two default sandboxes and cleans them up after failure as well as success.

`just test-e2b-adapter` runs deterministic fake-provider tests and adapter-kit ControlFrame conformance. `just qualify-e2b-environment` runs against E2B Cloud; GitHub Actions exposes the same command as a manual workflow. Deterministic tests do not establish provider isolation. Keep the provider off/unqualified until the live evidence contains passing observations for every required live gate.
