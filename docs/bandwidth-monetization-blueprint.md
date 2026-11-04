# P2P Bandwidth Monetization Platform — Technical Blueprint & MVP Specification

**Prepared as a Senior Principal Engineering / Product Strategy reference document**
**Scope:** System architecture, security model, monetization + VTU integration, and phased MVP delivery plan.

---

## 0. Framing & Business-Model Reality Check

Before the architecture: this product category (residential/P2P bandwidth reselling — Honeygain, PacketStream, EarnApp, Repocket, Grass, etc.) is a real, legal business model, but it lives in a regulatory and reputational grey zone that must shape your architecture from day one, not be bolted on later:

- **You are, functionally, becoming a residential proxy network operator.** Enterprise "buyers" of residential IPs are disproportionately used for web scraping, ad verification, sneaker botting, and — at the bad end — account takeover, ad fraud, and CAPTCHA farming. Your anti-abuse and client-vetting architecture is not a nice-to-have; it is the core IP of the business and the thing that keeps you bankable and compliant with your telecom/payment partners.
- **Your users' ISPs may prohibit connection sharing/reselling in their Terms of Service.** You are not legally liable for that, but you should disclose it to users clearly (informed consent), because ISP account suspension is a real user-facing risk.
- **Telecom VTU partners and payment processors (Paystack, Flutterwave, Moniepoint, etc.) will ask "what does your platform do" during KYB.** Being able to say "we operate strict IP-reputation and traffic-category filtering, block CSAM/illicit-market traffic, and maintain full audit trails" is what keeps your merchant account open.

Everything below is designed around that reality.

---

## PART A — STRATEGIC BLUEPRINT

## 1. System Architecture & Tech Stack

### 1.1 High-Level Architecture (text diagram)

```
                                   ┌───────────────────────────────┐
                                   │      Enterprise Traffic        │
                                   │  (customers buying bandwidth)  │
                                   └───────────────┬────────────────┘
                                                    │ HTTPS/SOCKS5 request
                                                    ▼
 ┌────────────────────────────────────────────────────────────────────┐
 │                     EXIT GATEWAY / SUPER-PROXY LAYER                │
 │   (Go, stateless, horizontally scaled — Envoy/HAProxy front)        │
 │   - Auth enterprise API key → session                               │
 │   - Selects eligible exit node (geo, ASN, sticky-session)           │
 │   - Enforces traffic-category filter (domain/IP blacklist, DPI)     │
 │   - Streams bytes, meters bytes in + out                            │
 └───────────────┬───────────────────────────────────────┬────────────┘
                  │ WireGuard / mTLS tunnel                │ gRPC/WebSocket
                  ▼                                        ▼
 ┌────────────────────────────┐            ┌───────────────────────────────┐
 │   CLIENT NODE (sandbox)     │            │   ORCHESTRATION CONTROL PLANE  │
 │  Android / Win / macOS /    │◄──────────►│  - Node registry & heartbeats  │
 │  Linux background agent     │  heartbeat │  - Metering ingestion service  │
 │  - Local SOCKS5 relay only  │  + metrics │  - Wallet & payout service     │
 │  - No FS/local-net access   │            │  - Fraud/anti-abuse engine     │
 │  - Battery/Wi-Fi policy     │            │  - Admin/ops dashboard API     │
 └──────────────┬──────────────┘            └───────────┬───────────────────┘
                │                                        │
                ▼                                        ▼
     ┌─────────────────────┐                  ┌───────────────────────┐
     │  Local ISP / Router  │                  │   Data Layer            │
     │  (user's home Wi-Fi) │                  │  Postgres (ledger)      │
     └─────────────────────┘                  │  TimescaleDB/ClickHouse │
                                               │  (metering time-series) │
                                               │  Redis (session/rate)   │
                                               │  Kafka/NATS (event bus) │
                                               └───────────┬─────────────┘
                                                            ▼
                                               ┌────────────────────────┐
                                               │  Payout Engine          │
                                               │  → VTU API aggregator   │
                                               │  → Bank/Paystack rails  │
                                               └────────────────────────┘
```

Key architectural decision: **the client never terminates enterprise traffic directly to the open internet without transiting the Exit Gateway.** The gateway is your only enforcement + metering + legal-liability control point. Never let enterprise clients hold a direct WireGuard/SOCKS5 credential that maps straight to a consumer device's IP without your gateway proxying/logging it — that's the difference between "we can audit and kill any traffic stream instantly" and "we have no idea what happened until law enforcement calls."

### 1.2 Client Node — Language/Framework Recommendation

