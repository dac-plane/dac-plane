#!/usr/bin/env bash
# Q1 prerequisites on a Linux box. Pins match OCE mise.toml at the pinned commit.
set -euo pipefail
OCE_REPO=https://github.com/openclaw/openclaw-enterprise
OCE_SHA=07590c64e27ac61c9ad60d9ab61424b70281011a
OCE_DIR=${OCE_DIR:-/workspace/oce}

# Docker Engine >= 27 is required ('occ dev up' uses 'docker image save --platform'; 26.x fails).
if ! docker version --format '{{.Server.Version}}' >/dev/null 2>&1; then
  curl -fsSL https://download.docker.com/linux/static/stable/x86_64/docker-28.5.1.tgz | tar xz -C /tmp
  sudo cp /tmp/docker/* /usr/local/bin/ && (sudo dockerd >/tmp/dockerd.log 2>&1 &) && sleep 8
  sudo chmod 666 /var/run/docker.sock
fi
# This box had a pre-existing iptables-legacy FORWARD policy DROP that blocked bridged container
# traffic (k3d LB -> API server, container egress). Narrow allow rules for docker bridges only:
sudo iptables-legacy -C FORWARD -i br-+ -j ACCEPT 2>/dev/null || sudo iptables-legacy -I FORWARD 1 -i br-+ -j ACCEPT
sudo iptables-legacy -C FORWARD -o br-+ -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null \
  || sudo iptables-legacy -I FORWARD 1 -o br-+ -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT

[ -d "$OCE_DIR/.git" ] || git clone "$OCE_REPO" "$OCE_DIR"
git -C "$OCE_DIR" fetch -q origin && git -C "$OCE_DIR" checkout -q "$OCE_SHA"
command -v mise >/dev/null || { curl -fsSL https://mise.run | sh; export PATH=$HOME/.local/bin:$PATH; }
cd "$OCE_DIR" && mise trust -q && mise install
eval "$(mise env -s bash)" && pnpm install --frozen-lockfile && pnpm cli:build

# Kernel feature check (OCC requires NetworkPolicy enforcement; k3s flannel default needs vxlan).
echo "kernel: $(uname -r)"
[ -e /proc/net/ip_set ] && echo "ip_set: present" || echo "ip_set: MISSING (k3s NetworkPolicy controller cannot start)"
ip link add dacpvx type vxlan id 42 dev lo dstport 0 2>/dev/null && { ip link del dacpvx; echo "vxlan: present"; } || echo "vxlan: MISSING (shim switches flannel to host-gw)"
