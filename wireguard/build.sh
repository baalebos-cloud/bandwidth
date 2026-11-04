#!/usr/bin/env bash
# Reproducible build of wireguard-go (userspace WireGuard — no kernel module
# or special "wireguard" netlink link type required, just a generic TUN
# device). This is the exact recipe used to build the binary checked into
# this directory; re-run it if you need to rebuild for a different
# architecture or pull a newer wireguard-go release.
#
# Requires Go >= 1.23 (the upstream go.mod requires this even though the
# rest of this project targets 1.22 — install golang-1.24-go alongside your
# existing Go if needed; `go` at your normal version is untouched).
set -euo pipefail

WORKDIR="${1:-/tmp/wireguard-go-build}"
GO_BIN="${GO_BIN:-$(command -v go)}"

rm -rf "$WORKDIR"
git clone --depth 1 https://github.com/WireGuard/wireguard-go.git "$WORKDIR"
cd "$WORKDIR"

# wintun is a Windows-only TUN driver binding — irrelevant on Linux, and
# not fetchable in network environments that don't allowlist
# golang.zx2c4.com, so drop it rather than work around it.
"$GO_BIN" mod edit -droprequire=golang.zx2c4.com/wintun

# golang.org/x/* packages aren't reachable in network environments that
# only allowlist github.com — redirect to the official GitHub mirrors,
# which serve identical content.
cat >> go.mod << 'EOF'

replace golang.org/x/sys => github.com/golang/sys v0.32.0
replace golang.org/x/net => github.com/golang/net v0.39.0
replace golang.org/x/crypto => github.com/golang/crypto v0.37.0
replace golang.org/x/time => github.com/golang/time v0.7.0
EOF

# Build only the main package (the userspace WireGuard daemon). The
# repo also contains tun/netstack, an alternate in-process TCP/IP stack
# mode that pulls in gvisor.dev/gvisor — not needed for this project (we
# use plain TUN mode) and not fetchable in restricted network
# environments, so we deliberately never touch that package.
export GOPROXY=direct GOSUMDB=off GOFLAGS=-mod=mod
"$GO_BIN" build -o wireguard-go-bin .

echo "Built: $WORKDIR/wireguard-go-bin"
