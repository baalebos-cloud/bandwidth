'use strict';
/**
 * Payout orchestrator — plain Node.js, no dependencies, so it runs with
 * nothing but `node`. This is the same design as the corrected orchestrator
 * from the blueprint: idempotency keys, requery-before-retry on ambiguous
 * failures, automatic failover to a secondary provider, and an explicit
 * reversal path that credits the user back rather than silently losing
 * their request.
 *
 * In-memory store here stands in for Postgres — every write below maps to
 * a specific table/column in db/schema.sql, called out in comments.
 */

const crypto = require('crypto');

// ---------------------------------------------------------------------
// In-memory ledger + payout_requests (== db/schema.sql tables)
// ---------------------------------------------------------------------
const ledgerEntries = []; // == ledger_entries
const payoutRequests = new Map(); // == payout_requests, keyed by id
const idempotencyLocks = new Set(); // == a Redis SETNX lock in production

function derivedBalance(accountId) {
  return ledgerEntries
    .filter(e => e.accountId === accountId)
    .reduce((sum, e) => sum + (e.direction === 'credit' ? e.amount : -e.amount), 0);
}

function bookLedgerPair({ userAccount, amount, referenceType, referenceId }) {
  const groupId = crypto.randomUUID();
  ledgerEntries.push(
    { entryGroupId: groupId, accountId: userAccount, accountType: 'user_wallet', direction: 'debit', amount, referenceType, referenceId, createdAt: new Date() },
    { entryGroupId: groupId, accountId: 'payout_clearing', accountType: 'payout_clearing', direction: 'credit', amount, referenceType, referenceId, createdAt: new Date() },
  );
}

function reverseLedgerPair({ userAccount, amount, referenceId }) {
  const groupId = crypto.randomUUID();
  ledgerEntries.push(
    { entryGroupId: groupId, accountId: userAccount, accountType: 'user_wallet', direction: 'credit', amount, referenceType: 'payout_reversal', referenceId, createdAt: new Date() },
    { entryGroupId: groupId, accountId: 'payout_clearing', accountType: 'payout_clearing', direction: 'debit', amount, referenceType: 'payout_reversal', referenceId, createdAt: new Date() },
  );
}

// ---------------------------------------------------------------------
// Mock VTU / Paystack providers — deliberately simulate the three real
// failure modes the orchestrator has to handle: clean success, definitive
// rejection, and an AMBIGUOUS timeout (the case that caused the
// double-refund bug in the earlier draft this design replaces).
// ---------------------------------------------------------------------
function makeMockProvider(name, { failureRate = 0, ambiguousRate = 0 } = {}) {
  const completedTransactions = new Map(); // idempotencyKey -> outcome, simulates the provider's own dedupe

  return {
    name,
    async send(payout) {
      if (completedTransactions.has(payout.idempotencyKey)) {
        // Provider-side idempotency: a retried call with the same key
        // returns the ORIGINAL outcome, never processes twice.
        return completedTransactions.get(payout.idempotencyKey);
      }

      const roll = Math.random();
      if (roll < ambiguousRate) {
        // Simulate: the provider actually processed it, but the response
        // never made it back to us (classic timeout ambiguity).
        const outcome = { status: 'success', providerReference: `${name}-${crypto.randomUUID()}` };
        completedTransactions.set(payout.idempotencyKey, outcome);
        const err = new Error('ETIMEDOUT');
        err.code = 'TIMEOUT';
        throw err;
      }
      if (roll < ambiguousRate + failureRate) {
        const err = new Error(`${name} rejected the request (invalid destination)`);
        err.code = 'REJECTED';
        throw err;
      }
      const outcome = { status: 'success', providerReference: `${name}-${crypto.randomUUID()}` };
      completedTransactions.set(payout.idempotencyKey, outcome);
      return outcome;
    },
    async queryStatus(idempotencyKey) {
      const outcome = completedTransactions.get(idempotencyKey);
      if (!outcome) return 'not_found';
      return outcome.status; // 'success'
    },
  };
}

const rails = {
  vtu: {
    primary: makeMockProvider('vtu_reseller_existing', { ambiguousRate: 0.3 }), // the user's existing reseller API — modeled as flakier
    secondary: makeMockProvider('vtu_aggregator_b', { failureRate: 0.1 }),
  },
};

// ---------------------------------------------------------------------
// Orchestrator — the actual state machine
// ---------------------------------------------------------------------
async function requestPayout({ userId, amount, rail }) {
  const id = crypto.randomUUID();
  const idempotencyKey = crypto.randomUUID();

  const balance = derivedBalance(userId);
  if (balance < amount) {
    throw new Error(`insufficient balance: have ${balance}, need ${amount}`);
  }

  // Reserve funds immediately — this is what makes the request "committed"
  // even before any external provider call happens.
  bookLedgerPair({ userAccount: userId, amount, referenceType: 'payout', referenceId: id });

  const payout = { id, userId, amount, rail, idempotencyKey, status: 'reserved', createdAt: new Date() };
  payoutRequests.set(id, payout);
  return id;
}

async function callProviderWithRequery(provider, payout) {
  try {
    return await provider.send(payout);
  } catch (err) {
    if (err.code === 'TIMEOUT') {
      // THE FIX: never blindly retry on an ambiguous failure. Ask the
      // provider what actually happened first.
      const status = await provider.queryStatus(payout.idempotencyKey);
      if (status === 'success') {
        console.log(`  [requery] ${provider.name}: transaction actually succeeded despite timeout — NOT retrying, NOT refunding`);
        return { status: 'success', providerReference: `${provider.name}-recovered-${payout.idempotencyKey}` };
      }
      if (status === 'not_found' || status === 'failed') {
        throw err; // genuinely safe to retry/failover
      }
      throw new Error('ambiguous_provider_state — escalate to ops, do not auto-retry');
    }
    throw err;
  }
}

async function processPayout(payoutId) {
  const payout = payoutRequests.get(payoutId);
  if (!payout || payout.status !== 'reserved') return;

  if (idempotencyLocks.has(payout.idempotencyKey)) {
    console.log(`  lock held for ${payout.idempotencyKey} — another worker already processing, skipping`);
    return;
  }
  idempotencyLocks.add(payout.idempotencyKey);

  payout.status = 'submitted';
  const { primary, secondary } = rails[payout.rail];

  try {
    const result = await callProviderWithRequery(primary, payout);
    finalizeSuccess(payout, result, primary.name);
  } catch (primaryErr) {
    console.log(`  primary (${primary.name}) failed: ${primaryErr.message} — failing over`);
    try {
      const result = await callProviderWithRequery(secondary, payout);
      finalizeSuccess(payout, result, secondary.name);
    } catch (secondaryErr) {
      console.log(`  secondary (${secondary.name}) also failed: ${secondaryErr.message} — reversing`);
      reversePayout(payout, secondaryErr.message);
    }
  } finally {
    idempotencyLocks.delete(payout.idempotencyKey);
  }
}

function finalizeSuccess(payout, result, providerName) {
  payout.status = 'success';
  payout.provider = providerName;
  payout.providerReference = result.providerReference;
  // No ledger mutation here — the reservation booked at request time is now
  // settled, not reversible. This mirrors the real orchestrator design.
}

function reversePayout(payout, reason) {
  reverseLedgerPair({ userAccount: payout.userId, amount: payout.amount, referenceId: payout.id });
  payout.status = 'failed';
  payout.failureReason = reason;
}

module.exports = { requestPayout, processPayout, payoutRequests, derivedBalance, bookLedgerPair, ledgerEntries };
