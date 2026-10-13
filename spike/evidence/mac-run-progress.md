# Mac run progress (Bryan's Mac, arm64, OrbStack Docker 29.8.2, kernel without ip_set)

- Toolchain: brew `mise` (logged in .work/mac-installs.log); OCE @07590c6 tools via mise under .work/.
- `spike/shim-mac/k3d`: on `cluster create` adds `--flannel-backend=none --disable-network-policy`, mounts bpffs/cgroup2, adds one manual MASQUERADE rule, installs Cilium 1.16.5 (as PR #2 probe) before returning to `occ dev up`.
- `spike/scripts/06-mac-occ-up.sh`: runs `occ dev up` with that shim (K3D_FIX_DNS=0: k3d's DNS fix points nodes at the OrbStack gateway, which refuses DNS; image pulls failed).
- `02-deploy-codex-agent.mjs`: SPIKE_VARIANT=codex-custom|embedded-custom, SPIKE_BASE_URL, MODEL_KEY_JSON=<file>#<path> (key read in-process into the OCC Secret API, never printed).
- Status: occ dev up attempt 4 in progress (09:35 WAT).
- 10:10 WAT attempt 5 result: Cilium 1.16.5 enforced namespace/label policy (wrong-ns peer blocked, control reachable) but OCC's
  probe also applies an *egress* policy with `ipBlock` = target/control pod IPs; Cilium by default does not match pod IPs with
  ipBlock, so the allowed peer was blocked too and `occ dev up` failed "NetworkPolicy enforcement could not be verified".
  Cilium 1.18 rejects `policyCIDRMatchMode=pods` ("unknown CIDR match mode"). Attempt 6: Cilium 1.20.2 with
  `policyCIDRMatchMode={pods,nodes}`.
- 10:16 WAT attempt 6 (Cilium 1.20.2, policyCIDRMatchMode={pods,nodes}): **OCC network isolation check PASSED** (allowed peer
  reached target, wrong-label/wrong-ns denied). Next failure: "wait for the k3d Pod bridge route used by the API server Pod proxy"
  (OCC expects flannel's `dev cni0` for `ip route get <podCIDR+2>`). Attempt 7: shim adds a dummy `cni0` with a /32 route for
  podCIDR+2 sourced from the cilium_host address (the real Pod-proxy source under Cilium). Spike-only workaround.
- 03-slice.mjs written (native gateway session/turn/approval scenarios; mapping persisted to spike/out/slice-mapping.json).
- 13:27 WAT: in-cluster OpenCodex proxy path is configured (`opencodex` Service on port 10100, Secret-backed Camer key and
  admission token). Manual pod-to-proxy probes succeeded earlier, proving labeled OCC-style pods can call the proxy through
  NetworkPolicy. Dedicated Codex Agent deployment still fails at `codex.model_probe` with `RUNTIME_AUTHENTICATION_FAILED`.
  Latest bounded run `spike/out/q1-spike-codex-incluster3.json` retried three revisions of the same Agent and all failed.
  Agent pod logs show workspace setup, Codex login, node setup and gateway connection succeed before the single Codex model
  probe exits 1. OpenCodex logs from the same run family show Camer upstream failures (`502`, then `429`) for
  `camer-digital/glm-5.3-flash`; do not spend more retries until upstream capacity/key behavior is addressed or the probe
  path is made to report 429/5xx as transient instead of authentication failure.
- 14:00 WAT: corrected root-cause after in-pod repro. The Camer/OpenCodex path is not the decisive blocker: direct
  OpenCodex checks for `camer-digital/glm-5.3-flash` and `camer-digital/deepseek-v4-flash-0731` returned `200 OK` for
  `/v1/chat/completions` and `/v1/responses`. The deployed Agent still failed because OCE's dedicated Codex startup probe
  runs Codex with `--ignore-user-config` and no `openai_base_url`, so Codex defaults to `api.openai.com`/`chatgpt.com` and
  rejects the OpenCodex admission token as an invalid OpenAI credential. Running the same probe manually inside the failed
  Agent pod with `-c openai_base_url=http://opencodex:10100/v1` returned `READY`. Clean rerun
  `spike/out/q1-spike-codex-incluster5.json` failed again with `RUNTIME_AUTHENTICATION_FAILED`.
- 14:20 WAT: attempted a local OCE controller patch to pass `openai_base_url` into the generated Codex probe. The patch
  requires rolling a custom OCE controller image (`openclaw-enterprise-controller:probe-base-url`) and is therefore evidence
  that adoption would require carrying an OCE fork/patch, not evidence that stock OCE qualifies. Stop the qualification here:
  **Q1 FAIL / do not adopt stock OCE for this project's runtime path.**
