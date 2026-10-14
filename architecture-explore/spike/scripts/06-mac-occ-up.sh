R=~/dev/delegated-agent-control-plane; W=$R/.work
export MISE_DATA_DIR=$W/mise-data MISE_CACHE_DIR=$W/mise-cache MISE_CONFIG_DIR=$W/mise-config MISE_STATE_DIR=$W/mise-state HELM_CACHE_HOME=$W/helm/cache HELM_CONFIG_HOME=$W/helm/config HELM_DATA_HOME=$W/helm/data
export KUBECONFIG=$W/kubeconfig; cd $W/oce && mise trust -q . && eval "$(mise env -s bash)"; export PATH=$W/shim-mac:$PATH
export K3D_FIX_DNS=0 OCC_DEVELOPMENT_COMPUTE_DRIVER=kubernetes OCC_DEVELOPMENT_CONTROL_PLANE=kubernetes OCC_DEVELOPMENT_SANDBOX_DRIVER=none
date; ./bin/occ dev down; ./bin/occ dev up; echo "EXIT $?"; date
