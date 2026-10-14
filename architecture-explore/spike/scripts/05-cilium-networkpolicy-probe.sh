#!/usr/bin/env bash
# Feasibility probe (disposable cluster, separate from occ dev up): does Cilium give enforcing NetworkPolicy
# on a kernel without ip_set? Cilium iptables masquerade needs ipset, so masquerade is done by one manual
# iptables rule in the node; kube-proxy stays (kubeProxyReplacement=true broke ClusterIP services here).
set -uo pipefail
export PATH=$HOME/.local/bin:$PATH; cd /workspace/oce && eval "$(mise env -s bash)"
C=${PROBE_CLUSTER:-cprobe}
k3d cluster delete $C >/dev/null 2>&1
k3d cluster create $C --servers 1 --agents 0 --no-lb --wait \
  --k3s-arg "--flannel-backend=none@server:*" --k3s-arg "--disable-network-policy@server:*" \
  --k3s-arg "--disable=traefik@server:*" --k3s-arg "--disable=metrics-server@server:*" || { echo "RESULT: cluster create failed"; exit 1; }
N=k3d-$C-server-0
docker exec $N sh -c 'mount | grep -q /sys/fs/bpf || mount bpffs /sys/fs/bpf -t bpf; mount --make-shared /sys/fs/bpf; mkdir -p /run/cilium/cgroupv2; mount | grep -q /run/cilium/cgroupv2 || mount -t cgroup2 none /run/cilium/cgroupv2; mount --make-shared /run/cilium/cgroupv2'
docker exec $N sh -c 'iptables -t nat -A POSTROUTING -s 10.42.0.0/16 ! -d 10.42.0.0/16 -j MASQUERADE' || echo 'manual masq rule failed'
helm repo add cilium https://helm.cilium.io >/dev/null 2>&1; helm repo update cilium >/dev/null
helm install cilium cilium/cilium --version 1.16.5 -n kube-system \
  --set routingMode=native --set autoDirectNodeRoutes=true --set ipv4NativeRoutingCIDR=10.42.0.0/16 \
  --set ipam.mode=kubernetes --set kubeProxyReplacement=false --set enableIPv4Masquerade=false --set ipv4.enabled=true --set ipv6.enabled=false --set operator.replicas=1 \
  --set bpf.autoMount.enabled=false --set cgroup.autoMount.enabled=false --set cgroup.hostRoot=/run/cilium/cgroupv2 \
  --set hubble.enabled=false || { echo "RESULT: helm install failed"; exit 1; }
kubectl -n kube-system rollout status ds/cilium --timeout=600s || { kubectl -n kube-system get pods -o wide; kubectl -n kube-system logs ds/cilium --tail=60; echo "RESULT: cilium not ready"; exit 1; }
kubectl create ns np
kubectl -n np run target --image=nginx:alpine --labels app=target --port 80
kubectl -n np expose pod target --port 80
kubectl -n np wait pod/target --for=condition=Ready --timeout=300s
cat <<Y | kubectl apply -f -
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: only-allowed, namespace: np}
spec:
  podSelector: {matchLabels: {app: target}}
  policyTypes: [Ingress]
  ingress: [{from: [{podSelector: {matchLabels: {role: allowed}}}]}]
Y
sleep 10
probe(){ kubectl -n np run "p-$1" --image=busybox:1.36 --labels role=$1 --restart=Never --rm -i --quiet -- \
  sh -c 'wget -q -T 5 -O /dev/null http://target && echo REACHED || echo BLOCKED' 2>&1 | tail -1; }
A=$(probe allowed); W=$(probe wronglabel)
E=$(kubectl -n np run p-egress --image=curlimages/curl:8.10.1 --restart=Never --rm -i --quiet -- curl -s -o /dev/null -w "%{http_code}" -m 10 https://example.com 2>&1 | tail -1); echo "egress probe (HTTP status): $E"
echo "allowed-label probe: $A"; echo "wrong-label probe: $W"
if [ "$A" = REACHED ] && [ "$W" = BLOCKED ]; then echo "RESULT: PASS (NetworkPolicy enforced by Cilium)"; echo "Clean up: k3d cluster delete $C"; else echo "RESULT: FAIL"; fi
