# Android client — reference implementation, uncompiled

## Honest status

This is real, carefully-written Kotlin source implementing the client
architecture from the platform blueprint (§1.2, §5.1–5.2) — **not a
production-ready, tested Android app.** The distinction matters:

- **What was actually done**: every `.kt` file was checked with a real
  Kotlin compiler (`kotlinc`, installed in the build sandbox) to catch
  genuine syntax errors, filtering out the expected "unresolved reference"
  noise from Android/AndroidX classes not being on the classpath (no
  Android SDK is installed in this sandbox, and installing one plus
  running a full Gradle build was judged impractical here — multi-GB
  download, and Google's Maven repositories aren't in this sandbox's
  network allowlist). That check found **zero real syntax errors** — the
  handful of flagged lines were all trailing commas in argument lists,
  a Kotlin 1.4+ feature the only offline compiler available (1.3.31,
  from Ubuntu's apt repo) predates. This project targets Kotlin 1.9.24
  (see `app/build.gradle.kts`), which supports that syntax natively.
- **What was NOT done**: this has never been built into an APK, never run
  on a device or emulator, and never actually connected to the real
  control-plane/gateway. Treat it as a strong starting point that needs a
  real Android Studio build-and-run pass — with a real device or
  emulator — before it's trustworthy.

## What's implemented

- `NetworkPolicyMonitor.kt` — Wi-Fi/ethernet-only enforcement as a hard
  `ConnectivityManager.NetworkCallback` interrupt (not polling), including
  the metered-network case (mobile hotspots presenting as Wi-Fi), plus a
  battery/charging check. This is the client-side half of the platform's
  most trust-critical guarantee — see the main README and blueprint for
  why "hard interrupt, not polled" matters here.
- `RelayForegroundService.kt` — the always-running background component:
  starts as a proper foreground service with an honest, non-deceptive
  notification (per Play Store policy expectations for this app
  category), registers with the control plane, requests a session, and
  **immediately tears down the active socket** — not just "stops starting
  new ones" — the moment the network monitor reports ineligibility.
- `ControlPlaneClient.kt` — talks to the exact same control-plane API
  already tested end-to-end against the Go control-plane and the desktop
  Rust client (`POST /v1/nodes/register`, `POST /v1/sessions`).
- `RelaySession.kt` — TLS relay using the exact same wire protocol as the
  desktop client (length-prefixed session_id, length-prefixed token, then
  payload bytes) — protocol-compatible by design rather than inventing a
  parallel mobile-specific protocol.
- `MainActivity.kt` — minimal start/stop UI. Deliberately not
  feature-complete; a real build would add the wallet/earnings view here,
  mirroring `frontend/wallet-dashboard`.

## What's simplified, matching where the rest of this project currently stops

- Like `gateway/main.go` in its current form, `RelaySession.kt` sends a
  synthetic payload rather than bridging real bidirectional enterprise
  traffic — this reference implementation's gateway doesn't yet forward
  to a real destination either (see the comment in `gateway/main.go`
  about the drain-only relay loop). Both sides are consistent with each
  other and with the project's actual current scope.
- Certificate pinning loads a `gateway-cert.pem` from Android assets that
  isn't bundled here — copy the same demo cert the desktop client uses
  (or, correctly for production, drop the custom `X509TrustManager`
  entirely and use a real CA-issued cert with the platform default trust
  store).
- `DEMO_ENTERPRISE_ID` is a placeholder constant — wire this to a real
  KYB-approved enterprise client id (from the admin console) before
  testing against a live control plane.
- No WireGuard integration yet — see `../wireguard/README.md` for what's
  proven there and what remains; the natural integration point on Android
  is the official `wireguard-android` Kotlin bindings (same core as the
  `wireguard-go` proven in this project), not a from-scratch client.

## To actually build and run this

You'll need Android Studio (or the command-line SDK tools) — neither is
available in this build sandbox:

```bash
# From Android Studio: File > Open > select the mobile/ directory
# Or from the command line, once ANDROID_HOME is set:
cd mobile
./gradlew assembleDebug
```

Point `CONTROL_PLANE_URL` and `GATEWAY_HOST` (in
`RelayForegroundService.kt`) at your running backend — `10.0.2.2` is the
Android emulator's alias for the host machine's `localhost`, which is
already set as the default for exactly that reason.