| Platform | Recommendation | Why |
|---|---|---|
| **Windows / macOS / Linux (desktop)** | **Rust** for the core relay/metering daemon, compiled to a native background service (Windows Service / launchd daemon / systemd unit). Optional thin Tauri or Electron shell only for the settings UI (Rust core keeps the always-running process tiny; Electron is far too heavy to run 24/7 in the background). | Rust gives you memory safety without GC pauses, tiny binary/RAM footprint (a Go equivalent runs ~2-3x the RSS due to goroutine/GC overhead at idle), and first-class async I/O (Tokio) ideal for proxying thousands of small connections. Cross-compiles cleanly to all three desktop OSes from one codebase. |
| **Android** | **Kotlin + native Rust core via JNI/UniFFI**, run as a `Foreground Service` with a persistent notification (required by Android 8+ to avoid being killed), using `JobScheduler`/`WorkManager` only for periodic heartbeats when the foreground service itself is dormant. | Kotlin is mandatory for correct integration with Android's background-execution and battery-optimization APIs (Doze mode, App Standby Buckets, Foreground Service types). The actual bandwidth relay logic should be the *same* Rust core used on desktop (shared via `cargo-ndk`), so metering/anti-fraud logic isn't duplicated and re-audited per platform. |
| **iOS** | Deliberately **out of scope for v1.** iOS does not allow arbitrary background network relay processes (no persistent background sockets, aggressive suspension, App Store policy explicitly forbids proxy/VPN-selling apps that aren't a declared "Network Extension" with a narrow allowed use-case). Support iOS later, if at all, only via a `PacketTunnelProvider` extension with Apple's explicit blessing — treat this as a v2+ discussion, not MVP. |

Reject React Native for the client agent itself: RN is fine for a companion *dashboard* app (stats, wallet, redemption UI) but wrong for the actual network relay — you'd be running a JS bridge and a full RN runtime just to hold a socket open, which is both a battery and memory liability, and RN has no good story for a true always-on background daemon on desktop.

**Client responsibilities (and nothing more — least privilege by design):**
- Register with orchestration server, receive signed session credentials (short-lived JWT, rotated).
- Open outbound-only WireGuard tunnel (or mTLS+SOCKS5 fallback) to the Exit Gateway — client never listens on a publicly routable port.
- Relay bytes for gateway-assigned sessions; locally rate-limit and cap concurrent sessions.
- Emit signed heartbeat + metering telemetry every 15–30s.
- Enforce local policy: Wi-Fi-only, battery threshold, foreground-app CPU/mem ceilings.

### 1.3 Orchestration Server — Backend Stack

Recommend a **Go-first microservices** backend for the control plane and gateway, with Python reserved for fraud/ML scoring, and Node.js only if you want a fast-to-hire team for the customer-facing dashboard/BFF layer.

| Service | Language | Rationale |
|---|---|---|
| **Exit Gateway / Proxy Router** | Go | Excellent concurrency model (goroutines) for proxying tens of thousands of concurrent low-throughput streams; mature SOCKS5/HTTP proxy libraries; easy to run at the edge (small static binaries in containers). |
| **Node Registry & Heartbeat Service** | Go | High write throughput (heartbeats from potentially millions of devices), gRPC bidirectional streaming for low-overhead persistent connections. |
| **Metering Ingestion Service** | Go, consuming from Kafka/NATS | Needs to sustain high ingest QPS with backpressure; write-optimized pipeline into the time-series store. |
| **Wallet / Ledger Service** | Go or Java (if you want strong existing double-entry-ledger libraries) | Must be the most rigorously tested service in the stack — treat it like fintech code, not a side feature. Idempotent, event-sourced, double-entry accounting. |
| **Fraud/Anti-Abuse Scoring** | Python (FastAPI) + a feature store | Python's ML ecosystem (scikit-learn/XGBoost, or hosted classifiers) is the pragmatic choice for anomaly detection on node behavior; doesn't need Go's raw throughput. |
| **Payout / VTU Orchestrator** | Go or Node.js | I/O-bound orchestration calling out to 3rd-party VTU/payment APIs; Node.js is fine here since it's not on your hot path and your team may iterate on payout provider integrations frequently. |
| **Public API / BFF (mobile+web dashboard)** | Node.js (NestJS) or Go | NestJS is a strong pick if you want fast iteration on a customer-facing REST/GraphQL API with a large hiring pool. |
| **Admin/Ops Console** | Node.js/React | Standard internal tooling stack. |

**Inter-service communication:** gRPC for internal service-to-service calls (heartbeat streaming, wallet debits/credits), Kafka (or NATS JetStream if you want a lighter footprint at MVP scale) as the event backbone for metering events, payout triggers, and fraud signals — this decouples ingestion rate from downstream processing rate, which matters a lot once you have node counts that spike unpredictably.

### 1.4 Database Design

Use **purpose-fit polyglot persistence** — do not try to force time-series metering and financial ledgers into the same database.

**a) Transactional / Ledger store — PostgreSQL**
- Source of truth for users, nodes, wallets, and the financial ledger. ACID guarantees are non-negotiable for money movement.
- Model the wallet as an **immutable double-entry ledger**, not a mutable `balance` column:

