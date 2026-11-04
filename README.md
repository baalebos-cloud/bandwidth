# Bandwidth Monetization Platform — Reference Implementation

A working reference implementation of the platform blueprint (see
`docs/`): **gateway-authoritative metering, KYB-gated enterprise access,
an append-only ledger backed by real Postgres, an idempotent payout
orchestrator with a real VTU adapter, ML-based fraud scoring, a proven
real WireGuard tunnel, and an Android client.** This is closer to a full
system than a single MVP-loop proof now, but read the "Honest status per
component" section below before treating any one piece as production-
ready — some are fully tested end-to-end, some are proven in isolation,
and one (mobile) is uncompiled.

Every backend component was compiled and run in the build sandbox, most
against a real Postgres database, not in-memory state. The bugs listed
below were found by actually running the system, not by re-reading the
design.

## What's in here

```
bandwidth-platform/
  control-plane/            Go — node registry, KYB gate, session issuance, Postgres-backed ledger
  gateway/                   Go — TLS relay, authoritative byte counter
  client/rust-relay/         Rust — reference client (registers, relays a known payload)
  payout-engine/             Node.js — idempotent payout orchestrator + real VTpass adapter
  fraud-service/             Python/Flask — trained scikit-learn fraud-scoring model + API
  wireguard/                 A real, proven userspace WireGuard tunnel (built + tested here)
  mobile/                    Android/Kotlin client — real source, uncompiled (see mobile/README.md)
  ios/                        iOS/Swift client — real source, unvalidated, NetworkExtension-based (see ios/BandwidthPlatform/README.md)
  frontend/admin-console/     Internal ops UI — KYB queue, node trust, sessions, fraud scores, payouts
  frontend/wallet-dashboard/  Consumer-facing earnings + redemption UI
  frontend/enterprise-portal/ B2B developer console — signup, KYB status, API key, quickstart
  db/schema.sql               The Postgres schema actually running behind control-plane
  docs/                       The architecture blueprint and phased roadmap
  scripts/gen-cert.sh         Generates a throwaway local TLS cert for the demo
  .env.example                Config for Postgres, the fraud service, and real VTpass credentials
```

## Latest full test run (this session)

A complete pass across everything, run fresh in the build sandbox rather
than assumed from prior sessions:

| Suite | Result |
|---|---|
| Backend (`scripts/backend-test.sh`) — 30 assertions | **30/30 passed** |
| Frontend real-browser tests (`frontend/tests/ui-test.js`) — 20 assertions | **20/20 passed**, zero console/page errors |
| WireGuard standalone tunnel | Fresh keys, fresh interfaces, real encrypted ICMP traffic, 0% packet loss |
| VTpass adapter | Fails cleanly (not silently) with no credentials configured; rejects an unsupported network before attempting any network call |
| Fraud model | Retrains reproducibly (fixed seed) to the same 0.9991 ROC-AUC |
| Android Kotlin | Re-verified clean with a real compiler — zero genuine syntax errors |

