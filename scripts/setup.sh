#!/usr/bin/env bash
# One-shot setup for the full stack. Safe to re-run — schema.sql and every
# step here is idempotent. Run this from the repo root:
#   bash scripts/setup.sh
#
# It does NOT start a persistent supervisor — services are backgrounded
# with nohup for local dev. Run this in one continuous shell session; see
# the main README if you're doing this inside a sandboxed/ephemeral
# environment where background processes don't survive across separate
# tool/terminal invocations.
set -euo pipefail
cd "$(dirname "$0")/.."

# Portable across environments with sudo (typical dev machines) and
# environments with only su (some containers/sandboxes, including the
# one this project was built and tested in).
run_as_postgres() {
  if command -v sudo >/dev/null 2>&1; then
    sudo -u postgres "$@"
  else
    # su -c takes a single string, re-parsed by a shell — "$*" would
    # flatten and lose the quoting on any argument containing spaces
    # (e.g. a -c "SQL text here" argument), so rebuild it with each
    # argument correctly re-escaped instead.
    su postgres -c "$(printf '%q ' "$@")"
  fi
}

echo "=== 1. Database ==="
service postgresql start 2>/dev/null || true
sleep 2
run_as_postgres psql -c "ALTER USER postgres PASSWORD 'demo_pw_local_only';" 2>/dev/null || true
run_as_postgres psql -c "CREATE DATABASE bandwidth_platform;" 2>/dev/null || true
run_as_postgres psql -d bandwidth_platform -c "CREATE EXTENSION IF NOT EXISTS pgcrypto;"
run_as_postgres psql -d bandwidth_platform -f db/schema.sql

echo ""
echo "=== 2. Auth secrets ==="
if [ -f .env ]; then
  echo ".env already exists — using it (delete it first to regenerate secrets)"
  export $(grep -v '^#' .env | xargs)
else
  echo "Generating real random secrets into .env"
  cat > .env << EOF
ADMIN_API_KEY=$(openssl rand -hex 24)
INTERNAL_SERVICE_KEY=$(openssl rand -hex 24)
AUTH_SECRET=$(openssl rand -hex 24)
DATABASE_URL=postgres://postgres:demo_pw_local_only@127.0.0.1:5432/bandwidth_platform?sslmode=disable
CONTROL_PLANE_URL=http://127.0.0.1:8080
FRAUD_SERVICE_URL=http://127.0.0.1:8082
EOF
  export $(grep -v '^#' .env | xargs)
  echo "Wrote .env — keep this out of version control (it's already in a typical .gitignore pattern; double-check yours)"
fi

echo ""
echo "=== 3. Build ==="
(cd control-plane && go build -o control-plane .)
(cd gateway && go build -o gateway .)
(cd client/rust-relay && cargo build --release)
[ -f gateway/gateway-cert.pem ] || bash scripts/gen-cert.sh

echo ""
echo "=== 3b. Python environment for the fraud-scoring service ==="
# Recent Debian/Ubuntu (PEP 668) blocks system-wide `pip install` on
# purpose — a venv is the correct fix, not a workaround, so this is the
# default rather than something you have to opt into.
if [ ! -d fraud-service/venv ]; then
  python3 -m venv fraud-service/venv
  fraud-service/venv/bin/pip install --quiet flask scikit-learn joblib numpy pandas
  # Retrain rather than trust the pre-built fraud_model.joblib shipped in
  # the repo: scikit-learn's own pickle format is not guaranteed
  # compatible across versions, and pip installs whatever the latest
  # release is at the time you run this — which may not match whatever
  # version originally produced that file (this was caught by actually
  # running a fresh install and seeing a real
  # InconsistentVersionWarning, not by reasoning about it in advance).
  # Retraining here means the model and the sklearn doing the unpickling
  # are always the same install. Training data is synthetic and this
  # takes a few seconds — see fraud-service/train.py.
  (cd fraud-service && ./venv/bin/python3 train.py)
fi
FRAUD_PYTHON="$(pwd)/fraud-service/venv/bin/python3"

