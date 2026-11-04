# P2P Bandwidth Monetization Platform — Consolidated Blueprint & Roadmap (v2)

**Status:** This is the reconciled version. It keeps the sound architectural decisions from the original blueprint (gateway-authoritative metering, ledger-based wallets, anti-abuse-before-revenue sequencing), adopts the genuinely good ideas from the alternate draft (multi-region relay topology, GeoDNS, staged multi-rail payouts), and fixes the specific defects identified in review (payout idempotency/rollback bug, mutable-balance ledger, KYB sequencing, Sybil resistance gaps).

---

## 1. Design Principles (non-negotiable, everything else follows from these)

1. **The gateway pays, not the client.** A node is credited only for bytes the Exit Gateway itself relayed to a paying enterprise session — never for a client's self-reported figure. This is the single control that prevents the fabricated-earnings failure mode.
2. **No payout leaves the building until fraud review has had a window to catch it.** Earnings accrue immediately; withdrawability is delayed slightly and gated by KYB/fraud checks on the traffic that earned them.
3. **You vet who you sell to before you scale who sells to you.** Enterprise KYB gates and traffic-category filtering ship *before* open enterprise self-serve access — not after, as an afterthought once volume shows up.
4. **Money movement is append-only and idempotent, everywhere.** No mutable balance columns; no external payout call without a stable idempotency key and a requery-before-retry step.
5. **Wi-Fi/battery policy is a hard interrupt, not a polled setting.** A network-type change tears down active sessions immediately; there is no code path where a user discovers after the fact that cellular data was consumed.

---

## 2. System Architecture

```
                              ┌────────────────────────────┐
                              │      Enterprise Clients      │
                              │  (KYB-approved, tiered API)  │
                              └───────────────┬───────────────┘
                                              │ HTTPS / SOCKS5 (per-client API key + IP allowlist)
                                              ▼
                    ┌───────────────────────────────────────────────┐
                    │        Regional Ingress Gateway Cluster         │
                    │         (Go + Envoy, stateless, autoscaled)     │
                    │  - Enterprise auth & per-client rate limiting   │
                    │  - Domain/IP category filter (threat-intel feed)│
                    │  - RFC1918/loopback/link-local egress block     │
                    │  - AUTHORITATIVE byte counter per session       │
                    └───────────────┬─────────────────┬───────────────┘
                                    │                 │
                        mTLS/WireGuard tunnel     Regional pairing via GeoDNS
                                    │            (Route53 / Cloudflare LB)
                                    ▼                 ▼
              ┌──────────────────────────┐  ┌──────────────────────────┐
              │  Regional Relay: EU/US/   │  │  Regional Relay: Africa /  │
              │  Asia gateway nodes       │  │  pilot-region gateway     │
              └─────────────┬──────────────┘  └─────────────┬──────────────┘
                            │                               │
                            ▼                               ▼
              ┌─────────────────────────────────────────────────────┐
              │            Edge Client (sandboxed relay agent)         │
              │  Android (Kotlin+Rust core) / Desktop (Rust daemon)    │
              │  - Wi-Fi/ethernet-only enforcement (hard interrupt)    │
              │  - Battery/thermal policy                              │
              │  - No filesystem or LAN access (OS-level sandbox)      │
              │  - Signed, sequence-numbered local meter (diagnostic)  │
              └─────────────────────────┬───────────────────────────────┘
                                        │ gRPC heartbeat stream (signed telemetry)
                                        ▼
              ┌─────────────────────────────────────────────────────┐
              │              Central Control Plane                    │
              │  - Node registry, trust scoring, Sybil/ASN checks      │
              │  - Kafka/NATS event bus (metering + fraud signals)     │
              │  - Fraud engine (Python) — cross-checks gateway vs.    │
              │    client-reported bytes, flags anomalies              │
              │  - Append-only ledger (PostgreSQL, double-entry)       │
              │  - TimescaleDB — time-series metering + heartbeats     │
              └─────────────────────────┬───────────────────────────────┘
                                        ▼
              ┌─────────────────────────────────────────────────────┐
              │            Global Payout Orchestrator                  │
              │   (idempotent state machine, per-rail adapters)        │
              └───────────────┬────────────────┬────────────────────────┘
                              ▼                ▼
                ┌───────────────────┐ ┌──────────────────────────┐
                │  Pilot rail (NG):  │ │  Phase-2 rails:            │
                │  VTU aggregator     │ │  M-Pesa (KE), Pix (BR),    │
                │  (multi-provider    │ │  Stripe Connect / Wise      │
                │  failover) +         │ │  (developed markets),       │
                │  Paystack/Monnify    │ │  crypto rail — gated on      │
                │  for bank cash-out   │ │  separate AML/KYC legal      │
                │                     │ │  review before enabling      │
                └───────────────────┘ └──────────────────────────┘
```

