# Q1: OCC real deployment (OCE + dedicated Codex, local k3d)

- Date: 12 Oct 2026, 22:40–23:40 WAT
- **Result: FAIL (environment) — real execution NOT RUN**
- Upstream: openclaw/openclaw-enterprise `07590c64e27ac61c9ad60d9ab61424b70281011a` (same commit the spec inspected)
- Runtime image (built locally by `occ dev up` from `deploy/runtime/Dockerfile`):
  `docker.io/library/openclaw-enterprise-runtime@sha256:2b6c2829072d7464e4f0419c8612039517eb71411dc0095be7081a14112d4919`
  (OpenClaw source `90d30a1178a79dddd92e6190b66b95d89dfb3ca8`, Codex `0.163.0-alpha.2` per `deploy/runtime/README.md`)
- Controller image: built and imported (`openclaw-development/import-b9b4e5950649@sha256:0227255a49c33c5ac9c164ae8e5c321509a8179ae004432d63cffba25ccc9d6e`)
- Postgres: `postgres:18.6@sha256:86c951e05bf56c93d95d397747fb8820ac76cc3bedb78f43abd83eedbe3666ae`
- k3s node image: `rancher/k3s:v1.36.4-k3s1@sha256:edad48e12bf81c3a09ac1c05c0c0ffaaa22145980b989d6fae84543a76b83657`
- Tools (OCE `mise.toml`): node 24.16.0, go 1.27.1, pnpm 11.15.1, k3d 5.8.3, kubectl 1.35.0, helm 3.19.2; Docker 28.5.1

## Commands
`spike/scripts/00-prereqs.sh`, then `spike/scripts/01-occ-up.sh` (= quickstart: `pnpm cli:build`;
`OCC_DEVELOPMENT_COMPUTE_DRIVER=kubernetes OCC_DEVELOPMENT_CONTROL_PLANE=kubernetes OCC_DEVELOPMENT_SANDBOX_DRIVER=none ./bin/occ dev up`).
Agent deployment script prepared but not reached: `spike/scripts/02-deploy-codex-agent.mjs`.

## Observed (in order)
| # | Step | Outcome | Fix / workaround |
|---|------|---------|------------------|
| 1 | No Docker on box | — | Installed Docker; 26.1 (Debian) later replaced by static 28.5.1 |
| 2 | k3d cluster: API unreachable through k3d LB (`upstream timed out`) | Pre-existing `iptables-legacy` FORWARD policy DROP dropped bridged traffic | Narrow legacy rules for `br-+` (00-prereqs.sh) |
| 3 | `occ dev up` one-shot `kubectl get --raw=/version` failed right after create | LB warm-up race | `spike/shim/kubectl` retries that probe only |
| 4 | Image export: `docker image save --platform` unknown | Docker 26 too old | Docker 28.5.1 |
| 5 | `k3d image import` panic: `all goroutines are asleep - deadlock` | k3d 5.8.3 vs Docker 28 here | `spike/shim/k3d` imports via `ctr` in the node |
| 6 | k3s restarting: `flannel ... failed to create vxlan device: operation not supported` | Box kernel 6.12.94+ has no vxlan | shim adds `--flannel-backend=host-gw` (single node) |
| 7 | Pod image pulls failed (DNS timeouts from containers) | same legacy FORWARD DROP for egress | rule from step 2 |
| 8 | cert-manager, Envoy Gateway, Postgres, controller image all came up | OK | — |
| 9 | **`Verifying Kubernetes network isolation before configuring gateway trust...` never passes** | `wrong-label` probe pod reaches the target (HTTP 200) although a NetworkPolicy denies it. k3s log: `Skipping network policy controller start, ipset save failed: ipset v7.16: Kernel error received: Invalid argument`. `/proc/net/ip_set` absent. | **Blocker.** Not bypassed: OCC requires an enforcing NetworkPolicy plugin, and skipping the check would void the isolation evidence. |

Evidence: `spike/evidence/q1-occ-dev-up.trimmed.log` (build layer noise removed).

## Not reached
Namespace ready, Agent create/deploy, Harness model probe, runtime readiness, bounded model task.
Additionally **no model credential is available** (`OPENAI_API_KEY` / `OPENAI_API_KEY_FILE` unset), so even on a
capable host the model probe and a real turn would be NOT RUN until one is supplied.

## What is needed to close Q1
1. A host whose kernel has `ip_set` (+ `xt_set`) and ideally `vxlan` — e.g. a normal VM/bare-metal Linux with Docker,
   or an existing Kubernetes cluster with an enforcing CNI (Calico/Cilium). The current box is a minimal guest kernel without loadable modules.
2. `OPENAI_API_KEY_FILE` pointing to a private file with an OpenAI API key able to use the chosen Codex model
   (`SPIKE_MODEL`, default `gpt-6-astra` as in OCE docs). Dedicated Codex receives it as `OPENAI_API_KEY` via an OCC Secret.

## Cost / operator time
~1 h to reach step 9 on this box, 5 local workarounds; ~20 GB container storage; first build ~10 min.
