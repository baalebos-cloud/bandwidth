#!/usr/bin/env bash
# Sets up the gateway side of a real WireGuard tunnel using wireguard-go
# (userspace — works without kernel WireGuard support or CAP_SYS_MODULE).
# This is the exact sequence proven working in the build sandbox: real
# X25519 keys, a real Noise-protocol handshake, ChaCha20Poly1305-encrypted
# traffic, carrying both ICMP and real TCP/HTTP application traffic.
#
# Usage: sudo ./gateway-setup.sh <client-pubkey> [listen-port] [tunnel-cidr]
set -euo pipefail

CLIENT_PUBKEY="${1:?Usage: $0 <client-pubkey> [listen-port] [tunnel-ip]}"
LISTEN_PORT="${2:-51820}"
GATEWAY_TUNNEL_IP="${3:-10.99.0.1}"
CLIENT_TUNNEL_IP="${4:-10.99.0.2}"
IFACE="wg-gw"
WG_BIN="$(dirname "$0")/wireguard-go-bin"

if [ ! -f "$WG_BIN" ]; then
  echo "wireguard-go-bin not found next to this script — run build.sh first." >&2
  exit 1
fi

umask 077
if [ ! -f gw_privkey ]; then wg genkey > gw_privkey; fi
wg pubkey < gw_privkey > gw_pubkey

"$WG_BIN" -f "$IFACE" &
sleep 1

wg set "$IFACE" listen-port "$LISTEN_PORT" private-key gw_privkey \
  peer "$CLIENT_PUBKEY" allowed-ips "${CLIENT_TUNNEL_IP}/32"
ip addr add "${GATEWAY_TUNNEL_IP}/24" dev "$IFACE"
ip link set "$IFACE" up

echo "Gateway WireGuard interface '$IFACE' up on ${GATEWAY_TUNNEL_IP}, listening on UDP $LISTEN_PORT."
echo "Gateway public key (give this to the client): $(cat gw_pubkey)"
echo "The actual bandwidth-platform gateway process should bind its TLS"
echo "relay listener to ${GATEWAY_TUNNEL_IP} instead of a public IP once"
echo "this is wired in — see docs note on transport-layer swap."