**What changed from the alternate draft, and why:** the ingress gateway — not the edge client — is the authoritative meter; enterprise access sits behind KYB before it reaches the ingress layer at all; the payout orchestrator is a state machine with idempotency keys and requery-before-retry, not a single try/catch with a naive rollback; and the crypto payout rail is explicitly gated behind its own legal review rather than presented as a same-tier option alongside VTU/Stripe.

---

## 3. Database Design (corrected)

Append-only ledger, not a mutable balance column — this is the fix for the double-refund bug identified in review.

```sql
CREATE TABLE users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    phone_number VARCHAR(20) UNIQUE,
    email VARCHAR(255),
    country_code CHAR(3) NOT NULL,
    kyc_status VARCHAR(20) NOT NULL DEFAULT 'unverified',
    preferred_payout_rail VARCHAR(20) NOT NULL, -- 'vtu','mpesa','pix','stripe','bank'
    status VARCHAR(20) NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE nodes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id),
    device_fingerprint VARCHAR(128) NOT NULL,
    os_type VARCHAR(20) NOT NULL,
    public_key TEXT NOT NULL,
    isp_asn INTEGER,
    is_datacenter_asn BOOLEAN NOT NULL DEFAULT false, -- auto-flagged, blocks earning if true
    country_code CHAR(3),
    trust_score NUMERIC(5,2) NOT NULL DEFAULT 50.0,
    status VARCHAR(20) NOT NULL DEFAULT 'pending_verification',
    last_heartbeat_at TIMESTAMPTZ,
    UNIQUE(user_id, device_fingerprint)
);

-- Append-only double-entry ledger. No UPDATEs to balances, ever.
CREATE TABLE ledger_entries (
    id BIGSERIAL PRIMARY KEY,
    entry_group_id UUID NOT NULL,       -- ties a debit+credit pair together
    account_id UUID NOT NULL,
    account_type VARCHAR(30) NOT NULL,  -- user_wallet, platform_revenue, payout_clearing
    direction VARCHAR(6) NOT NULL CHECK (direction IN ('debit','credit')),
    amount_minor_units BIGINT NOT NULL CHECK (amount_minor_units > 0),
    currency CHAR(3) NOT NULL,
    reference_type VARCHAR(30) NOT NULL, -- bandwidth_earning, payout, payout_reversal
    reference_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_ledger_account ON ledger_entries(account_id, created_at);
-- Spendable balance is always derived: SUM(credit) - SUM(debit) per account_id,
-- cached in Redis with a nightly reconciliation job against this table.

CREATE TABLE payout_requests (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id),
    amount_minor_units BIGINT NOT NULL,
    rail VARCHAR(20) NOT NULL,           -- vtu, mpesa, pix, stripe, bank
    provider VARCHAR(30),
    provider_reference VARCHAR(100),
    idempotency_key VARCHAR(64) UNIQUE NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'pending', -- pending, reserved, submitted, success, failed, reversed
    failure_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Gateway is authoritative; client_reported is diagnostic only, never the pay basis
CREATE TABLE bandwidth_sessions (
    session_id UUID NOT NULL,
    node_id UUID NOT NULL,
    enterprise_client_id UUID NOT NULL,
    gateway_confirmed_bytes BIGINT NOT NULL DEFAULT 0, -- source of truth for payment
    client_reported_bytes BIGINT,                       -- fraud-signal comparison only
    dest_category VARCHAR(30),
    time TIMESTAMPTZ NOT NULL DEFAULT now()
) PARTITION BY RANGE (time); -- or TimescaleDB hypertable

CREATE TABLE enterprise_clients (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    company_name VARCHAR(255) NOT NULL,
    kyb_status VARCHAR(20) NOT NULL DEFAULT 'pending', -- pending, approved, rejected, suspended
    declared_use_case TEXT NOT NULL,
    tier VARCHAR(20) NOT NULL DEFAULT 'trial',
    rate_limit_gb_daily INTEGER NOT NULL DEFAULT 5,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Ingress gateway refuses traffic from any client not in 'approved' kyb_status.
```

