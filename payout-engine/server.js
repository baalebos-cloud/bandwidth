'use strict';
/**
 * Payout engine HTTP server. Same idempotent orchestrator design as before
 * (orchestrator.js / demo.js), but now talks to the control-plane's ledger
 * over HTTP for reserve/reverse instead of keeping its own — one ledger,
 * one source of truth, which is what the corrected design always called
 * for. payout_requests themselves still live here, matching the schema's
 * intent that the payout engine owns that table.
 */
const http = require('http');
const crypto = require('crypto');

const CONTROL_PLANE_URL = process.env.CONTROL_PLANE_URL || 'http://127.0.0.1:8080';
const PORT = process.env.PORT || 8081;
const INTERNAL_SERVICE_KEY = process.env.INTERNAL_SERVICE_KEY || 'DEMO-ONLY-INSECURE-INTERNAL-KEY-CHANGE-ME';
const AUTH_SECRET = process.env.AUTH_SECRET || 'DEMO-ONLY-INSECURE-SECRET-CHANGE-ME';
const ADMIN_API_KEY = process.env.ADMIN_API_KEY || '';

if (!process.env.INTERNAL_SERVICE_KEY || !process.env.AUTH_SECRET) {
  console.log('WARNING: INTERNAL_SERVICE_KEY and/or AUTH_SECRET not set — using hardcoded demo defaults that MUST match the control-plane\'s own (also-demo) defaults. Set both before deploying beyond localhost.');
}

const payoutRequests = new Map(); // == payout_requests table
const idempotencyLocks = new Set();

function controlPlaneCall(path, body) {
  return new Promise((resolve, reject) => {
    const data = JSON.stringify(body);
    const url = new URL(CONTROL_PLANE_URL + path);
    const req = http.request(
      { hostname: url.hostname, port: url.port, path: url.pathname, method: 'POST',
        headers: {
          'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(data),
          'Authorization': `Bearer ${INTERNAL_SERVICE_KEY}`, // authenticates THIS SERVICE to the control plane
        } },
      (res) => {
        let chunks = '';
        res.on('data', (c) => (chunks += c));
        res.on('end', () => {
          try { resolve({ status: res.statusCode, body: JSON.parse(chunks || '{}') }); }
          catch (e) { resolve({ status: res.statusCode, body: {} }); }
        });
      }
    );
    req.on('error', reject);
    req.write(data);
    req.end();
  });
}

function controlPlaneGet(path) {
  return new Promise((resolve, reject) => {
    const url = new URL(CONTROL_PLANE_URL + path);
    http.get({ hostname: url.hostname, port: url.port, path: url.pathname + url.search }, (res) => {
      let chunks = '';
      res.on('data', (c) => (chunks += c));
      res.on('end', () => {
        try { resolve({ status: res.statusCode, body: JSON.parse(chunks || '{}') }); }
        catch (e) { resolve({ status: res.statusCode, body: {} }); }
      });
    }).on('error', reject);
  });
}

// ---------------------------------------------------------------------
// Verifies the SAME user tokens the control plane issues (via
// /v1/auth/verify-otp), independently, using the shared AUTH_SECRET —
// this service doesn't need to call back to the control plane just to
// know who's asking, matching normal microservice practice.
// ---------------------------------------------------------------------
function verifyUserToken(token) {
  const parts = (token || '').split('.');
  if (parts.length !== 3) return null;
  const [header, payload, sig] = parts;
  const expectedSigB64 = crypto.createHmac('sha256', AUTH_SECRET).update(`${header}.${payload}`).digest('base64url');
  if (!timingSafeEqualStr(expectedSigB64, sig)) return null;
  let claims;
  try { claims = JSON.parse(Buffer.from(payload, 'base64url').toString('utf8')); }
  catch (e) { return null; }
  if (!claims.exp || Date.now() / 1000 > claims.exp) return null;
  return claims.user_id;
}

function timingSafeEqualStr(a, b) {
  const bufA = Buffer.from(a);
  const bufB = Buffer.from(b);
  if (bufA.length !== bufB.length) return false;
  return crypto.timingSafeEqual(bufA, bufB);
}

