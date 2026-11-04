# iOS client — reference implementation, unvalidated

## Honest status — read this before anything else

This is real Swift source, written carefully against documented Apple
APIs (`Network.framework`, `NetworkExtension`, `SwiftUI`) — **it has not
been compiled, not even partially.** Unlike the Android client, where a
real Kotlin compiler was available in the build sandbox to at least catch
genuine syntax errors, **no Swift toolchain is available in this Linux
sandbox at all** — Swift's Linux toolchain doesn't include
`NetworkExtension`, `UIKit`, or any of Apple's iOS-only frameworks this
code depends on, and installing even the generic Linux Swift toolchain
wasn't pursued because it couldn't have validated the parts that matter
most here anyway. This code has a **lower confidence level than the
Android client** for that reason, and should be treated as a strong
starting draft for a real Xcode build-and-run pass, not as tested code.

Xcode itself only runs on macOS — there is no way to produce or validate
an actual `.xcodeproj` from a Linux sandbox. The file layout here mirrors
what an Xcode project needs (two targets, their entitlements, their
`Info.plist`s) so that creating the real project is mostly "add these
existing files to a new Xcode project" rather than starting from scratch
— see "Assembling this into a real Xcode project" below.

## Why this isn't just a background app — the real constraint

The platform blueprint originally deferred iOS with a one-line reason:
*"no persistent background sockets, aggressive suspension, App Store
policy explicitly forbids proxy/VPN-selling apps that aren't a declared
Network Extension."* This project takes that constraint at face value
rather than working around it:

A standard iOS app has no background execution mode that fits "keep an
arbitrary outbound TLS socket open indefinitely and relay bytes for a
third party" — iOS suspends ordinary apps within seconds of
backgrounding. The one mechanism Apple provides that grants a process
extended, persistent background execution tied to a live network
connection is a **Network Extension activated as a VPN configuration**
(`NETunnelProviderManager` / `NEPacketTunnelProvider`) — which is what
`BandwidthPlatformTunnel/PacketTunnelProvider.swift` implements.

This has a real, disclosed honesty implication: activating sharing shows
the standard iOS VPN indicator in the status bar and in Settings ▸ VPN,
even though this app isn't actually tunneling the user's own traffic
anywhere (`startTunnel` deliberately configures non-routing tunnel
settings — see that file's header comment for the full reasoning). Per
the platform blueprint's own informed-consent principle, **this must be
disclosed to the user, not hidden** — `ContentView.swift` says so
explicitly in its UI copy, on purpose, rather than only in a code
comment.

## What's implemented

- `Shared/NetworkPolicyMonitor.swift` — Wi-Fi-only enforcement via
  `NWPathMonitor`, including the Personal-Hotspot-presents-as-Wi-Fi case
  (`path.isExpensive`/`isConstrained`), plus a battery/charging check via
  `UIDevice` — the same hard-interrupt design as the Android
  implementation, an OS push callback rather than polling.
- `Shared/ControlPlaneClient.swift` — talks to the exact same
  control-plane API already tested end-to-end against the Go
  control-plane, the Rust desktop client, and the Android client.
- `Shared/RelaySession.swift` — the same wire protocol as the desktop and
  Android clients (length-prefixed session_id, length-prefixed token,
  then payload bytes), built on `NWConnection` rather than `URLSession`
  since this is a raw TLS byte stream, not a series of HTTP calls.
- `Shared/RelayCoordinator.swift` — a Swift `actor` tying the above
  together, mirroring `RelayForegroundService.kt`'s orchestration logic
  (register → request session → relay → hard-teardown-on-ineligibility).
- `BandwidthPlatformTunnel/PacketTunnelProvider.swift` — the
  `NEPacketTunnelProvider` subclass that actually runs `RelayCoordinator`
  in the background.
- `BandwidthPlatformApp/` — the thin main-app target: `TunnelManager.swift`
  installs/toggles the VPN configuration, `ContentView.swift` is the
  minimal UI (with the required consent copy).

## What's simplified, matching where the rest of this project currently stops

Same two caveats as the Android client, for the same reasons (see
`../mobile/README.md`):
- `RelaySession.swift` sends a synthetic payload rather than bridging
  real bidirectional enterprise traffic, matching the gateway's own
  current drain-only relay loop.
- TLS verification in `RelaySession.swift` accepts the demo cert
  unconditionally (`complete(true)` in the verify block) — fine for
  local testing against `gen-cert.sh`'s throwaway cert, wrong for
  production, which should delete that verify block entirely and use a
  real CA-issued certificate with the system trust store.
- `enterpriseClientId` is an empty placeholder in `ContentView.swift` —
  wire it to a real KYB-approved client id from the admin console before
  testing against a live control plane.

## Assembling this into a real Xcode project

From a Mac, with Xcode installed:

1. **File ▸ New ▸ Project ▸ iOS ▸ App**, name it `BandwidthPlatform`,
   interface: SwiftUI. This generates the base app target.
2. **File ▸ New ▸ Target ▸ Network Extension**, template "Packet Tunnel",
   name it `BandwidthPlatformTunnel`. Xcode wires up the extension
   embedding automatically.
3. Delete Xcode's generated placeholder Swift files for both targets and
   drag in the files from this directory instead — `Shared/` should be
   added to **both** targets' membership (Xcode's file inspector has a
   target-membership checklist per file); the rest go to their
   already-implied target.
4. Replace the auto-generated `Info.plist` and `.entitlements` files with
   the ones in this directory (or merge the `NSExtension`/entitlement
   keys in if Xcode's versions have other keys you want to keep).
5. In **Signing & Capabilities** for both targets, add the "Network
   Extensions" capability (Xcode manages the entitlement file for you
   once you do this via the UI, which will overwrite/merge with the
   `.entitlements` files here — check the diff rather than assuming
   Xcode's automatic version is a strict superset).
6. Build. This is the point where real bugs neither the Kotlin-style
   syntax check nor a careful read-through can catch will surface —
   `NetworkExtension` and `Network.framework` have plenty of API surface
   this was written against from documentation and training knowledge,
   not from a compiler.