One real process lesson from this run, not a code bug: fraud-service
intermittently failed to bind when several services were backgrounded in
rapid succession in the same shell command — confirmed by running it
alone immediately after, where it started cleanly every time. The fix
was a pre-flight reachability check (real HTTP calls, not "I started it a
moment ago") before trusting any service is actually up — see
`scripts/backend-test-README.md` for the full note. Genuinely
intermittent sandbox behavior, not a bug in the service itself.

## Honest status per component

| Component | Status |
|---|---|
| control-plane, gateway, rust-relay | Compiled, run, and verified end-to-end repeatedly, now against real Postgres |
| Persistent DB (Postgres) | **Real.** Schema loaded, control-plane rewritten against `database/sql` + `lib/pq`. Proven durable: data survived across separate tool-call/process boundaries when the app processes didn't |
| ML fraud scoring | **Real.** A `RandomForestClassifier` trained on synthetic-but-structured data (0.999 ROC-AUC on a held-out split), served via Flask, wired into `FinalizeSession`, verified to correctly separate honest sessions (score 0.001) from fraud-shaped ones (score 0.994) it wasn't explicitly trained on |
| VTU wiring (VTpass adapter) | **Code-complete against the real, documented API contract** (real endpoint paths, real `request_id` format rules, real header shape). **Not tested against the live API** — no credentials available in this sandbox. Verified to fail cleanly without credentials rather than silently no-op |
| WireGuard | **Proven working** as a standalone tunnel — real keys, real handshake, real encrypted ICMP and TCP/HTTP traffic, all actually run in this sandbox. **Not yet wired into the gateway/client transport** — see `wireguard/README.md` for exactly what remains |
| Mobile — Android (Kotlin) | Real source, syntax-checked with an actual Kotlin compiler (zero genuine errors found — see `mobile/README.md`). **Never compiled into an APK or run** — no Android SDK in this sandbox |
| Mobile — iOS (Swift) | Real source against documented Apple APIs, using the architecturally-correct `NetworkExtension`/`NEPacketTunnelProvider` mechanism (a normal background app cannot do what this needs on iOS — see `ios/BandwidthPlatform/README.md`). **Not even syntax-checked** — no Swift/Apple-framework toolchain available in this Linux sandbox at all, a strictly lower confidence level than Android |
| Frontends | **Genuinely tested now, not just built.** All three real login flows were driven end-to-end in an actual headless Chromium browser (Playwright) against the live backend — not simulated via curl. This caught and led to fixing two real frontend bugs a curl-based check structurally could not have found — see `frontend/tests/README.md` |
| Authentication | **Real.** OTP-based user login, enterprise API-key auth, admin-key auth, and internal service-to-service auth — all implemented, wired into every relevant endpoint, and verified end-to-end including the negative cases (wrong key, cross-user access, pre-approval session attempts) — see "Authentication" below |

## The core guarantee this demonstrates

**No enterprise client gets a session, and no node gets paid, without
passing through explicit gates — and the number that gets paid is always
the gateway's own count, never the client's claim.**

Concretely, in `control-plane/store.go` (now Postgres-backed):
- `CreateSession()` refuses to issue a session token unless the enterprise
  client's `kyb_status == 'approved'` and the node isn't flagged as a
  datacenter ASN or banned — enforced by a real SQL query against
  persisted state, not an in-memory map that resets on restart.
- `FinalizeSession()` is the *only* place bytes become ledger credit,
  using the gateway's own measured byte count, and now also calls the
  real fraud-scoring service and logs the result to `fraud_score_log`
  before crediting — a scoring-service outage never blocks the credit,
  it just means the session goes out unscored (visible in the admin
  console as "unscored").

## Authentication

Every endpoint that needs to know "who is asking" now actually checks,
rather than trusting a caller-supplied field. Four separate auth
mechanisms, each matched to what's actually asking:

| Who | Mechanism | Enforced on |
|---|---|---|
| End users (people sharing bandwidth) | Phone-based OTP login → signed token (`control-plane/auth.go`) | Node registration, wallet balance, ledger history, payout requests |
| Enterprise clients (bandwidth buyers) | Their own API key, issued once at registration | Session creation — the enterprise id is *resolved from the key*, never trusted from a request body |
| Platform operators | `ADMIN_API_KEY` | KYB approve/reject, node/session listing, unfiltered payout listing |
| Backend services (gateway, payout-engine) | Shared `INTERNAL_SERVICE_KEY` | Session validate/finalize, ledger reserve/reverse |

The user token is a hand-rolled but standard-shaped HS256 JWT
(`header.payload.signature`, HMAC-SHA256, base64url) — verified
independently by both the control plane (Go) and the payout engine
(Node.js) against a shared `AUTH_SECRET`, so the payout engine doesn't
need a network round trip just to know who's asking. This cross-language
interoperability was actually tested, not assumed: a token issued by the
Go service was verified successfully by the Node service in the same
test run.

**What was verified, end-to-end, against the live stack** (not just unit
logic in isolation):
- An admin endpoint rejects requests with no key (401) and accepts them
  with the right one.
- A session-creation request is rejected before it ever reaches the KYB
  check if the API key itself doesn't resolve to a real enterprise client
  (401), and separately rejected *after* key resolution if that
  enterprise isn't yet approved (403) — two different failure reasons,
  correctly distinguished.
- An OTP code is single-use: replaying the same correct code a second
  time is rejected.
- A wrong OTP code is rejected, and doesn't leak whether the phone number
  itself is registered.
- A user can view their own wallet (200) but is blocked (403) from
  viewing another user's wallet by number-guessing the URL.
- A payout request derives *whose* balance to debit from the verified
  token, not from a `userId` field in the request body — so a caller can
  no longer trigger a payout against someone else's balance just by
  naming their id.
- The full Rust reference client now performs its own real login (OTP →
  token → node registration → enterprise-keyed session request) rather
  than being handed pre-authenticated identity — this is the same code
  path a real client would use.
- All three frontends' actual API call sequences were simulated against
  the live, auth-enforced backend (signup → whoami → approve → whoami →
  sessions/mine for the enterprise portal; OTP login → wallet → node
  registration for the wallet dashboard; admin-key verification →
  listings for the admin console) — not just the backend in isolation.

**Reference-build-only bypass, disclosed rather than hidden**: no SMS
provider is wired up (same honest gap as the untested VTpass credentials
elsewhere in this project), so `POST /v1/auth/request-otp` returns the
code directly in its response instead of sending it by SMS
(`code_for_demo_only` field). Every frontend surfaces this as an
on-screen note rather than silently depending on it. **A production build
removes this field from the response entirely** and sends the code via a
real SMS API.

**Demo secrets, disclosed rather than hidden**: `ADMIN_API_KEY`,
`INTERNAL_SERVICE_KEY`, and `AUTH_SECRET` all fall back to hardcoded
`DEMO-ONLY-INSECURE-...` values with a loud startup warning if not set —
convenient for local testing, and impossible to mistake for a secure
default. Real values need to be generated and set via environment
variables (see `.env.example`) before this runs anywhere but localhost.


## Running it

Requires Go 1.22+ (plus Go 1.23+ only if rebuilding `wireguard-go`), Rust/
Cargo, Node.js, Python 3 with `flask`/`scikit-learn`/`joblib` installed,
and PostgreSQL. **Everything must run in one continuous shell session** —
background processes here don't persist once a session ends (Postgres
itself is the one exception — it runs as a real system service and
survives). Build steps and service starts need to happen together.

