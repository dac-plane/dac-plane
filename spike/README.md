# Spike: OCE (OCC) + dedicated Codex integration (ADR-0011)

Status: **stopped at Q1** on the agent box (kernel without `ip_set` => no NetworkPolicy enforcement) and
**no model credential**. See `docs/qualification/`.

## Reproduce
```sh
spike/scripts/00-prereqs.sh            # Docker 28, OCE @ 07590c64, mise toolchain, kernel feature check
spike/scripts/01-occ-up.sh             # occ dev up (Kubernetes-only, no sandbox)
OPENAI_API_KEY_FILE=~/secure/openai.key node spike/scripts/02-deploy-codex-agent.mjs spike-codex
#   or, without a key, observe the fail-closed path:
node spike/scripts/02-deploy-codex-agent.mjs --negative-fixture
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

## Not yet written (needs a live runtime to avoid guessing the protocol)
`03-slice.mjs`: product task -> gateway session/turn via `@openclaw/gateway-client`, subscribe to
`exec.approval.requested`, persist correlation (`product.occ_binding`, `product.native_turn`,
`attention_request.native_request_ref`), resolve with `exec.approval.resolve`, then Q2–Q5 scenarios.
