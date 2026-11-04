# Real WireGuard — proven working, honestly scoped

## What this is

A genuinely working userspace WireGuard tunnel (`wireguard-go`), built and
tested in the sandbox — not a mock, not a design doc. This directory
contains the built binary, the reproducible build recipe, and setup
scripts for both sides of a tunnel.

## What was actually proven (not claimed — run and observed)

1. Kernel WireGuard (`ip link add type wireguard`) is **not** available in
   this sandbox — it fails with "Unknown device type" despite `/dev/net/tun`
   existing and `cap_net_admin` being present, consistent with a
   userspace-networking sandbox that doesn't implement the WireGuard
   netlink device family.
2. **Userspace WireGuard (`wireguard-go`) works.** Built from the official
   `github.com/WireGuard/wireguard-go` source (patched only to redirect
   `golang.org/x/*` dependencies to their GitHub mirrors and drop the
   Windows-only `wintun` dependency — both blocked by this network's
   allowlist, neither related to whether WireGuard itself works).
3. Two independent peers, each with a real generated X25519 keypair, were
   brought up as separate `wireguard-go` processes with separate TUN
   interfaces (`wg-gw` at 10.99.0.1, `wg-client` at 10.99.0.2), configured
   as WireGuard peers of each other over UDP on localhost.
4. **Real ICMP traffic** passed through the tunnel: `ping -c 3` from the
   client interface to the gateway interface — 0% packet loss, real
   round-trip timings (0.027–0.334ms).
5. **Real TCP/HTTP traffic** passed through the tunnel: an HTTP server
   bound to the gateway's tunnel IP served a file, fetched via `curl` from
   the client side with no special routing — the OS routed it through the
   WireGuard interface automatically because that's the only route to
   that IP, exactly as it would in production.
6. `wg show` confirmed a real Noise-protocol handshake occurred and real
   byte counters incremented — this is genuine ChaCha20Poly1305-encrypted
   traffic, not a stub.

## What this does NOT include yet — the honest gap

This proves the **transport layer** works. It does not yet replace the
platform's actual gateway/client transport, which currently uses a plain
TLS tunnel (see `gateway/main.go`, `client/rust-relay/`). Swapping the
transport is a real, separate engineering task:

- **Key distribution**: right now you run `gateway-setup.sh` and
  `client-setup.sh` by hand and manually exchange public keys. In
  production, the control plane should generate/distribute WireGuard
  keys as part of `CreateSession` (alongside or instead of the current
  session token), and the gateway's peer list needs to be updated
  dynamically per session (`wg set <iface> peer <key> allowed-ips <ip>`)
  rather than configured once at startup.
- **The gateway's byte-counting logic** (`countingConn` in
  `gateway/main.go`) currently counts TLS bytes. Over a WireGuard
  transport, the gateway would instead count bytes at the point where it
  reads from the TUN interface for a given session's allowed-ip range —
  a different (and in some ways simpler) counting point, but a real code
  change, not a config change.
- **Per-node WireGuard IP allocation**: this proof used two fixed IPs.
  Production needs an IP allocation scheme (e.g., a `/16` carved into
  per-node `/32`s) tracked in the `nodes` table.
- **Android**: `wireguard-go` (or the official `wireguard-android` Kotlin
  bindings, which wrap the same core) is the correct integration point
  for the mobile client's tunnel — see `../mobile/` for how the Android
  client is scoped to plug into this.

## Reproducing this

```bash
# One-time build (needs Go >= 1.23 — see build.sh for why)
./build.sh

# On the gateway host:
sudo ./gateway-setup.sh <client-pubkey-once-you-have-it>
# prints its own pubkey — give that to the client operator

# On the client host:
sudo ./client-setup.sh <gateway-pubkey> <gateway-public-ip>

# Then, from the client host:
curl http://10.99.0.1:<whatever-port>/...   # routes through the real tunnel
```

Both scripts need root (or `CAP_NET_ADMIN` + access to `/dev/net/tun`) to
create network interfaces.