```bash
# 1. Postgres
service postgresql start
sudo -u postgres psql -c "ALTER USER postgres PASSWORD 'demo_pw_local_only';"
sudo -u postgres createdb bandwidth_platform
sudo -u postgres psql -d bandwidth_platform -c "CREATE EXTENSION IF NOT EXISTS pgcrypto;"
sudo -u postgres psql -d bandwidth_platform -f db/schema.sql

# 2. Generate a local demo cert
./scripts/gen-cert.sh

# 3. Build the backend services and client
(cd control-plane && go build -o control-plane .)
(cd gateway && go build -o gateway .)
(cd client/rust-relay && cargo build --release)

# 4. Set the auth secrets (all fall back to loud, obviously-fake demo
#    defaults if unset — fine for local testing, never for anything else)
export ADMIN_API_KEY="pick-a-real-random-value"
export INTERNAL_SERVICE_KEY="pick-a-different-real-random-value"
export AUTH_SECRET="pick-a-third-real-random-value"

# 5. Start every backend service (control-plane reads DATABASE_URL, or
#    falls back to the same credentials used above — see .env.example).
#    The gateway and payout-engine need INTERNAL_SERVICE_KEY to
#    authenticate to the control plane; payout-engine also needs
#    AUTH_SECRET (to verify user tokens) and ADMIN_API_KEY (to gate its
#    own unfiltered payout listing).
(cd fraud-service && [ -d venv ] || (python3 -m venv venv && venv/bin/pip install --quiet flask scikit-learn joblib numpy pandas))
(cd fraud-service && ./venv/bin/python3 app.py > /tmp/fraud.log 2>&1 &)
(cd control-plane && ADMIN_API_KEY="$ADMIN_API_KEY" INTERNAL_SERVICE_KEY="$INTERNAL_SERVICE_KEY" AUTH_SECRET="$AUTH_SECRET" ./control-plane > /tmp/cp.log 2>&1 &)
(cd gateway && INTERNAL_SERVICE_KEY="$INTERNAL_SERVICE_KEY" ./gateway > /tmp/gw.log 2>&1 &)
(cd payout-engine && INTERNAL_SERVICE_KEY="$INTERNAL_SERVICE_KEY" AUTH_SECRET="$AUTH_SECRET" ADMIN_API_KEY="$ADMIN_API_KEY" node server.js > /tmp/pe.log 2>&1 &)
sleep 2

# 6. Start the three frontends (each on its own port)
(cd frontend/admin-console && python3 -m http.server 8090 &)
(cd frontend/wallet-dashboard && python3 -m http.server 8091 &)
(cd frontend/enterprise-portal && python3 -m http.server 8092 &)

# 7. Open them:
#    Admin console      → http://localhost:8090  (sign in with ADMIN_API_KEY)
#    Wallet dashboard    → http://localhost:8091  (sign in with a phone number)
#    Enterprise portal   → http://localhost:8092  (sign up, or sign in with an API key)
```

