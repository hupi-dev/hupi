#!/usr/bin/env bash
# Creates (first run) or restarts the Windows 11 test VM: dockur/windows in
# Docker, with KVM. The first run downloads Windows 11 IoT Enterprise LTSC
# (evaluation, ~5-6 GB) and installs it unattended, then oem/setup.ps1
# installs VS Code — allow ~30-60 minutes. See ../README.md.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
VM_DIR="${HUPI_WIN_DIR:-$HOME/hupi-win}"
NAME="${HUPI_WIN_CONTAINER:-hupi-win}"
mkdir -p "$VM_DIR/storage" "$VM_DIR/shared" "$VM_DIR/oem"
cp "$HERE"/oem/* "$VM_DIR/oem/"
if docker ps -a --format '{{.Names}}' | grep -qx "$NAME"; then
  docker start "$NAME"
  exit 0
fi
# Viewer on http://127.0.0.1:8006, RDP on 127.0.0.1:3389 (user Docker, no password).
docker run -d --name "$NAME" \
  -e VERSION=11l -e RAM_SIZE=8G -e CPU_CORES=6 -e DISK_SIZE=64G \
  --device=/dev/kvm --device=/dev/net/tun --cap-add NET_ADMIN \
  -p 127.0.0.1:8006:8006 -p 127.0.0.1:3389:3389/tcp -p 127.0.0.1:3389:3389/udp \
  -v "$VM_DIR/storage:/storage" -v "$VM_DIR/shared:/shared" -v "$VM_DIR/oem:/oem" \
  --stop-timeout 120 dockurr/windows
echo "installing; watch http://127.0.0.1:8006 — done when $VM_DIR/shared/setup-status.txt says 'setup done'"
