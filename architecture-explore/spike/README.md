# Spike: OCE (OCC) + dedicated Codex integration (ADR-0011)

Status: **Q1 failed; stock OCE rejected for current adoption**. The first Linux run stopped on missing kernel
NetworkPolicy support, but the later Mac/k3d+Cilium run passed OCC's NetworkPolicy check and reached real dedicated Codex
Agent deployment. The Agent still failed readiness because OCE's dedicated Codex startup probe did not pass the configured
OpenAI-compatible `baseUrl` to Codex as `openai_base_url`, causing Codex to call OpenAI with the OpenCodex admission token.
See `docs/qualification/` and `docs/adr/0012-adoption-decision-occ.md`.

## Reproduce
```sh
spike/scripts/00-prereqs.sh            # Docker 28, OCE @ 07590c64, mise toolchain, kernel feature check
spike/scripts/01-occ-up.sh             # occ dev up (Kubernetes-only, no sandbox)
# Historical OCE reproduction only. Current decision: do not continue stock-OCE qualification
# unless upstream fixes the dedicated Codex base-url probe or the project explicitly accepts a fork.
OPENAI_API_KEY_FILE=~/secure/openai.key node spike/scripts/02-deploy-codex-agent.mjs spike-codex
spike/scripts/99-down.sh
```
Results land in `spike/out/` (git-ignored); copy reviewed evidence into `spike/evidence/`.

## Local workarounds (`spike/shim/`, prepended to PATH by 01-occ-up.sh)
| Shim | Why |
|------|-----|
| `kubectl` | retries the one-shot `get --raw=/version` probe after cluster create (LB warm-up race) |
| `k3d image import` | k3d 5.8.3 deadlocks importing against Docker 28 here; imports with `ctr` in the node instead |
| `k3d cluster create` | adds `--flannel-backend=host-gw` because the kernel lacks vxlan |
These change no OCC behavior. The NetworkPolicy check was deliberately **not** bypassed.

## Native slice status
`03-slice.mjs` exists for the smoke/approval/deny/cancel/stale/history slice, but it is intentionally not counted as OCE
evidence because the deployed OCC Agent never reached runtime readiness. Running it against patched internals would prove a
fork, not stock-OCE adoption.