Generate real data through the actual auth flow rather than pre-trusted
IDs:

```bash
# Register as an enterprise (self-serve, no auth needed to register itself)
ENT_RESP=$(curl -s -X POST localhost:8080/v1/enterprise/register \
  -d '{"CompanyName":"Acme Scrapers Ltd","UseCase":"Ad verification"}')
ENT_ID=$(echo "$ENT_RESP" | sed -n 's/.*"ID":"\([^"]*\)".*/\1/p')
ENT_API_KEY=$(echo "$ENT_RESP" | sed -n 's/.*"APIKey":"\([^"]*\)".*/\1/p')

# This should be REJECTED (403) — the KYB gate before approval, now
# enforced against a real resolved API key, not a body field:
curl -i -X POST localhost:8080/v1/sessions \
  -H "Authorization: Bearer $ENT_API_KEY" -d '{"NodeID":"whatever"}'

# Approve it (needs the real ADMIN_API_KEY you set above)
curl -X POST localhost:8080/v1/enterprise/approve \
  -H "Authorization: Bearer $ADMIN_API_KEY" -d "{\"ID\":\"$ENT_ID\"}"

# Log in as a user via OTP (no SMS provider wired up in this reference
# build, so the code comes back in the response — see the Authentication
# section above)
PHONE="+2348000000001"
OTP_RESP=$(curl -s -X POST localhost:8080/v1/auth/request-otp -d "{\"PhoneNumber\":\"$PHONE\"}")
CODE=$(echo "$OTP_RESP" | sed -n 's/.*"code_for_demo_only":"\([^"]*\)".*/\1/p')
VERIFY_RESP=$(curl -s -X POST localhost:8080/v1/auth/verify-otp -d "{\"PhoneNumber\":\"$PHONE\",\"Code\":\"$CODE\"}")
USER_TOKEN=$(echo "$VERIFY_RESP" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')

# Register a node — ownership comes from the token, not a request field
NODE_RESP=$(curl -s -X POST localhost:8080/v1/nodes/register \
  -H "Authorization: Bearer $USER_TOKEN" -d '{"DeviceFingerprint":"fp-abc-123"}')
NODE_ID=$(echo "$NODE_RESP" | sed -n 's/.*"ID":"\([^"]*\)".*/\1/p')

# Relay 10MB through the gateway — the reference client below does its
# own full login/registration/session flow, so this just needs the
# enterprise's key:
cd client/rust-relay
CONTROL_PLANE_URL=http://127.0.0.1:8080 \
DEMO_PHONE_NUMBER="$PHONE" DEMO_ENTERPRISE_API_KEY="$ENT_API_KEY" \
./target/release/rust-relay
cd ../..
```

Refresh the Wallet dashboard (sign in with the same phone number) —
balance and activity now reflect the real relay session. Refresh the
Admin console's Sessions tab to see the ML fraud score attached to that
session. Use the wallet's "Redeem" buttons to trigger a real
payout-engine call (mock VTU provider unless you've set
`VTPASS_API_KEY`/`VTPASS_SECRET_KEY` — see `.env.example`).

### Fraud model training (standalone)

```bash
cd fraud-service
python3 -m venv venv          # first time only — see note below on PEP 668
venv/bin/pip install flask scikit-learn joblib numpy pandas
venv/bin/python3 train.py     # regenerates fraud_model.joblib + prints a real eval report
venv/bin/python3 app.py       # serves it on :8082
```

Recent Debian/Ubuntu (including WSL) blocks system-wide `pip install`
with an "externally managed environment" error by design (PEP 668) — the
venv above is the actual fix, not a workaround. `scripts/setup.sh` does
this automatically as part of the full setup.

### Payout orchestrator demo (standalone, no other servers needed)

```bash
cd payout-engine
node demo.js
```

Watch for the `[requery]` line — the fix for the double-refund bug
identified in an earlier draft: on a simulated ambiguous provider
timeout, the orchestrator queries transaction status before deciding to
retry or refund, rather than assuming failure and refunding money that
was actually already sent.

### WireGuard (standalone proof)

```bash
cd wireguard
./gateway-setup.sh <client-pubkey>     # on the gateway host
./client-setup.sh <gateway-pubkey> <gateway-ip>   # on the client host
```

