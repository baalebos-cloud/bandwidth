# Backend test suite

`backend-test.sh` runs 30 real assertions against the live backend stack
— no mocking, real HTTP calls to real running services. It's the
backend-side counterpart to `frontend/tests/ui-test.js` (which drives a
real browser against the frontends).

## What it checks

- Service reachability (control-plane, fraud-service, payout-engine)
- Admin auth (no key / wrong key / real key)
- The full enterprise lifecycle (register → pre-approval rejection →
  approve → whoami; a separate rejection path)
- OTP login (valid code, wrong code, single-use enforcement)
- Node registration requires and correctly uses a user token
- Wallet ownership enforcement (own wallet vs. someone else's)
- A real session creation + relay through the gateway, checked against
  an actual wallet balance increase — not just an HTTP status code
- Fraud scoring correctly distinguishes an honest session from a
  fraud-shaped one it wasn't specifically trained on
- Payout auth and completion
- Internal service-to-service auth (gateway/payout-engine ↔ control-plane)

## Running it

Requires the full backend stack already running — control-plane (:8080),
gateway (:9443), payout-engine (:8081), fraud-service (:8082) — with
matching `ADMIN_API_KEY` / `INTERNAL_SERVICE_KEY` / `AUTH_SECRET`
exported (see the main README's "Running it" section). Then:

```bash
ADMIN_API_KEY=<your admin key> bash scripts/backend-test.sh
```

Exits 0 with all passes, 1 with a summary of any failures.

## A real lesson from running this against this sandbox

The single most common test failure encountered while building this
script had nothing to do with the code being tested — it was
**forgetting to (re-)start every backend service in the same shell
invocation**, since background processes in this build environment don't
survive across separate tool calls, and a service (like fraud-service)
that silently failed to bind on one attempt can look fine on a
follow-up manual check while still being down when the actual test
suite runs moments later. The fix that matters: always verify every
service is genuinely reachable (a real HTTP 200, not just "I started it
a moment ago") immediately before running the suite, not after — see the
pre-flight check pattern in the main README's testing instructions.