echo ""
echo "=== 4. Start backend services ==="
# fraud-service is started first, on its own, with a head start before
# the others: it's by far the slowest to initialize (loading numpy/
# pandas/scikit-learn and unpickling the model takes real time compared
# to the Go/Node binaries), and starting all four simultaneously via
# backgrounding was observed to sometimes cause it to fail outright
# rather than just start slowly — giving it a few seconds alone first
# avoids the race entirely, and costs nothing when it wasn't needed.
(cd fraud-service && nohup "$FRAUD_PYTHON" app.py > /tmp/fraud.log 2>&1 &)
sleep 3
(cd control-plane && nohup env ADMIN_API_KEY="$ADMIN_API_KEY" INTERNAL_SERVICE_KEY="$INTERNAL_SERVICE_KEY" AUTH_SECRET="$AUTH_SECRET" ./control-plane > /tmp/cp.log 2>&1 &)
(cd gateway && nohup env INTERNAL_SERVICE_KEY="$INTERNAL_SERVICE_KEY" ./gateway > /tmp/gw.log 2>&1 &)
(cd payout-engine && nohup env INTERNAL_SERVICE_KEY="$INTERNAL_SERVICE_KEY" AUTH_SECRET="$AUTH_SECRET" ADMIN_API_KEY="$ADMIN_API_KEY" node server.js > /tmp/pe.log 2>&1 &)
sleep 2

echo "-- health checks (polling up to 20s — fraud-service in particular takes a moment to load the ML model) --"
check_health() {
  local name="$1" url="$2" hdr="${3:-}"
  local code
  for i in $(seq 1 20); do
    if [ -n "$hdr" ]; then
      code=$(curl -s -o /dev/null -w '%{http_code}' -m 2 "$url" -H "$hdr" 2>/dev/null || echo "000")
    else
      code=$(curl -s -o /dev/null -w '%{http_code}' -m 2 "$url" 2>/dev/null || echo "000")
    fi
    if [ "$code" = "200" ]; then
      echo "  $name: 200 (took ${i}s)"
      return 0
    fi
    sleep 1
  done
  echo "  $name: NOT healthy after 20s (last status: $code) — check its log in /tmp/"
  return 1
}

healthy=0
check_health "control-plane" "localhost:8080/v1/enterprise" "Authorization: Bearer $ADMIN_API_KEY" || healthy=1
check_health "fraud-service" "localhost:8082/health" || healthy=1
check_health "payout-engine" "localhost:8081/v1/payouts" "Authorization: Bearer $ADMIN_API_KEY" || healthy=1

if [ "$healthy" -ne 0 ]; then
  echo ""
  echo "One or more services did not become healthy — logs:"
  echo "--- /tmp/cp.log ---";    cat /tmp/cp.log    2>/dev/null || echo "(no log)"
  echo "--- /tmp/fraud.log ---"; cat /tmp/fraud.log 2>/dev/null || echo "(no log)"
  echo "--- /tmp/pe.log ---";    cat /tmp/pe.log    2>/dev/null || echo "(no log)"
  exit 1
fi

echo ""
echo "=== 5. Start frontends ==="
(cd frontend/admin-console && nohup python3 -m http.server 8090 > /tmp/fe-admin.log 2>&1 &)
(cd frontend/wallet-dashboard && nohup python3 -m http.server 8091 > /tmp/fe-wallet.log 2>&1 &)
(cd frontend/enterprise-portal && nohup python3 -m http.server 8092 > /tmp/fe-ent.log 2>&1 &)
sleep 1

echo ""
echo "=== Done ==="
echo "Admin console:      http://localhost:8090  (sign in with ADMIN_API_KEY in .env)"
echo "Wallet dashboard:    http://localhost:8091  (sign in with any phone number)"
echo "Enterprise portal:   http://localhost:8092  (sign up for an API key)"
echo ""
echo "Verify everything: ADMIN_API_KEY=\$ADMIN_API_KEY bash scripts/backend-test.sh"
