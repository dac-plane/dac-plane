# Mac run progress (Bryan's Mac, arm64, OrbStack Docker 29.8.2, kernel without ip_set)

- Toolchain: brew `mise` (logged in .work/mac-installs.log); OCE @07590c6 tools via mise under .work/.
- `spike/shim-mac/k3d`: on `cluster create` adds `--flannel-backend=none --disable-network-policy`, mounts bpffs/cgroup2, adds one manual MASQUERADE rule, installs Cilium 1.16.5 (as PR #2 probe) before returning to `occ dev up`.
- `spike/scripts/06-mac-occ-up.sh`: runs `occ dev up` with that shim (K3D_FIX_DNS=0: k3d's DNS fix points nodes at the OrbStack gateway, which refuses DNS; image pulls failed).
- `02-deploy-codex-agent.mjs`: SPIKE_VARIANT=codex-custom|embedded-custom, SPIKE_BASE_URL, MODEL_KEY_JSON=<file>#<path> (key read in-process into the OCC Secret API, never printed).
- Status: occ dev up attempt 4 in progress (09:35 WAT).
