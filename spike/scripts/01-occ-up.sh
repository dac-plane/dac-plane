#!/usr/bin/env bash
# Q1: start OCE local Kubernetes-only profile exactly as docs/guides/quickstart.md, plus local shims.
set -euo pipefail
here=$(cd "$(dirname "$0")/.." && pwd)
cd "${OCE_DIR:-/workspace/oce}" && eval "$(mise env -s bash)"
export PATH="$here/shim:$PATH"   # see spike/README.md "Local workarounds"
export OCC_DEVELOPMENT_COMPUTE_DRIVER=kubernetes OCC_DEVELOPMENT_CONTROL_PLANE=kubernetes OCC_DEVELOPMENT_SANDBOX_DRIVER=none
./bin/occ dev up 2>&1 | tee "$here/out/occ-dev-up.log"