---

## 4. Payout Engine (corrected — idempotent, requery-before-retry)

```typescript
async function processPayoutRequest(payoutId: string) {
  const payout = await db.payoutRequests.findById(payoutId);
  if (payout.status !== 'pending') return; // idempotent re-entry guard

  const lock = await redis.set(`payout_lock:${payout.idempotency_key}`, '1', 'NX', 'EX', 120);
  if (!lock) return;

  await db.payoutRequests.update(payoutId, { status: 'reserved' });
  // Reservation already happened at request-creation time via a ledger debit
  // into payout_clearing — see §3. This function only ever moves the request
  // forward or reverses that single reservation; it never mutates a balance directly.

  try {
    await db.payoutRequests.update(payoutId, { status: 'submitted' });

    const result = await callProviderWithRequery(payout, rails[payout.rail].primary);
    await finalizeSuccess(payout, result.providerReference, rails[payout.rail].primary.name);

  } catch (primaryErr) {
    try {
      const fallback = await callProviderWithRequery(payout, rails[payout.rail].secondary);
      await finalizeSuccess(payout, fallback.providerReference, rails[payout.rail].secondary.name);
    } catch (secondaryErr) {
      await reverseReservation(payout, secondaryErr.message);
    }
  } finally {
    await redis.del(`payout_lock:${payout.idempotency_key}`);
  }
}

// The fix for the double-refund bug: on ambiguous failure (timeout), ALWAYS
// requery transaction status before deciding to retry or refund.
async function callProviderWithRequery(payout, provider) {
  try {
    return await provider.send(payout); // includes payout.idempotency_key
  } catch (err) {
    if (err.code === 'TIMEOUT' || err.code === 'ECONNABORTED') {
      const status = await provider.queryStatus(payout.idempotency_key);
      if (status === 'success') return { providerReference: status.reference };
      if (status === 'not_found' || status === 'failed') throw err; // safe to retry/failover
      throw new Error('ambiguous_provider_state'); // do NOT retry — escalate to ops queue
    }
    throw err;
  }
}

async function finalizeSuccess(payout, providerRef, providerName) {
  await db.payoutRequests.update(payout.id, {
    status: 'success', provider: providerName, provider_reference: providerRef,
  });
  // No ledger mutation here — the clearing-account debit booked at reservation
  // time is now settled, not reversible.
}

async function reverseReservation(payout, reason) {
  await db.transaction(async (tx) => {
    await tx.ledgerEntries.insertPair({
      entryGroupId: uuid(),
      userAccountCredit: payout.amount_minor_units, // give it back
      clearingAccountDebit: payout.amount_minor_units,
      referenceType: 'payout_reversal',
      referenceId: payout.id,
    });
    await tx.payoutRequests.update(payout.id, { status: 'failed', failure_reason: reason });
  });
  await notifyUser(payout.user_id, 'payout_failed_refunded', { amount: payout.amount_minor_units });
}
```

Nightly reconciliation job still required regardless of the above: pull each provider's transaction export and diff against `payout_requests` — catches the cases where even a webhook and a requery both silently failed.

---

## 5. Anti-Abuse Sequencing (the ordering that was missing from the alternate draft)

Enterprise self-serve access must sit **behind**, not alongside, these gates:

1. **KYB before API key issuance.** `enterprise_clients.kyb_status = 'approved'` is a hard gateway-level check — traffic from a `pending` client is refused at the ingress layer, not just flagged for later review.
2. **Sybil/datacenter-ASN detection at node registration.** `nodes.is_datacenter_asn` auto-computed via IP-to-ASN lookup at registration; such nodes never enter the eligible-node pool regardless of what they claim about their connection type.
3. **Soft caps per subnet/ASN** to blunt Sybil farms of "residential-looking" nodes registered in bulk.
4. **Domain/IP category filtering** at the ingress gateway (threat-intel feed + your own denylist) — applied to every session regardless of enterprise tier, not just flagged accounts.
5. **Trust-score gating** — new nodes start capped at low session concurrency/bandwidth until they accumulate a heartbeat-consistency and gateway/client-byte-match history.