```sql
-- Core entities
CREATE TABLE users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    phone_number VARCHAR(20) UNIQUE NOT NULL,
    email VARCHAR(255),
    kyc_status VARCHAR(20) NOT NULL DEFAULT 'unverified',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    status VARCHAR(20) NOT NULL DEFAULT 'active' -- active, suspended, banned
);

CREATE TABLE nodes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id),
    device_fingerprint VARCHAR(128) NOT NULL,
    platform VARCHAR(20) NOT NULL, -- android, windows, macos, linux
    public_key TEXT NOT NULL,       -- WireGuard/session pubkey
    isp_asn INTEGER,
    country_code CHAR(2),
    trust_score NUMERIC(5,2) NOT NULL DEFAULT 50.0,
    status VARCHAR(20) NOT NULL DEFAULT 'pending_verification',
    last_heartbeat_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(user_id, device_fingerprint)
);

-- Double-entry ledger: NEVER update a balance in place. Append-only.
CREATE TABLE ledger_entries (
    id BIGSERIAL PRIMARY KEY,
    entry_group_id UUID NOT NULL,       -- ties debit+credit pairs together
    account_id UUID NOT NULL,           -- user wallet, platform revenue, VTU float, etc.
    account_type VARCHAR(30) NOT NULL,  -- 'user_wallet','platform_revenue','payout_clearing'
    direction VARCHAR(6) NOT NULL CHECK (direction IN ('debit','credit')),
    amount_minor_units BIGINT NOT NULL CHECK (amount_minor_units > 0), -- kobo/cents
    currency CHAR(3) NOT NULL DEFAULT 'NGN',
    reference_type VARCHAR(30) NOT NULL, -- 'bandwidth_earning','payout','adjustment'
    reference_id UUID NOT NULL,          -- FK to bandwidth_session or payout_request
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_ledger_account ON ledger_entries(account_id, created_at);

CREATE TABLE payout_requests (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id),
    amount_minor_units BIGINT NOT NULL,
    payout_method VARCHAR(20) NOT NULL, -- 'airtime','data_bundle','bank_transfer'
    provider VARCHAR(30),               -- 'vtu_aggregator_x','paystack', etc.
    provider_reference VARCHAR(100),
    idempotency_key VARCHAR(64) UNIQUE NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'pending', -- pending, processing, success, failed, reversed
    failure_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

A user's spendable balance is always **derived** (`SUM(credits) - SUM(debits)` for their `account_id`, materialized into a fast-read cache in Redis and periodically reconciled) — never a value you write directly. This is what makes the system auditable and resistant to double-spend/race-condition payout bugs.

**b) Time-series metering store — TimescaleDB (Postgres extension) or ClickHouse**
- Raw per-session bandwidth events land here first, at very high write volume, before being rolled up.
- TimescaleDB is the pragmatic MVP choice (same Postgres ops model, hypertables, continuous aggregates). Migrate hot paths to ClickHouse later if ingest exceeds ~50–100k events/sec.

```sql
CREATE TABLE bandwidth_sessions (
    session_id UUID NOT NULL,
    node_id UUID NOT NULL,
    enterprise_client_id UUID NOT NULL,
    started_at TIMESTAMPTZ NOT NULL,
    ended_at TIMESTAMPTZ,
    bytes_up BIGINT NOT NULL DEFAULT 0,
    bytes_down BIGINT NOT NULL DEFAULT 0,
    gateway_confirmed_bytes BIGINT,   -- the authoritative figure (see 2.3 anti-spoofing)
    client_reported_bytes BIGINT,     -- what the node claimed — compared, not trusted
    exit_ip INET,
    dest_category VARCHAR(30),        -- filtered category tag from DPI/domain classifier
    time TIMESTAMPTZ NOT NULL DEFAULT now()
);
SELECT create_hypertable('bandwidth_sessions', 'time');

CREATE TABLE node_heartbeats (
    node_id UUID NOT NULL,
    time TIMESTAMPTZ NOT NULL DEFAULT now(),
    battery_pct SMALLINT,
    is_charging BOOLEAN,
    network_type VARCHAR(10), -- 'wifi','cellular','ethernet'
    cpu_pct SMALLINT,
    mem_mb INTEGER,
    app_version VARCHAR(20)
);
SELECT create_hypertable('node_heartbeats', 'time');
```

**c) Hot-path cache / rate-limiting / session state — Redis**
- Live node availability set (sorted set by trust score + geo, for gateway node selection).
- Sliding-window rate limiters per node, per enterprise client, per IP.
- Cached wallet balances (write-through from ledger, TTL + reconciliation job).
- Idempotency keys for payout requests (prevents double-payout on retry storms).

---

## 2. Security, Privacy & Traffic Filtering

### 2.1 Sandboxing the client — traffic must never reach local resources

The single most important trust guarantee you make to users is: *"Enterprise traffic exits through your internet connection, but it can never see your files, your other devices, or your local network."* Architect this as defense-in-depth, not a single check:

1. **No local network access by design, not by filter.** The client agent should run the relay as an **outbound-only SOCKS5/WireGuard client to your Exit Gateway** — it is never a listening proxy that enterprise traffic connects to directly. Enterprise traffic never touches the user's OS socket stack in a way that could pivot to `192.168.x.x`/`10.x.x.x`/`169.254.x.x`/`fe80::` ranges. Enforce this at **two layers**:
   - **Gateway-side egress filtering:** the Exit Gateway (which brokers the actual destination) hard-blocks any destination resolving to RFC1918 private ranges, loopback, link-local, and multicast — before the request is ever forwarded down to a client node.
   - **Client-side OS sandboxing:** run the relay process under the most restrictive sandbox primitive available per OS:
     - **Windows:** run as a restricted-token Windows Service (`LocalService` account, not `LocalSystem`), with AppContainer isolation for the network component where feasible; Windows Filtering Platform (WFP) callout driver or simple OS-level firewall rules to block the process from originating connections to private ranges.
     - **macOS:** run under a hardened runtime with sandbox entitlements (`com.apple.security.network.client` only, no file-system entitlements), packaged as a `launchd` daemon with a minimal `com.apple.security.app-sandbox` profile.
     - **Linux:** run inside a **network namespace** with an explicit `nftables`/`iptables` egress allow-list (only the Exit Gateway's IP/port), and drop all capabilities except `CAP_NET_BIND_SERVICE` if even needed; consider a lightweight container (via `runc`/`bubblewrap`) for the relay binary so it has no filesystem view beyond its own directory.
     - **Android:** the relay logic lives inside your app's own sandboxed process (Android enforces per-app UID isolation already); explicitly do **not** request `READ_EXTERNAL_STORAGE`, contacts, SMS, or any permission unrelated to networking + foreground-service notification. Route all traffic through Android's `VpnService` API scoped only to the tunnel to your gateway — never open a local listening SOCKS proxy on the device that other LAN devices could reach.
2. **No file-system access from the relay process at all.** The relay binary should not link against or call any file I/O APIs beyond its own config/log directory — enforce this via code review + static analysis (e.g., `cargo-geiger`/capability linting) as a release gate, not just a runtime sandbox.
3. **Router/local-device protection:** because private-IP ranges are blocked at both the gateway and OS network layer, there is no code path by which enterprise traffic can reach a user's smart TV, NAS, or router admin panel — this should be validated with automated **penetration tests in CI** (a red-team test suite that repeatedly tries SSRF-style pivots: `http://192.168.1.1`, `http://169.254.169.254/latest/meta-data` cloud-metadata-style probes, DNS rebinding attempts, etc.) run against every gateway release before deploy.

