'use strict';
/**
 * Runs several payout requests through the orchestrator to demonstrate all
 * three real outcome paths: clean success, timeout-then-recovered-via-
 * requery (the bug fix), and full failure-with-reversal.
 *
 * Run: node demo.js
 */
const { requestPayout, processPayout, derivedBalance, bookLedgerPair, ledgerEntries, payoutRequests } = require('./orchestrator');

async function main() {
  const userId = 'user_demo_1';

  // Seed an initial "bandwidth earning" credit, same reference_type the
  // control plane's FinalizeSession would have booked.
  bookLedgerPair({ userAccount: userId, amount: 0, referenceType: 'seed', referenceId: 'seed' });
  // (bookLedgerPair debits the user by design — for seeding we credit directly)
  ledgerEntries.push({ entryGroupId: 'seed', accountId: userId, accountType: 'user_wallet', direction: 'credit', amount: 500000, referenceType: 'bandwidth_earning', referenceId: 'seed', createdAt: new Date() });

  console.log(`Starting balance for ${userId}: ${derivedBalance(userId)} (minor units)\n`);

  console.log('--- Payout attempt 1 (should succeed cleanly on primary or recover via requery) ---');
  const p1 = await requestPayout({ userId, amount: 5000, rail: 'vtu' });
  await processPayout(p1);
  console.log('  result:', JSON.stringify(payoutRequests.get(p1), null, 2));
  console.log(`  balance after: ${derivedBalance(userId)}\n`);

  console.log('--- Payout attempt 2 ---');
  const p2 = await requestPayout({ userId, amount: 3000, rail: 'vtu' });
  await processPayout(p2);
  console.log('  result:', JSON.stringify(payoutRequests.get(p2), null, 2));
  console.log(`  balance after: ${derivedBalance(userId)}\n`);

  console.log('--- Payout attempt 3 ---');
  const p3 = await requestPayout({ userId, amount: 2000, rail: 'vtu' });
  await processPayout(p3);
  console.log('  result:', JSON.stringify(payoutRequests.get(p3), null, 2));
  console.log(`  balance after: ${derivedBalance(userId)}\n`);

  console.log('--- Payout attempt 4 (insufficient balance case) ---');
  try {
    await requestPayout({ userId, amount: 10_000_000, rail: 'vtu' });
  } catch (err) {
    console.log(`  correctly rejected: ${err.message}\n`);
  }

  console.log('Final ledger (append-only, every row kept):');
  for (const e of ledgerEntries) {
    console.log(`  ${e.createdAt.toISOString()} ${e.accountId.padEnd(20)} ${e.direction.padEnd(6)} ${e.amount} (${e.referenceType})`);
  }
  console.log(`\nFinal derived balance for ${userId}: ${derivedBalance(userId)}`);
}

main();
