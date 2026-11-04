-- Bandwidth Monetization Platform — Core Schema (v2, corrected)
-- Append-only ledger. No mutable balance columns anywhere.

CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY, -- caller-supplied identifier (e.g. "user_demo_1"), not a generated UUID —
                          -- the wallet/session APIs treat this as an opaque external user key
    phone_number VARCHAR(20) UNIQUE,
    email VARCHAR(255),
    country_code CHAR(3) NOT NULL,
    kyc_status VARCHAR(20) NOT NULL DEFAULT 'unverified',
    preferred_payout_rail VARCHAR(20) NOT NULL DEFAULT 'vtu', -- vtu, mpesa, pix, stripe, bank
    status VARCHAR(20) NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS nodes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL REFERENCES users(id),
    device_fingerprint VARCHAR(128) NOT NULL,
    os_type VARCHAR(20) NOT NULL,
    public_key TEXT NOT NULL,
    isp_asn INTEGER,
    is_datacenter_asn BOOLEAN NOT NULL DEFAULT false,
    country_code CHAR(3),
    trust_score NUMERIC(5,2) NOT NULL DEFAULT 50.0,
    status VARCHAR(20) NOT NULL DEFAULT 'pending_verification',
    last_heartbeat_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(user_id, device_fingerprint)
);

CREATE TABLE IF NOT EXISTS enterprise_clients (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    company_name VARCHAR(255) NOT NULL,
    kyb_status VARCHAR(20) NOT NULL DEFAULT 'pending', -- pending, approved, rejected, suspended
    declared_use_case TEXT NOT NULL,
    tier VARCHAR(20) NOT NULL DEFAULT 'trial',
    rate_limit_gb_daily INTEGER NOT NULL DEFAULT 5,
    rejection_reason TEXT,
    api_key VARCHAR(64), -- demo: stored plain for local dev visibility; production should store api_key_hash only
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Append-only double-entry ledger. Spendable balance is always derived
-- (SUM(credit) - SUM(debit) per account_id) — never write a balance directly.
-- account_id is TEXT, not UUID: it holds either a user id (a caller-supplied
-- key, e.g. "user_demo_1") or one of the fixed platform account names
-- ("platform_revenue", "payout_clearing") — never a generated UUID itself.
CREATE TABLE IF NOT EXISTS ledger_entries (
    id BIGSERIAL PRIMARY KEY,
    entry_group_id TEXT NOT NULL,
    account_id TEXT NOT NULL,
    account_type VARCHAR(30) NOT NULL, -- user_wallet, platform_revenue, payout_clearing
    direction VARCHAR(6) NOT NULL CHECK (direction IN ('debit','credit')),
    amount_minor_units BIGINT NOT NULL CHECK (amount_minor_units > 0),
    currency CHAR(3) NOT NULL DEFAULT 'NGN',
    reference_type VARCHAR(30) NOT NULL, -- bandwidth_earning, payout, payout_reversal
    reference_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_ledger_account ON ledger_entries(account_id, created_at);

CREATE TABLE IF NOT EXISTS bandwidth_sessions (
    session_id UUID PRIMARY KEY,
    node_id UUID NOT NULL REFERENCES nodes(id),
    enterprise_client_id UUID NOT NULL REFERENCES enterprise_clients(id),
    session_token_hash VARCHAR(128) NOT NULL,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at TIMESTAMPTZ,
    gateway_confirmed_bytes BIGINT NOT NULL DEFAULT 0, -- source of truth for payment
    client_reported_bytes BIGINT,                       -- diagnostic / fraud signal only
    dest_category VARCHAR(30),
    status VARCHAR(20) NOT NULL DEFAULT 'open', -- open, finalized, rejected
    ml_fraud_score NUMERIC(6,5),   -- 0.0 (looks fine) to 1.0 (high risk), from the fraud-scoring service
    ml_fraud_flag BOOLEAN NOT NULL DEFAULT false
);

-- Every call to the fraud-scoring service is logged here, independent of
-- bandwidth_sessions, so the model's behavior is auditable even if a
-- session's own score field is later overwritten by a rescore.
CREATE TABLE IF NOT EXISTS fraud_score_log (
    id BIGSERIAL PRIMARY KEY,
    session_id UUID NOT NULL,
    node_id UUID NOT NULL,
    model_version VARCHAR(20) NOT NULL,
    score NUMERIC(6,5) NOT NULL,
    flagged BOOLEAN NOT NULL,
    features_json JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS payout_requests (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL REFERENCES users(id),
    amount_minor_units BIGINT NOT NULL,
    rail VARCHAR(20) NOT NULL,
    provider VARCHAR(30),
    provider_reference VARCHAR(100),
    idempotency_key VARCHAR(64) UNIQUE NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'pending', -- pending, reserved, submitted, success, failed, reversed
    failure_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS node_heartbeats (
    node_id UUID NOT NULL REFERENCES nodes(id),
    time TIMESTAMPTZ NOT NULL DEFAULT now(),
    battery_pct SMALLINT,
    is_charging BOOLEAN,
    network_type VARCHAR(10), -- wifi, cellular, ethernet
    app_version VARCHAR(20)
);

-- One-time codes for phone-based login. Never store the plain code at
-- rest longer than necessary to verify it — hashed, short-lived, single
-- use (consumed_at set on successful verification).
CREATE TABLE IF NOT EXISTS otp_requests (
    id BIGSERIAL PRIMARY KEY,
    phone_number VARCHAR(20) NOT NULL,
    code_hash VARCHAR(64) NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_otp_phone ON otp_requests(phone_number, created_at);
