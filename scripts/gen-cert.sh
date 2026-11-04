#!/usr/bin/env bash
# Throwaway self-signed cert for local demo use only.
set -euo pipefail
cd "$(dirname "$0")/../gateway"
openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout gateway-key.pem -out gateway-cert.pem \
  -days 7 -subj "/CN=localhost" \
  -addext "basicConstraints=critical,CA:FALSE" \
  -addext "keyUsage=critical,digitalSignature,keyEncipherment" \
  -addext "extendedKeyUsage=serverAuth" \
  -addext "subjectAltName=DNS:localhost" 2>/dev/null
cp gateway-cert.pem ../client/rust-relay/
echo "Generated gateway-cert.pem / gateway-key.pem and copied cert into client/rust-relay/"
