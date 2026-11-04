'use strict';
/**
 * Real adapter for the VTpass API (https://vtpass.com/documentation) — a
 * live, widely-used Nigerian VTU aggregator. This implements the actual
 * documented contract, not an approximation:
 *
 *  - request_id must be 12+ chars, numeric, first 12 chars = today's date
 *    in YYYYMMDDHHII (Africa/Lagos time) — VTpass rejects malformed ones.
 *  - Purchase:  POST {base}/api/pay      { request_id, serviceID, amount, phone }
 *  - Requery:   POST {base}/api/requery  { request_id }
 *  - Auth headers: api-key, secret-key (server-to-server), or public-key
 *    for GET-only lookups.
 *  - Success is code "000" AND response_description "TRANSACTION SUCCESSFUL"
 *    — VTpass can return HTTP 200 with a non-success code, so the HTTP
 *    status alone is not enough to tell success from failure.
 *
 * This talks to the REAL VTpass API when VTPASS_API_KEY/VTPASS_SECRET_KEY
 * are set — sandbox by default (VTPASS_ENV=live to go live). No live
 * credentials were available to test this from the sandbox, so treat the
 * request/response shape as correct-per-documentation and verify against
 * the real sandbox before routing production payouts through it.
 */
const https = require('https');

const BASE_URL = (process.env.VTPASS_ENV === 'live')
  ? 'https://vtpass.com'
  : 'https://sandbox.vtpass.com';

const SERVICE_ID_BY_NETWORK = { MTN: 'mtn', AIRTEL: 'airtel', GLO: 'glo', '9MOBILE': 'etisalat' };

// VTpass's mandated request_id shape: first 12 chars numeric, date-prefixed,
// in Africa/Lagos time (UTC+1, no DST) — built without a timezone library
// dependency since the offset is fixed.
function buildRequestID() {
  const lagos = new Date(Date.now() + 60 * 60 * 1000); // UTC+1, fixed offset
  const pad = (n) => String(n).padStart(2, '0');
  const prefix = `${lagos.getUTCFullYear()}${pad(lagos.getUTCMonth() + 1)}${pad(lagos.getUTCDate())}${pad(lagos.getUTCHours())}${pad(lagos.getUTCMinutes())}`;
  const suffix = Math.random().toString(36).slice(2, 10);
  return prefix + suffix; // 12 numeric + alnum suffix, matches documented format
}

function vtpassRequest(path, body) {
  return new Promise((resolve, reject) => {
    const apiKey = process.env.VTPASS_API_KEY;
    const secretKey = process.env.VTPASS_SECRET_KEY;
    if (!apiKey || !secretKey) {
      const err = new Error('VTPASS_API_KEY / VTPASS_SECRET_KEY not configured — cannot call the real VTpass API');
      err.code = 'NOT_CONFIGURED';
      return reject(err);
    }

    const data = JSON.stringify(body);
    const url = new URL(BASE_URL + path);
    const req = https.request(
      {
        hostname: url.hostname, path: url.pathname, method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'Content-Length': Buffer.byteLength(data),
          'api-key': apiKey,
          'secret-key': secretKey,
        },
        timeout: 15000,
      },
      (res) => {
        let chunks = '';
        res.on('data', (c) => (chunks += c));
        res.on('end', () => {
          try { resolve({ httpStatus: res.statusCode, body: JSON.parse(chunks) }); }
          catch (e) { reject(new Error('VTpass returned non-JSON response: ' + chunks.slice(0, 200))); }
        });
      }
    );
    req.on('timeout', () => { req.destroy(); const e = new Error('ETIMEDOUT'); e.code = 'TIMEOUT'; reject(e); });
    req.on('error', (e) => { e.code = e.code || 'NETWORK_ERROR'; reject(e); });
    req.write(data);
    req.end();
  });
}

const vtpassProvider = {
  name: 'vtpass_live',

  async send(payout) {
    const serviceID = SERVICE_ID_BY_NETWORK[(payout.network || '').toUpperCase()];
    if (!serviceID) throw Object.assign(new Error(`unsupported network: ${payout.network}`), { code: 'REJECTED' });

    const requestId = buildRequestID();
    let result;
    try {
      result = await vtpassRequest('/api/pay', {
        request_id: requestId,
        serviceID,
        amount: (payout.amount / 100).toFixed(2), // payout.amount is minor units (kobo); VTpass wants naira
        phone: payout.phone,
      });
    } catch (err) {
      if (err.code === 'TIMEOUT') { err.vtpassRequestId = requestId; throw err; } // caller will requery on this
      throw err;
    }

    const { code, response_description } = result.body || {};
    if (code === '000' && response_description === 'TRANSACTION SUCCESSFUL') {
      return { status: 'success', providerReference: requestId };
    }
    // VTpass returned a definitive non-success code — not an ambiguous
    // timeout, so this is safe to treat as a real rejection.
    const err = new Error(`VTpass rejected: code=${code} desc=${response_description}`);
    err.code = 'REJECTED';
    err.vtpassRequestId = requestId;
    throw err;
  },

  // Called by the orchestrator's requery-before-retry logic on an ambiguous
  // timeout — this is the exact mechanism the payout engine's design
  // depends on, now wired to VTpass's real /api/requery endpoint.
  async queryStatus(idempotencyKeyOrVtpassRequestId) {
    try {
      const result = await vtpassRequest('/api/requery', { request_id: idempotencyKeyOrVtpassRequestId });
      const status = result.body?.content?.transactions?.status;
      if (status === 'delivered') return 'success';
      if (status === 'pending') return 'pending'; // caller should NOT retry yet on this
      if (status === 'failed' || status === 'reversed') return 'failed';
      return 'not_found';
    } catch (err) {
      return 'not_found'; // can't confirm — caller treats conservatively
    }
  },
};

module.exports = { vtpassProvider, buildRequestID, SERVICE_ID_BY_NETWORK };