// ---------------------------------------------------------------------
// Mock providers — same three failure modes as the standalone demo:
// clean success, definitive rejection, ambiguous timeout.
// ---------------------------------------------------------------------
function makeMockProvider(name, { failureRate = 0, ambiguousRate = 0 } = {}) {
  const completedTransactions = new Map();
  return {
    name,
    async send(payout) {
      if (completedTransactions.has(payout.idempotencyKey)) return completedTransactions.get(payout.idempotencyKey);
      const roll = Math.random();
      if (roll < ambiguousRate) {
        const outcome = { status: 'success', providerReference: `${name}-${crypto.randomUUID()}` };
        completedTransactions.set(payout.idempotencyKey, outcome);
        const err = new Error('ETIMEDOUT'); err.code = 'TIMEOUT'; throw err;
      }
      if (roll < ambiguousRate + failureRate) {
        const err = new Error(`${name} rejected the request (invalid destination)`); err.code = 'REJECTED'; throw err;
      }
      const outcome = { status: 'success', providerReference: `${name}-${crypto.randomUUID()}` };
      completedTransactions.set(payout.idempotencyKey, outcome);
      return outcome;
    },
    async queryStatus(idempotencyKey) {
      const outcome = completedTransactions.get(idempotencyKey);
      return outcome ? outcome.status : 'not_found';
    },
  };
}

// Named after the user's actual existing VTU reseller integration —
// modeled as moderately flaky to exercise the requery path realistically.
// Set VTPASS_API_KEY + VTPASS_SECRET_KEY to route real payouts through the
// actual VTpass API instead of the mock — see providers/vtpass.js.
const { vtpassProvider } = require('./providers/vtpass');

const useRealVTpass = !!(process.env.VTPASS_API_KEY && process.env.VTPASS_SECRET_KEY);

const rails = {
  vtu: {
    primary: useRealVTpass ? vtpassProvider : makeMockProvider('vtu_reseller_existing', { ambiguousRate: 0.35 }),
    secondary: makeMockProvider('vtu_aggregator_backup', { failureRate: 0.1 }),
  },
};
console.log(useRealVTpass
  ? `VTPASS_API_KEY set — routing 'vtu' rail through the REAL VTpass API (${process.env.VTPASS_ENV === 'live' ? 'LIVE' : 'sandbox'})`
  : `No VTPASS_API_KEY set — 'vtu' rail uses the mock provider. Set VTPASS_API_KEY/VTPASS_SECRET_KEY to go live.`);

async function callProviderWithRequery(provider, payout) {
  try {
    return await provider.send(payout);
  } catch (err) {
    if (err.code === 'TIMEOUT') {
      // Use the provider's OWN reference for requery when it gave one
      // (VTpass's request_id is not the same string as our internal
      // idempotencyKey) — falling back to idempotencyKey for providers
      // that key their own dedupe off what we sent them, like the mocks.
      const requeryRef = err.vtpassRequestId || payout.idempotencyKey;
      const status = await provider.queryStatus(requeryRef);
      if (status === 'success') return { status: 'success', providerReference: `${provider.name}-recovered-${requeryRef}` };
      if (status === 'not_found' || status === 'failed') throw err;
      throw new Error('ambiguous_provider_state'); // e.g. VTpass says "pending" — do not retry or refund yet
    }
    throw err;
  }
}

async function processPayout(payout) {
  if (idempotencyLocks.has(payout.idempotencyKey)) return;
  idempotencyLocks.add(payout.idempotencyKey);
  payout.status = 'submitted';
  const { primary, secondary } = rails[payout.rail];

  try {
    const result = await callProviderWithRequery(primary, payout);
    payout.status = 'success'; payout.provider = primary.name; payout.providerReference = result.providerReference;
  } catch (primaryErr) {
    try {
      const result = await callProviderWithRequery(secondary, payout);
      payout.status = 'success'; payout.provider = secondary.name; payout.providerReference = result.providerReference;
    } catch (secondaryErr) {
      await controlPlaneCall('/v1/ledger/reverse', { UserID: payout.userId, Amount: payout.amount, ReferenceID: payout.id });
      payout.status = 'failed'; payout.failureReason = secondaryErr.message;
    }
  } finally {
    idempotencyLocks.delete(payout.idempotencyKey);
  }
}