See `wireguard/README.md` for the full proof (real ICMP + TCP/HTTP
traffic through a genuine encrypted tunnel) and exactly what remains to
wire this into the platform's actual transport.

## Frontends

Three separate apps for three different audiences, styled accordingly:

- **Admin console** (`frontend/admin-console`) — internal ops tool. Dense
  tables, muted compliance-toned palette, monospace for every ID and byte
  count. KYB queue, node trust scores, session ledger with the ML fraud
  score column, payout reconciliation.
- **Wallet dashboard** (`frontend/wallet-dashboard`) — consumer-facing,
  mobile-first, warm palette. The balance is the dominant visual element;
  redemption uses plain language, not platform jargon.
- **Enterprise portal** (`frontend/enterprise-portal`) — B2B developer
  console. Signup → KYB status banner → API key (inactive until approved)
  → curl quickstart → session history.

## Bugs found by actually running this (kept here as a record)

1. **Session token/hash mismatch** — the control plane hashed one string
   at creation but returned a different one as the token, rejecting every
   session. Fixed with a proper independent random token.
2. **Self-signed cert generated as a CA cert** — `rustls` correctly
   refused it as a leaf certificate. Fixed with explicit `CA:FALSE`,
   `keyUsage`, and `extendedKeyUsage` extensions.
3. **Background processes don't survive across separate tool
   invocations** in this build environment (Postgres, started as a real
   system service, is the one exception).
4. **Payout-engine crashed the entire process on a downstream connection
   error** — an unhandled promise rejection from a failed control-plane
   call killed the whole service, not just the one failing request. Fixed
   by wrapping the handler so downstream failures return a clean `503`.
5. **`users.id` and `ledger_entries.account_id` were declared `UUID` in
   the original schema design, but the application actually uses
   caller-supplied string keys** (`"user_demo_1"`) **and literal account
   names** (`"platform_revenue"`, `"payout_clearing"`) — neither is a
   UUID. Caught and fixed before it ever hit runtime, by tracing through
   how the columns would actually be used rather than only checking that
   the schema was internally consistent.
6. **VTpass's requery endpoint needs its own `request_id`, not the
   orchestrator's internal idempotency key** — the generic
   `callProviderWithRequery` logic assumed a single reference string
   worked for every provider. Fixed to use a provider-supplied reference
   when available, falling back to the internal key for providers (like
   the mocks) that key their own dedupe off what we sent them.
