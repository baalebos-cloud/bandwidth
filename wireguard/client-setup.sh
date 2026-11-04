#!/usr/bin/env bash
# Sets up the client side of a real WireGuard tunnel, matching
# gateway-setup.sh. See that file's header for what this proves.
#
# Usage: sudo ./client-setup.sh <gateway-pubkey> <gateway-endpoint-ip> [listen-port] [tunnel-cidr]
set -euo pipefail

GATEWAY_PUBKEY="${1:?Usage: $0 <gateway-pubkey> <gateway-endpoint-ip> [listen-port]}"
GATEWAY_ENDPOINT_IP="${2:?Usage: $0 <gateway-pubkey> <gateway-endpoint-ip> [listen-port]}"
LISTEN_PORT="${3:-51820}"
GATEWAY_TUNNEL_IP="${4:-10.99.0.1}"
CLIENT_TUNNEL_IP="${5:-10.99.0.2}"
IFACE="wg-client"
WG_BIN="$(dirname "$0")/wireguard-go-bin"

if [ ! -f "$WG_BIN" ]; then
  echo "wireguard-go-bin not found next to this script — run build.sh first." >&2
  exit 1
fi

umask 077
if [ ! -f client_privkey ]; then wg genkey > client_privkey; fi
wg pubkey < client_privkey > client_pubkey

"$WG_BIN" -f "$IFACE" &
sleep 1

wg set "$IFACE" private-key client_privkey \
  peer "$GATEWAY_PUBKEY" allowed-ips "${GATEWAY_TUNNEL_IP}/32" \
  endpoint "${GATEWAY_ENDPOINT_IP}:${LISTEN_PORT}"
ip addr add "${CLIENT_TUNNEL_IP}/24" dev "$IFACE"
ip link set "$IFACE" up

echo "Client WireGuard interface '$IFACE' up on ${CLIENT_TUNNEL_IP}."
echo "Client public key (give this to the gateway operator): $(cat client_pubkey)"
echo "Once both sides are up: curl http://${GATEWAY_TUNNEL_IP}:<port>/... routes through the encrypted tunnel."