// ---------------------------------------------------------------------
// HTTP API
// ---------------------------------------------------------------------
function withCORS(handler) {
  return (req, res) => {
    res.setHeader('Access-Control-Allow-Origin', '*');
    res.setHeader('Access-Control-Allow-Methods', 'GET, POST, OPTIONS');
    res.setHeader('Access-Control-Allow-Headers', 'Content-Type, Authorization');
    if (req.method === 'OPTIONS') { res.writeHead(204); res.end(); return; }
    handler(req, res);
  };
}

function readBody(req) {
  return new Promise((resolve) => {
    let data = '';
    req.on('data', (c) => (data += c));
    req.on('end', () => { try { resolve(JSON.parse(data || '{}')); } catch { resolve({}); } });
  });
}

function sendJSON(res, status, body) {
  res.writeHead(status, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(body));
}

async function handleRequest(req, res) {
  const url = new URL(req.url, `http://localhost:${PORT}`);

  if (req.method === 'POST' && url.pathname === '/v1/payouts') {
    const authHeader = req.headers['authorization'] || '';
    const token = authHeader.startsWith('Bearer ') ? authHeader.slice(7) : '';
    const authedUserId = verifyUserToken(token);
    if (!authedUserId) return sendJSON(res, 401, { error: 'missing or invalid bearer token' });

    const body = await readBody(req);
    const { amount, rail, phone, network } = body;
    // userId comes from the verified token now, never from the request
    // body — a caller can no longer trigger a payout for someone else's
    // wallet just by naming their user id.
    const userId = authedUserId;
    if (!amount || !rail) return sendJSON(res, 400, { error: 'amount, rail required' });

    const id = crypto.randomUUID();
    const idempotencyKey = crypto.randomUUID();

    let reserve;
    try {
      reserve = await controlPlaneCall('/v1/ledger/reserve', { UserID: userId, Amount: amount, ReferenceID: id });
    } catch (err) {
      // Control plane unreachable — fail the request cleanly. No funds were
      // ever reserved, so there's nothing to roll back. A crashed payout
      // process is worse than a rejected request.
      return sendJSON(res, 503, { error: 'control plane unreachable: ' + err.message });
    }
    if (reserve.status !== 200) return sendJSON(res, 402, { error: reserve.body.error || 'insufficient balance' });

    const payout = { id, userId, amount, rail, phone, network, idempotencyKey, status: 'reserved', createdAt: new Date().toISOString() };
    payoutRequests.set(id, payout);

    await processPayout(payout);
    return sendJSON(res, 201, payout);
  }

  if (req.method === 'GET' && url.pathname === '/v1/payouts') {
    const userId = url.searchParams.get('userId');
    if (userId) {
      // Self-service view: the caller must be that same authenticated user.
      const authHeader = req.headers['authorization'] || '';
      const token = authHeader.startsWith('Bearer ') ? authHeader.slice(7) : '';
      const authedUserId = verifyUserToken(token);
      if (!authedUserId || authedUserId !== userId) {
        return sendJSON(res, 403, { error: 'cannot view another user\'s payouts' });
      }
    } else {
      // Unfiltered list: admin-only (this is the ops reconciliation view).
      const authHeader = req.headers['authorization'] || '';
      const key = authHeader.startsWith('Bearer ') ? authHeader.slice(7) : '';
      if (!ADMIN_API_KEY || key !== ADMIN_API_KEY) {
        return sendJSON(res, 401, { error: 'admin key required for the unfiltered payout list' });
      }
    }
    let list = [...payoutRequests.values()];
    if (userId) list = list.filter((p) => p.userId === userId);
    list.sort((a, b) => new Date(b.createdAt) - new Date(a.createdAt));
    return sendJSON(res, 200, list);
  }

  if (req.method === 'GET' && url.pathname === '/v1/wallet-passthrough') {
    // convenience passthrough so the wallet dashboard only needs one API base
    const userId = url.searchParams.get('userId');
    const result = await controlPlaneGet(`/v1/wallet/${userId}`);
    return sendJSON(res, result.status, result.body);
  }

  sendJSON(res, 404, { error: 'not found' });
}

const server = http.createServer(withCORS((req, res) => {
  handleRequest(req, res).catch((err) => {
    console.error('unhandled error in request handler:', err.message);
    if (!res.headersSent) sendJSON(res, 502, { error: 'internal error: ' + err.message });
  });
}));

server.listen(PORT, () => console.log(`payout-engine listening on :${PORT}, control-plane at ${CONTROL_PLANE_URL}`));
