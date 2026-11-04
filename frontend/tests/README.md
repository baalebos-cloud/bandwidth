# Real browser UI tests

`ui-test.js` uses Playwright to actually load all three frontends in a
real Chromium instance and drive their real login flows against the live
backend — not a simulation of the API calls a browser would make, an
actual browser making them.

This distinction mattered in practice: earlier verification of these
frontends was done by replaying each one's API call sequence with `curl`
and checking the responses. That approach is useless for one entire class
of real bug — the JavaScript never running in the first place. A real
browser test caught two bugs that curl-based verification and
`node -c`-on-extracted-`<script>`-content both missed:

1. **A double-escaped apostrophe in `admin-console/index.html`**
   (`isn\\'t` instead of `isn't`) closed a JS string early, which was a
   *parse* error — it broke the entire `<script>` block, meaning every
   single event listener on the page silently failed to attach. The page
   loaded, looked fine, and did nothing when clicked. `curl` testing the
   backend API never touches this failure mode at all, because the bug
   was never in the API — it was in whether the frontend's JS could run
   as a program.
2. **Leftover unconditional `loadAll()` calls in `wallet-dashboard`** from
   before the OTP login flow was added — they fired on every page load
   regardless of auth state, hitting `/v1/wallet/` and
   `/v1/ledger/history/` with an empty user id before login completed.
   Correctly rejected with 401 by the backend (the auth is doing its
   job), but a real bug in the frontend: dead code from a rewrite that
   nobody exercised in the "does this page load" sense of testing.

Neither bug would show up from reading the code carefully, either — both
were found by watching what a real browser's JS engine and network tab
actually did, not by reasoning about what the code should do.

## Running it

Requires Playwright (`npm install playwright` — this environment has
Chromium pre-cached at `/opt/pw-browsers/`; a fresh environment needs
`npx playwright install chromium` too) and the full backend stack plus
all three frontends running (see the main README's "Running it" section).

```bash
ADMIN_API_KEY=<your admin key> node ui-test.js
```

Screenshots land in `/tmp/ui-test-screenshots/` at each key step of each
flow — useful for actually looking at what rendered, not just trusting
that an assertion passed.

## What it checks, per frontend

- **Admin console**: login gate blocks access with no/wrong key, the real
  key works, KYB queue and Nodes tab load real data (not stuck on
  "Loading…"), zero console/page errors during the flow.
- **Wallet dashboard**: OTP request → code (shown on-screen in this
  reference build, since no SMS provider is wired up) → verify → balance
  renders, zero console/page errors.
- **Enterprise portal**: signup → API key shown once via alert → KYB
  pending banner displays correctly, zero console/page errors.

## What it does NOT check

This is a happy-path-plus-a-few-negative-cases smoke test, not a
comprehensive test suite — it doesn't cover the redemption modal's full
flow, the admin console's reject-with-reason flow, session expiry
behavior, or concurrent-user scenarios. Treat a clean run as "the basic
flows genuinely work end-to-end in a real browser," not as "this frontend
code is fully verified."
