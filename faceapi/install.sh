#!/usr/bin/env bash
# install.sh — idempotent installer for the connect-face-api sidecar.
# Copies server.py + requirements into /opt/faceapi, builds a venv, installs
# deps, and registers/starts the systemd unit. Safe to re-run for upgrades.
set -euo pipefail

DEST=/opt/faceapi
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [ "$(id -u)" -ne 0 ]; then
    echo "install.sh must run as root (systemd + /opt install)" >&2
    exit 1
fi

# Build + runtime prerequisites. insightface compiles C++/Cython extensions at
# pip-install time, so gcc/g++/python3-dev are required; opencv needs libGL +
# libglib; python3-venv is not always present on minimal images.
if command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y --no-install-recommends \
        python3 python3-venv python3-dev \
        build-essential gcc g++ \
        libgl1 libglib2.0-0
else
    echo "WARNING: apt-get not found — ensure python3-dev, a C/C++ toolchain, libGL and libglib are installed" >&2
fi

mkdir -p "$DEST"
cp "$SRC/server.py" "$DEST/server.py"
cp "$SRC/requirements.txt" "$DEST/requirements.txt"

if [ ! -x "$DEST/venv/bin/python" ]; then
    python3 -m venv "$DEST/venv"
fi
"$DEST/venv/bin/pip" install --upgrade pip wheel setuptools >/dev/null
# Cython must be present before insightface's sdist build; install it first.
"$DEST/venv/bin/pip" install "cython==3.0.10"
"$DEST/venv/bin/pip" install -r "$DEST/requirements.txt"

cp "$SRC/connect-face-api.service" /etc/systemd/system/connect-face-api.service
systemctl daemon-reload
systemctl enable --now connect-face-api
systemctl restart connect-face-api

echo "connect-face-api installed. Model (~330MB) downloads on first start; watch:"
echo "  journalctl -u connect-face-api -f"
echo "  curl -s http://127.0.0.1:8799/health"