---

## 6. Phased Roadmap

| Phase | Duration | Scope | Exit criteria before next phase |
|---|---|---|---|
| **0 — Tunneling foundation** | Weeks 1–3 | Rust/Go relay daemon ↔ ingress gateway over WireGuard/mTLS; node registration + phone OTP; Wi-Fi-only hard-interrupt enforcement on the client. No enterprise traffic — internal test harness only. | Gateway-measured bytes match a controlled test load; network-change teardown verified to fire immediately, not on next heartbeat. |
| **1 — Metering & control plane** | Weeks 4–6 | Kafka/NATS event bus; TimescaleDB partitioned metering store; append-only ledger schema live; gateway becomes the sole basis for `bandwidth_sessions.gateway_confirmed_bytes`. | Client-reported vs. gateway-confirmed byte comparison job running and alerting on drift — proves the anti-spoofing control actually works, not just exists on paper. |
| **2 — Anti-abuse gate (was missing/late in the alternate draft — moved earlier here deliberately)** | Weeks 6–8, overlaps Phase 1 | KYB workflow + `enterprise_clients` gating live; datacenter-ASN auto-detection; domain/IP category filter wired into the ingress path; red-team SSRF/private-IP-pivot test suite in CI as a release gate. | No enterprise traffic is technically possible without `kyb_status = 'approved'`; red-team suite passing on every build. |
| **3 — Pilot payout integration (Nigeria)** | Weeks 8–10 | Payout orchestrator (idempotent, requery-before-retry, as in §4) live against a primary + secondary VTU aggregator; Paystack/Monnify for bank cash-out; onboard 100–500 pilot users; earnings accrue with a short fraud-review holdback before becoming withdrawable. | Nightly reconciliation shows zero unexplained ledger/provider mismatches over a 2-week window; no double-payout incidents. |
| **4 — Android client** | Weeks 10–14 | Kotlin shell + shared Rust core; `VpnService`-scoped tunnel; battery/Doze handling; Play Store policy review budgeted in. | Passes store review; battery/network hard-interrupt behavior verified on-device, not just in emulator. |
| **5 — Controlled enterprise onboarding** | Weeks 12–16, overlaps Phase 4 | Open KYB-gated enterprise signup at small volume; per-client rate limits and tiered pricing; behavioral monitoring on blocked-category attempt rates. | Sustained low fraud/abuse-flag rate at real (not synthetic) enterprise volume before widening access. |
| **6 — Multi-region relay expansion** | Weeks 16–22 | Deploy relay clusters in additional regions (adopt the alternate draft's GeoDNS pairing idea here — it's a good scaling pattern once there's real geographic demand); regional read-replicas. | Latency/SLA targets met per region; no regression in metering integrity across regions. |
| **7 — Additional payout rails** | Ongoing, starts ~Week 18 | Add M-Pesa (Kenya), Pix (Brazil), Stripe Connect/Wise (developed markets) — each behind its own compliance review, sequenced by validated demand, not all at once. **Crypto rail explicitly deferred** until a separate AML/KYC legal review is complete — do not ship it as a same-tier checkbox next to VTU/Stripe. | Legal sign-off per rail before enabling; reconciliation job extended to cover each new rail before it goes live. |
| **8 — Scale & hardening** | Ongoing | ML-based fraud scoring to replace rules engine; service mesh + zero-trust internal networking; migrate metering store to ClickHouse if ingest volume demands it; SOC2-style controls if enterprise customers require them. | — |

---

## 7. Key Decisions Still Open (validate before committing further build time)

1. **Payout rate benchmarking** against current competitor $/GB rates — the $0.30–0.50/GB figure from the alternate draft is a reasonable starting hypothesis, not a validated number yet.
2. **Crypto payout rail** — separate legal review required before it appears in any customer-facing roadmap, given the stricter AML/KYC surface versus VTU/fiat rails.
3. **Regulatory posture per launch jurisdiction** — confirm telecom/ISP-adjacent licensing exposure and payment-services classification before expanding beyond the pilot region.
4. **VTU provider panel** — pilot with at least two providers concurrently (not sequential fallback-only) to get real comparative uptime/webhook-reliability data before locking in a primary.
5. **iOS support** — still deliberately out of scope; revisit only via a `PacketTunnelProvider` network-extension approach with explicit App Store policy review, not before.