7. **Kernel WireGuard isn't available in this sandbox** (`ip link add
   type wireguard` fails with "Unknown device type" despite `/dev/net/tun`
   and `cap_net_admin` both being present — consistent with a userspace-
   networking sandbox). Worked around by building `wireguard-go`
   (userspace WireGuard) from source, which required redirecting
   `golang.org/x/*` dependencies to their GitHub mirrors and dropping the
   Windows-only `wintun` dependency, both blocked by this network's
   allowlist and neither related to whether WireGuard itself works — see
   `wireguard/build.sh`.
8. **A real Swift concurrency bug**, caught while writing (not compiling)
   `RelayCoordinator.swift`: the first draft called an actor-isolated
   method (`report(_:)`) directly from a non-actor callback closure
   (`RelaySession`'s `onBytesSent`, which fires on its own dispatch
   queue). Real Swift wouldn't compile that without an `await` the
   surrounding closure isn't marked to provide. Fixed by wrapping the
   call in `Task { await self?.report(...) }` to properly hop back onto
   the actor. Caught by reasoning through Swift's actor-isolation rules
   during review, not by a compiler — which is exactly the kind of bug
   the missing Swift toolchain (see `ios/BandwidthPlatform/README.md`)
   means a real build could still surface more of.
9. **`schema.sql` was updated with a new `otp_requests` table, but the
   already-running Postgres database was never reloaded with it** —
   the very first auth integration test surfaced a real "relation
   `otp_requests` does not exist" error. A reminder that a schema file
   in the repo and the actual state of a running database are two
   different things, and only one of them is what the code executes
   against.
10. **A double-escaped apostrophe silently broke the entire admin
    console.** `isn\\'t` (double backslash) instead of `isn't` closed a
    JavaScript string one character early, which was a parse error on
    the whole `<script>` block — every event listener on the page
    silently failed to attach. The page loaded and looked completely
    normal; clicking anything did nothing. This was invisible to every
    verification method used up to that point (curl-simulated API
    sequences, `node -c` on backend `.js` files) because none of them
    actually executed the frontend's JavaScript. Found only once a real
    headless-browser test (Playwright) was run against the live stack —
    see `frontend/tests/README.md`.
11. **Leftover unconditional `loadAll()` calls in the wallet dashboard**,
    dating from before the OTP login rewrite, fired on every page load
    regardless of auth state — hitting `/v1/wallet/` and
    `/v1/ledger/history/` with an empty user id before login completed.
    The backend correctly rejected these with 401 (auth was never the
    problem), but it was real dead code left behind by a rewrite that
    nothing had exercised end-to-end until the same browser test caught
    it via unexpected console errors on page load.

## What real browser testing changed

Every frontend claim earlier in this document up through the auth
rewrite was verified by replaying the *API calls* a browser would make
with `curl`, and separately by running `node -c` against each backend
`.js` file. Neither approach can catch a bug in whether the frontend's
own JavaScript parses and runs at all — and #10 above is exactly that
class of bug, sitting in production-shipped code while every other check
passed. `frontend/tests/README.md` documents the real Playwright-driven
test suite now in the repo (`frontend/tests/ui-test.js`) and exactly what
it does and doesn't cover. All three frontends now pass a real,
browser-driven, 20-assertion end-to-end run with zero console or page
errors — a materially stronger claim than "the JS syntax-checks cleanly"
or "the right API calls return the right JSON."

## What is still NOT done

- **WireGuard is not wired into the actual gateway/client transport** —
  proven standalone, integration is a scoped, separate task (see
  `wireguard/README.md`).
- **Neither mobile client has been compiled or run.** Android was at
  least syntax-checked with a real Kotlin compiler; iOS wasn't checked at
  all (no Swift/Apple-framework toolchain available here) — see
  `mobile/README.md` and `ios/BandwidthPlatform/README.md`. Neither
  client has been updated for the new auth flow yet, either — both still
  register nodes with a caller-stated user id rather than going through
  OTP login the way the Rust client and web frontends now do.
- **iOS specifically still needs a real device/simulator pass** to
  validate the `NetworkExtension` entitlement setup and
  `NEPacketTunnelProvider` lifecycle — this is the part of the iOS build
  most likely to need real debugging, since it depends on Apple-side
  provisioning (a paid Apple Developer account is required for the
  Network Extension entitlement, even in development) that no amount of
  careful offline code review substitutes for.
- **VTpass adapter is untested against the live/sandbox API** — no
  credentials available here.
- **No rate limiting on the OTP endpoints** — `request-otp` can be
  called repeatedly for the same number with no cooldown, which is both
  an abuse vector (spamming a real phone once SMS is wired up) and a
  brute-force surface on `verify-otp` beyond the existing 5-attempt cap
  per code. Worth closing before this is reachable outside localhost.
- **No token revocation** — a user token is valid for 30 days from
  issuance with no server-side way to invalidate one early (e.g., on
  "sign out everywhere" or a suspected compromise). Fine for a demo,
  a real gap for production.
- No production security hardening (OS-level sandboxing, private-IP
  egress blocking, domain/IP threat-intel filtering beyond the ML fraud
  score) — specified in `docs/bandwidth-monetization-blueprint.md` §2,
  not implemented in this pass.
- The fraud model is trained on **synthetic** data — realistic in
  structure, not real production traffic. Retraining on real labeled
  `bandwidth_sessions` once the platform has volume is a data change to
  `train.py`, not a code change.

## Suggested next steps, in order

1. Update the Android and iOS clients for the new auth flow (OTP login,
   enterprise-keyed sessions) — they're the one piece of client code not
   yet consistent with how the Rust client and web frontends now work.
2. Add OTP rate limiting and token revocation — the two auth gaps most
   worth closing next, per the list above.
3. Get real VTpass sandbox credentials and verify `payout-engine/providers/vtpass.js`
   against the actual API before considering VTU wiring done, not just code-complete.
4. Decide whether to invest in wiring WireGuard into the real transport now
   (the hard part — proving it works at all in this environment — is
   done) or defer until real device-level bandwidth sharing is closer.
4. Get both mobile clients an actual build-and-run pass — Android in
   Android Studio, iOS in Xcode on a Mac (the latter also needs a paid
   Apple Developer account for the Network Extension entitlement, which
   is worth confirming access to early rather than after the code is
   otherwise ready).