### 2.2 Encryption & Routing Protocols

- **Client ↔ Gateway tunnel:** **WireGuard** as the primary transport — modern cryptography (Noise protocol framework), tiny attack surface (~4,000 LOC vs OpenVPN's ~100,000), excellent mobile battery characteristics (fast handshake resume after network changes, important for Doze-mode wakeups), and kernel-level implementations available on Linux/Android. Use ephemeral, frequently-rotated keys per session — never a long-lived static key baked into the client.
- **Fallback transport:** mTLS-wrapped SOCKS5 (TLS 1.3) for networks/carriers that block UDP/WireGuard — detect via handshake failure and fall back automatically.
- **Gateway ↔ Enterprise client:** standard HTTPS/SOCKS5-over-TLS with per-customer API keys and IP allow-listing on the enterprise side; consider mutual TLS for higher-tier enterprise accounts.
- **Control plane traffic (heartbeats, metering, wallet events):** gRPC over TLS 1.3 exclusively; never allow plaintext control-plane traffic even internally between microservices — assume internal network compromise is possible (zero-trust internal networking, e.g., via a service mesh like Linkerd/Istio with mTLS-by-default).
- **At-rest encryption:** encrypt the ledger and PII (phone numbers, KYC docs) columns at rest (Postgres `pgcrypto` or column-level KMS-backed encryption), and encrypt object storage (KYC uploads) with per-object keys via a cloud KMS.

### 2.3 Anti-abuse & fraud prevention (this is the core IP of the platform)

**Traffic-level filtering (protects you from being an accessory to abuse):**
- **DNS/domain and destination-IP category filtering** at the gateway: integrate a threat-intel feed (e.g., commercial IP/domain reputation API) to block known malware C2, phishing domains, CSAM-associated domains (zero tolerance, hard block + auto-report), and known fraud/carding sites. Maintain your own denylist informed by abuse reports.
- **DPI-lite content-category tagging** (via SNI inspection on TLS ClientHello — you don't need to break TLS to categorize destinations) to flag/rate-limit high-abuse categories: bulk account-creation endpoints, known bot-detection-evasion targets, etc. Log category + destination ASN per session for audit, not full payload content (privacy-preserving metering, not payload inspection).
- **Enterprise client vetting (KYB):** every enterprise buyer goes through a business-verification flow before they get API access — company registration check, declared use-case, and a manual review gate for the first N transactions. This is your strongest lever: most abuse is easier to stop by not selling to bad actors than by filtering after the fact.
- **Per-enterprise-client behavioral monitoring:** rate of requests, diversity of destination domains, ratio of failed/blocked requests — sudden spikes in blocked-category attempts trigger automatic suspension pending review.

**Node-level fraud prevention (protects your unit economics):**
- **Anti-spoofing for reported bandwidth:** never trust the client's self-reported byte counts as the basis for payment. The **Exit Gateway is the authoritative meter** — it counts bytes as they pass through its own socket buffers on the way to/from the enterprise client. The client's heartbeat-reported figures are logged for anomaly detection (client under-reporting vs. gateway is a *sign of a compromised/modified client*, not evidence for payment) but the ledger only ever credits `gateway_confirmed_bytes`.
- **Session-bound cryptographic proof:** each proxied session is tagged with a gateway-issued session token bound to a specific node's session key; bytes are only credited if they flow through a session with a valid, gateway-verified token — a modified client can't fabricate sessions that were never actually proxied.
- **Device/network fingerprinting + Sybil resistance:** cap earnings-eligible nodes per household/ASN/subnet (e.g., soft cap per `/24` and hard cap per residential ASN unless verified as a business), require phone-number verification (SMS OTP) per account, and flag data-center/hosting-provider ASNs (via IP-to-ASN + IP-intelligence lookup) — bandwidth from a cloud VM masquerading as "residential" is a common fraud vector and must be auto-rejected at node registration.
- **Trust score model:** maintain a per-node `trust_score` (as in the schema above) updated by a background scoring job (Python service) factoring in: heartbeat consistency, ratio of gateway-confirmed vs. client-reported bytes, session failure rates, geographic/ASN consistency over time, and payout dispute history. Low-trust nodes get throttled or excluded from the eligible-node pool before being outright banned — graduated response reduces false-positive harm to honest users.
- **Rate limiting** at multiple layers: per-node concurrent session cap, per-node daily bandwidth cap (protects the user's own connection *and* limits blast radius of a compromised node), per-enterprise-client QPS/bandwidth cap tied to their tier/spend.

---

## 3. Monetization & Telecom VTU Integration

### 3.1 Unit economics

Structure pricing as **you sell bandwidth to enterprises per GB, you pay users per GB, and the spread is your margin** — plus float/FX/payout-fee considerations:

```
Enterprise price:        $X.XX / GB   (market rate for residential proxy GB, tiered by geo — 
                                        Africa-sourced residential IPs often command a premium 
                                        due to scarcity vs. US/EU supply)
User payout rate:         Y% of X      (industry norms in this space run user payouts around 
                                        20–40% of enterprise revenue per GB — validate against 
                                        current competitor payout rates before fixing this number)
Gross margin:             (X - Y) per GB, before:
   - Payment processing / VTU aggregator fees (typically 0.5–3% depending on rail)
   - Infrastructure cost (gateway bandwidth, compute, DB) — must be netted against enterprise 
     revenue since your gateway also relays the bytes
   - Fraud losses / chargebacks reserve
   - KYC/compliance overhead
```

Recommend running **payout as a tiered rate** (higher $/GB payout for nodes with sustained high trust scores and uptime) — this rewards your most reliable, lowest-fraud-risk supply and is a stronger anti-churn lever than a flat rate.

**Do not pay out in real time per byte.** Accrue earnings continuously in the ledger (append-only, as above) but only make earnings *withdrawable* once a session is finalized and passed a short fraud-review window (e.g., minutes, not bytes-in-flight) — this gives your fraud engine a chance to claw back a session that gets reclassified as abusive before money actually leaves the platform.

### 3.2 Payout Gateway Architecture — VTU + local payment rails

**Nigeria/Africa-specific market note (since you're integrating VTU):** as of 2026 there's a healthy ecosystem of VTU aggregator APIs (e.g., Reloadly for pan-African/global multi-country reach with more formal compliance posture, plus Nigeria-focused aggregators like VTpass-style and Pairgate/eBills/VTUGATE-style providers for MTN/Airtel/Glo/9mobile). Given you're building a serious platform (not a side-hustle VTU reseller), prioritize providers that offer: **written SLAs, webhook-based async status callbacks, sandbox/test-mode environments, and multi-provider failover** — several of the smaller Nigerian VTU resale platforms are themselves resellers with thin margins and shakier uptime, which is a real risk if you route payouts through only one. Architect for **multi-provider abstraction from day one:**

```
                     ┌─────────────────────────────┐
                     │     Payout Orchestrator       │
                     │  (idempotent state machine)   │
                     └───────────────┬────────────────┘
                                      │
              ┌───────────────────────┼───────────────────────┐
              ▼                       ▼                       ▼
     ┌─────────────────┐   ┌──────────────────┐    ┌────────────────────┐
     │ VTU Provider A   │   │  VTU Provider B    │    │  Bank/Fiat rail     │
     │ (primary)        │   │  (failover)        │    │  (Paystack/         │
     │ Airtime + Data   │   │  Airtime + Data     │    │  Flutterwave/       │
     └─────────────────┘   └──────────────────┘    │  Moniepoint transfer)│
                                                     └────────────────────┘
```

**Payout state machine (per `payout_requests` row):**

`pending → reserved (funds locked in ledger) → submitted_to_provider → provider_processing → success | failed`

- **failed** → automatic retry against the *same* provider up to N times with exponential backoff (respecting the provider's own idempotency key), then automatic failover to the secondary VTU provider for that network, then — if all providers fail — refund the reserved amount back to the user's withdrawable balance and notify the user, rather than silently losing their request.
- Every provider call is made with a **stable idempotency key** (`payout_requests.idempotency_key`) so that a network timeout followed by a client retry can never result in double top-up — check-then-act against the provider's own dedupe if they support it, and always re-query `GET /transaction-status` before blindly retrying a call whose response you didn't receive.
- **Webhooks over polling** where the provider supports it — reduces cost and latency; but always pair webhooks with a periodic reconciliation job (`requery` endpoints most of these providers expose) since webhook delivery is not 100% guaranteed.
- **Reconciliation job (nightly):** compare your `payout_requests` ledger against each VTU provider's transaction history export — flag mismatches for manual review before they compound. This is standard fintech hygiene and will matter a lot the first time a provider's webhook silently drops for an hour.

### 3.3 Smart Network Controls — protecting users' cellular data

This is a trust-critical feature; get it wrong once (a user's cellular data plan gets silently drained) and you'll generate viral negative press.

- **Default-on, opt-in-only-with-friction cellular sharing.** Ship with `Wi-Fi only` as the hard default; require an explicit, separately-confirmed opt-in (not a single toggle buried in settings) if you ever allow cellular sharing at all — and even then, cap it hard (e.g., a small daily MB ceiling) and never enable it silently after an app update.
- **Client-side network state listeners:**
  - **Android:** `ConnectivityManager.NetworkCallback` registered for `NetworkCapabilities.TRANSPORT_WIFI` vs `TRANSPORT_CELLULAR`; also check `isActiveNetworkMetered()` — some Wi-Fi networks (mobile hotspots) are metered too and should be treated like cellular by default.
  - **Desktop (Rust core):** on Windows, query `INetworkListManager`/`WlanAPI` for connection type; on macOS, `SCNetworkReachability` + checking the active interface against known cellular (e.g., tethered) interfaces; on Linux, inspect `nmcli`/`NetworkManager` D-Bus state or `/sys/class/net/*/type`.
- **Fail-safe behavior:** the moment a network-change event fires and the new network is cellular/metered while sharing was Wi-Fi-only, the client must **immediately tear down active relay sessions** (not wait for the next heartbeat interval) — treat this as a hard interrupt, not a polled check.
- **Battery/power policy:** pause sharing below a configurable battery threshold (e.g., 20%) unless charging; always pause on Android Battery Saver / iOS-equivalent low-power mode signals; respect OS Doze/App Standby without fighting the OS (don't use aggressive wake-locks purely to keep sharing alive — that's exactly the behavior that gets apps flagged and uninstalled).

---

## 4. Implementation Roadmap & MVP Scope

**Principle:** ship the smallest system that can (a) prove the core metering-to-payout loop is trustworthy end-to-end and (b) prove your anti-abuse gate actually blocks the traffic categories that would get you banned by a payment processor — everything else is sequenced after that trust loop is validated.

| Phase | Duration (indicative) | Scope |
|---|---|---|
| **Phase 0 — Foundations** | 3–4 weeks | Core repo scaffolding; Postgres ledger schema + migrations; WireGuard tunnel POC between a bare Rust client and a bare Go gateway; basic node registration + phone OTP verification; no enterprise traffic yet — just prove the tunnel + metering pipe works and bytes counted at the gateway match bytes sent in a controlled test. |
| **Phase 1 — Core Node Network (MVP-1)** | 6–8 weeks | Desktop client (Windows/macOS/Linux) with Wi-Fi-only enforcement, battery policy, foreground background service; Exit Gateway with private-IP egress blocking + basic domain blacklist; node heartbeat service; TimescaleDB metering ingestion; a small internal "enterprise" test harness (your own scraping test traffic) to validate the full loop under load. **No real payouts yet — earnings just accrue in the ledger.** |
| **Phase 2 — Metering Integrity & Anti-Fraud (MVP-2)** | 4–6 weeks | Gateway-authoritative byte counting fully replacing client-reported figures as the payment basis; trust-score service (even a simple rules-based version before ML); Sybil-resistance (ASN/device fingerprint caps); data-center-ASN detection and auto-rejection; red-team test suite for SSRF/private-IP pivot attempts, wired into CI as a release gate. |
| **Phase 3 — Payout Engine (MVP-3)** | 4–5 weeks | Payout orchestrator with idempotent state machine; integrate first VTU provider (primary) + one failover provider; webhook + reconciliation job; withdrawal UI/API with minimum thresholds and the fraud-review holding window before funds become withdrawable. |
| **Phase 4 — Android Client** | 5–6 weeks | Kotlin shell + shared Rust core via JNI; `VpnService`-scoped tunnel; Android-specific battery/Doze handling; Play Store policy review (network-sharing apps face extra scrutiny — budget review-cycle time). |
| **Phase 5 — Enterprise Onboarding & Real Traffic** | Ongoing, starts ~Week 20 | KYB flow for enterprise buyers; API key issuance + per-client rate limits; billing/invoicing service; enterprise dashboard (usage, spend); begin routing small volumes of real enterprise traffic with tight monitoring before opening broader access. |
| **Phase 6 — Scale & Hardening** | Ongoing | Migrate metering store to ClickHouse if ingest volume demands it; service mesh + zero-trust internal networking; expand VTU provider panel + add fiat/bank payout rails; ML-based fraud scoring to replace the rules engine; SOC2-style controls if enterprise customers require it. |

**Sequencing rationale:** payouts (Phase 3) deliberately come *after* metering integrity (Phase 2) is proven, not before — the single biggest existential risk to this business is paying users for bandwidth you can't actually verify was delivered, or unknowingly laundering abuse traffic through your gateway. Both of those failure modes are far cheaper to prevent architecturally up front than to remediate after you have real money and real enterprise customers depending on the system.

---

---

## PART B — MVP TECHNICAL SPECIFICATION (Client, Metering API, Payout Engine)

This section drills into implementation-level detail for the three subsystems that make up the MVP core loop: client-side metering/policy enforcement, the backend heartbeat + metering protocol, and the VTU payout workflow.

## 5. Client App Specification

### 5.1 Background execution & battery strategy

**Android:**
- Run the relay as a `Foreground Service` (`android:foregroundServiceType="dataSync"`) with a persistent low-priority notification ("Sharing bandwidth — earning ₦X today") — this is required to avoid being killed by the OS and is also the honest, non-deceptive UX pattern Play Store policy expects for this app category.
- Use `WorkManager` for periodic reconciliation tasks (e.g., syncing the local metering buffer if the foreground service was briefly killed) but treat the foreground service, not WorkManager, as the primary relay host — `WorkManager` jobs are not guaranteed to run on tight intervals under Doze.
- Request exemption from battery optimization (`REQUEST_IGNORE_BATTERY_OPTIMIZATIONS`) but make this an explicit, explained opt-in during onboarding, not a silent request — over-asking here is a common cause of Play Store rejection and user distrust.
- Hard-stop relay sessions immediately on: battery below threshold and not charging, Battery Saver mode enabled, thermal throttling signals (`PowerManager.getCurrentThermalStatus()`), or app moved to a user-initiated "paused" state.

**Desktop (Windows/macOS/Linux):**
- Install as a native OS service/daemon (Windows Service via `sc.exe`/NSSM-style wrapper generated by your installer; `launchd` plist on macOS; `systemd` unit on Linux) so it survives user logout, but ship a clearly visible tray/menu-bar icon and a one-click pause/resume — always-on background processes without an obvious kill switch erode trust fast.
- CPU/mem ceilings: cap the relay process at a conservative CPU % (e.g., throttle concurrent sessions if sustained CPU exceeds a threshold) so it never visibly competes with the user's foreground applications.
- Pause on laptop battery (unplugged) by default, mirroring the mobile battery policy — configurable, off by default.

### 5.2 Network state listeners

Core rule: **every relay session must be provably tied to a currently-Wi-Fi/ethernet connection, re-validated continuously, not just at session start.**

```kotlin
// Android — Kotlin sketch
class NetworkPolicyMonitor(private val connectivityManager: ConnectivityManager) {

    private val wifiOnlyRequest = NetworkRequest.Builder()
        .addTransportType(NetworkCapabilities.TRANSPORT_WIFI)
        .build()

    fun start(onEligible: () -> Unit, onIneligible: () -> Unit) {
        connectivityManager.registerNetworkCallback(wifiOnlyRequest, object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) {
                val caps = connectivityManager.getNetworkCapabilities(network)
                val metered = connectivityManager.isActiveNetworkMetered
                if (caps?.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) == true && !metered) {
                    onEligible()
                } else {
                    onIneligible() // covers metered "Wi-Fi" like mobile hotspots
                }
            }
            override fun onLost(network: Network) = onIneligible()
            override fun onCapabilitiesChanged(network: Network, caps: NetworkCapabilities) {
                if (!caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) || connectivityManager.isActiveNetworkMetered) {
                    onIneligible() // immediate teardown, not next heartbeat
                }
            }
        })
    }
}
```

```rust
// Desktop Rust core — pseudo-interface, platform impls behind a trait
trait NetworkPolicySource {
    fn current_connection_type(&self) -> ConnectionType; // Wifi, Ethernet, Cellular, MeteredWifi, Unknown
    fn is_charging(&self) -> bool;
    fn battery_pct(&self) -> Option<u8>;
}

// Called on a tight poll (e.g., every 2s) PLUS OS change-event hooks where available
fn evaluate_sharing_eligibility(policy: &SharingPolicy, src: &dyn NetworkPolicySource) -> bool {
    let net_ok = matches!(src.current_connection_type(), ConnectionType::Wifi | ConnectionType::Ethernet)
        || policy.cellular_opt_in_enabled;
    let power_ok = src.is_charging() || src.battery_pct().unwrap_or(0) >= policy.min_battery_pct;
    net_ok && power_ok
}
```

### 5.3 Real-time local metering (pre-sync)

The client keeps a **local, tamper-evident metering buffer** that it syncs frequently — this is diagnostic/reconciliation data, not the basis for payment (the gateway remains authoritative, per §2.3), but mismatches between local and gateway figures are themselves a fraud signal.

```rust
struct SessionMeter {
    session_id: Uuid,
    bytes_up: AtomicU64,
    bytes_down: AtomicU64,
    started_at: Instant,
}

impl SessionMeter {
    fn record(&self, up: u64, down: u64) {
        self.bytes_up.fetch_add(up, Ordering::Relaxed);
        self.bytes_down.fetch_add(down, Ordering::Relaxed);
    }

    // Flushed on a fixed interval AND on session close — sequence-numbered so
    // the server can detect gaps/replays, and HMAC-signed with the node's
    // session key so the payload can't be forged by a modified client.
    fn to_signed_report(&self, seq: u64, session_key: &[u8]) -> SignedMeterReport {
        let payload = MeterReport {
            session_id: self.session_id,
            seq,
            bytes_up: self.bytes_up.load(Ordering::Relaxed),
            bytes_down: self.bytes_down.load(Ordering::Relaxed),
            timestamp: SystemTime::now(),
        };
        let sig = hmac_sha256(session_key, &payload.canonical_bytes());
        SignedMeterReport { payload, sig }
    }
}
```

## 6. Backend Node Metering & Proxy Gateway

### 6.1 Heartbeat & metering protocol

Use **gRPC bidirectional streaming** for the persistent heartbeat channel (lower overhead than repeated REST polling, and lets the server push commands — e.g., "pause session," "rotate key" — to the client immediately) with a **REST/webhook fallback** for constrained environments.

```protobuf
// node_control.proto
syntax = "proto3";

service NodeControl {
  rpc HeartbeatStream(stream HeartbeatMessage) returns (stream ServerCommand);
}

message HeartbeatMessage {
  string node_id = 1;
  int64 timestamp_unix = 2;
  BatteryState battery = 3;
  NetworkState network = 4;
  repeated SignedMeterReport meter_reports = 5; // batched since last heartbeat
  string agent_version = 6;
}

message ServerCommand {
  oneof command {
    PauseSharing pause = 1;
    ResumeSharing resume = 2;
    RotateSessionKey rotate_key = 3;
    RevokeNode revoke = 4; // fraud/ban action, takes effect immediately
  }
}

message SignedMeterReport {
  string session_id = 1;
  uint64 seq = 2;
  uint64 bytes_up = 3;
  uint64 bytes_down = 4;
  int64 timestamp_unix = 5;
  bytes hmac_signature = 6;
}
```

**REST equivalent (for the fallback path / initial registration):**

```
POST /v1/nodes/register
Authorization: Bearer <install-time attestation token>
{
  "device_fingerprint": "sha256:...",
  "platform": "android",
  "public_key": "base64-wireguard-pubkey",
  "app_version": "1.4.2"
}
→ 201
{
  "node_id": "b3f1...",
  "session_key": "base64...",      // used to HMAC-sign meter reports
  "gateway_endpoint": "wg://gw-lagos-1.internal:51820",
  "heartbeat_interval_sec": 20
}

POST /v1/nodes/{node_id}/heartbeat
{
  "battery_pct": 78, "is_charging": false, "network_type": "wifi",
  "meter_reports": [ { "session_id": "...", "seq": 42, "bytes_up": 10240,
                        "bytes_down": 51200, "hmac_signature": "..." } ]
}
→ 200 { "commands": [] }   // or a pause/revoke command if flagged
```

### 6.2 Preventing spoofed bandwidth logs

Layered controls (do not rely on any single one):

1. **Gateway is the payer of record.** The proxy gateway independently counts bytes as they physically pass through its own socket buffers per session — this figure (`gateway_confirmed_bytes`), not the client's report, is what's written to the ledger. A malicious client can lie in its heartbeat; it cannot make the gateway relay bytes that were never actually sent to an enterprise customer.
2. **HMAC-signed, sequence-numbered client reports** (shown above) — prevents naive replay/tampering of the *diagnostic* stream and lets you detect a modified client (invalid signature → immediate node flag + investigation).
3. **Session tokens are single-use and gateway-issued.** A node cannot fabricate a "session" that credits it bytes — the gateway only accepts traffic against a session ID it itself created in response to a real enterprise request, bound to that node's currently-authenticated WireGuard tunnel.
4. **Statistical cross-checks:** a background job compares `gateway_confirmed_bytes` vs `client_reported_bytes` per node over rolling windows; nodes with sustained large deltas (in either direction) are downgraded in trust score even if no single session looks obviously fraudulent — this catches slow-drift tampering that per-session checks miss.
5. **Impossible-throughput checks:** flag any node reporting sustained throughput inconsistent with plausible residential bandwidth for its declared ISP/region (e.g., a node claiming 500 Mbps sustained on an ASN known for capped residential plans) for manual/ML review.

### 6.3 Database schema — wallets, pending payouts, bandwidth logs (Postgres + Redis)

Postgres tables for `ledger_entries`, `payout_requests`, `bandwidth_sessions`, and `nodes` are defined in §1.4 above. Redis layer for the hot path:

```
# Cached derived balance (write-through, reconciled nightly against Postgres SUM)
HSET wallet:{user_id}  available_minor 125000  pending_minor 8000  currency NGN

# Idempotency lock for payout submission (SETNX-style, TTL matches provider timeout)
SET payout_lock:{idempotency_key} "1" NX EX 120

# Eligible-node pool for gateway session assignment, scored by trust + capacity
ZADD eligible_nodes:ng-lagos 87.5 node:b3f1...

# Sliding-window rate limiter per enterprise client (token-bucket via Lua script)
INCR ratelimit:{client_id}:{minute_bucket}
EXPIRE ratelimit:{client_id}:{minute_bucket} 60
```

## 7. Automated VTU Payout Engine

### 7.1 Workflow

```
User requests redemption (or auto-redemption threshold hit, e.g., ₦1,000 accrued)
        │
        ▼
1. Validate: KYC status, available (non-pending) balance ≥ requested amount,
   no active fraud hold on account
        │
        ▼
2. Reserve funds: ledger debit from user_wallet → payout_clearing account
   (atomic, within a DB transaction; this is what makes the money "committed"
   even before the external provider call happens)
        │
        ▼
3. Create payout_requests row: status=pending, idempotency_key = uuid4()
        │
        ▼
4. Enqueue to Payout Orchestrator (Kafka topic `payout.requested`)
        │
        ▼
5. Orchestrator calls primary VTU provider:
   POST /vtu/airtime  { network, phone, amount, idempotency_key }
        │
        ├─ 200 success (sync) or webhook confirms later ─► status=success,
        │      ledger already reflects the debit; done.
        │
        ├─ Timeout / 5xx ─► retry with backoff (same idempotency_key),
        │      max 3 attempts, then failover to secondary provider
        │
        └─ Definitive failure (invalid phone, provider rejects) ─►
               status=failed → REVERSE the reservation (credit back to
               user_wallet from payout_clearing) → notify user
```

### 7.2 Sample orchestrator code (Node.js, illustrative)

```javascript
async function processPayoutRequest(payoutId) {
  const payout = await db.payoutRequests.findById(payoutId);
  if (payout.status !== 'pending') return; // idempotent re-entry guard

  const lockAcquired = await redis.set(
    `payout_lock:${payout.idempotency_key}`, '1', 'NX', 'EX', 120
  );
  if (!lockAcquired) return; // another worker already handling this

  await db.payoutRequests.update(payoutId, { status: 'processing' });

  try {
    const result = await withRetry(
      () => vtuProvider.primary.sendAirtime({
        network: payout.network,
        phone: payout.phone,
        amountMinorUnits: payout.amount_minor_units,
        idempotencyKey: payout.idempotency_key,
      }),
      { retries: 3, backoffMs: [1000, 4000, 10000] }
    );

    await finalizePayoutSuccess(payout, result.providerReference, 'vtu_provider_primary');
  } catch (primaryErr) {
    try {
      const fallback = await vtuProvider.secondary.sendAirtime({ /* same payload */ });
      await finalizePayoutSuccess(payout, fallback.providerReference, 'vtu_provider_secondary');
    } catch (secondaryErr) {
      await reversePayoutReservation(payout, secondaryErr.message);
    }
  } finally {
    await redis.del(`payout_lock:${payout.idempotency_key}`);
  }
}

async function finalizePayoutSuccess(payout, providerRef, provider) {
  await db.transaction(async (tx) => {
    await tx.payoutRequests.update(payout.id, {
      status: 'success', provider, provider_reference: providerRef,
    });
    // Ledger entry already booked at reservation time; nothing further to move —
    // the clearing-account debit is now "settled" rather than reversible.
  });
}

async function reversePayoutReservation(payout, reason) {
  await db.transaction(async (tx) => {
    await tx.ledgerEntries.insertPair({          // credit user back, debit clearing
      entryGroupId: uuid(),
      userAccountCredit: payout.amount_minor_units,
      clearingAccountDebit: payout.amount_minor_units,
      referenceType: 'payout_reversal',
      referenceId: payout.id,
    });
    await tx.payoutRequests.update(payout.id, { status: 'failed', failure_reason: reason });
  });
  await notifyUser(payout.user_id, 'payout_failed_refunded', { amount: payout.amount_minor_units });
}
```

### 7.3 Error handling & retry policy summary

| Failure type | Handling |
|---|---|
| Network timeout / 5xx from provider | Retry same request with identical idempotency key (safe — provider dedupes), exponential backoff, max 3 attempts |
| Provider explicitly rejects (invalid number, unsupported network) | No retry — fail fast, reverse reservation, surface actionable error to user (e.g., "check the phone number") |
| Provider timeout with unknown outcome (may have actually succeeded) | **Never blindly retry a plain top-up call here** — call the provider's `GET /transaction/{ref}/status` (requery) endpoint first; only retry the send if requery confirms "not found"/"failed" |
| All providers exhausted | Reverse reservation fully, notify user, flag for ops review if this is happening at unusual frequency (possible provider outage) |
| Webhook never arrives after apparent success | Nightly reconciliation job cross-checks provider transaction export against local `payout_requests`; auto-resolves matches, flags true mismatches |

---

## Appendix — Open Decisions to Validate Before Full Build-Out

1. **Payout rate benchmarking** — confirm current competitor $/GB payout rates (Honeygain/PacketStream/EarnApp-style) before fixing your own; these shift.
2. **Regulatory posture** — confirm whether operating a bandwidth-reselling marketplace in your target jurisdictions requires any telecom/ISP-adjacent licensing; consult local counsel, especially given the VTU/payments integration touches regulated payment-services territory.
3. **VTU provider selection** — run a side-by-side pilot (uptime, webhook reliability, dispute-resolution responsiveness) across at least two candidate aggregators before committing production payout volume to either.
