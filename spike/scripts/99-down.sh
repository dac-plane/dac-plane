#!/usr/bin/env bash
here=$(cd "$(dirname "$0")/.." && pwd)
cd "${OCE_DIR:-/workspace/oce}" && eval "$(mise env -s bash)" && export PATH="$here/shim:$PATH"
OCC_DEVELOPMENT_COMPUTE_DRIVER=kubernetes OCC_DEVELOPMENT_CONTROL_PLANE=kubernetes ./bin/occ dev down || k3d cluster list
