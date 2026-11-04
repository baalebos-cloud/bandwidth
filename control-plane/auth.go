package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"
)

// ---------------------------------------------------------------------
// Minimal HS256 token (deliberately hand-rolled rather than pulling in a
// JWT library — this is a small, auditable amount of crypto-adjacent
// code, and the format is the standard header.payload.signature shape so
// it's still a real, interoperable JWT if you ever do want a library on
// either end later).
// ---------------------------------------------------------------------

var authSecret = []byte(envOr("AUTH_SECRET", "DEMO-ONLY-INSECURE-SECRET-CHANGE-ME"))

func init() {
	if os.Getenv("AUTH_SECRET") == "" {
		fmt.Println("WARNING: AUTH_SECRET not set — using a hardcoded demo secret.")
		fmt.Println("This is fine for local testing and MUST be set to a real random value before this leaves localhost.")
	}
}

type tokenClaims struct {
	UserID string `json:"user_id"`
	Exp    int64  `json:"exp"`
}

func b64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func generateUserToken(userID string, ttl time.Duration) string {
	header := b64url([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims, _ := json.Marshal(tokenClaims{UserID: userID, Exp: time.Now().Add(ttl).Unix()})
	payload := b64url(claims)
	signingInput := header + "." + payload
	sig := hmac.New(sha256.New, authSecret)
	sig.Write([]byte(signingInput))
	return signingInput + "." + b64url(sig.Sum(nil))
}

// validateUserToken checks the signature and expiry and returns the user id.
func validateUserToken(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("malformed token")
	}
	signingInput := parts[0] + "." + parts[1]
	sig := hmac.New(sha256.New, authSecret)
	sig.Write([]byte(signingInput))
	expectedSig := b64url(sig.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(expectedSig), []byte(parts[2])) != 1 {
		return "", fmt.Errorf("invalid signature")
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("malformed payload")
	}
	var claims tokenClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return "", fmt.Errorf("malformed claims")
	}
	if time.Now().Unix() > claims.Exp {
		return "", fmt.Errorf("token expired")
	}
	return claims.UserID, nil
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(h, "Bearer ")
}

// ---------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------

type ctxKey string

const ctxUserID ctxKey = "user_id"
const ctxEnterpriseID ctxKey = "enterprise_id"

// requireUserAuth validates a user's Bearer token (issued via the OTP
// login flow) and makes the user id available to the handler.
func requireUserAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			writeJSON(w, 401, map[string]string{"error": "missing bearer token"})
			return
		}
		userID, err := validateUserToken(token)
		if err != nil {
			writeJSON(w, 401, map[string]string{"error": "invalid or expired token: " + err.Error()})
			return
		}
		r.Header.Set("X-Authenticated-User-ID", userID) // simple pass-through, avoids a context-value plumbing detour for this handler set
		next(w, r)
	}
}

var adminAPIKey = envOr("ADMIN_API_KEY", "")

// requireAdminAuth protects ops-only endpoints (KYB approval, node/session
// listing). Fails closed if ADMIN_API_KEY was never configured — an
// unset admin key means NO admin access, not open admin access.
func requireAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if adminAPIKey == "" {
			writeJSON(w, 503, map[string]string{"error": "admin auth not configured on this server (ADMIN_API_KEY unset)"})
			return
		}
		key := bearerToken(r)
		if key == "" || subtle.ConstantTimeCompare([]byte(key), []byte(adminAPIKey)) != 1 {
			writeJSON(w, 401, map[string]string{"error": "invalid admin key"})
			return
		}
		next(w, r)
	}
}

// requireEnterpriseAuth validates an enterprise client's own API key and
// resolves it to that enterprise's id — the caller can no longer just
// state an EnterpriseClientID in the request body and have it trusted.
func requireEnterpriseAuth(store *Store, next func(w http.ResponseWriter, r *http.Request, enterpriseID string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := bearerToken(r)
		if key == "" {
			writeJSON(w, 401, map[string]string{"error": "missing bearer token (enterprise API key)"})
			return
		}
		enterpriseID, ok := store.EnterpriseIDForAPIKey(key)
		if !ok {
			writeJSON(w, 401, map[string]string{"error": "invalid API key"})
			return
		}
		next(w, r, enterpriseID)
	}
}

// ---------------------------------------------------------------------
// OTP login flow
// ---------------------------------------------------------------------

func generateOTPCode() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1000000))
	return fmt.Sprintf("%06d", n.Int64())
}

func hashOTP(phone, code string) string {
	h := sha256.Sum256([]byte(phone + ":" + code))
	return b64url(h[:])
}

// ---------------------------------------------------------------------
// Internal service-to-service auth — for calls the gateway and the
// payout-engine make to the control plane (session validate/finalize,
// ledger reserve/reverse). These aren't user- or enterprise-facing
// endpoints; they're trusted-backend-to-trusted-backend, so a shared
// secret is the right tool rather than a user token or admin key.
// ---------------------------------------------------------------------

var internalServiceKey = envOr("INTERNAL_SERVICE_KEY", "DEMO-ONLY-INSECURE-INTERNAL-KEY-CHANGE-ME")

func requireInternalService(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := bearerToken(r)
		if key == "" || subtle.ConstantTimeCompare([]byte(key), []byte(internalServiceKey)) != 1 {
			writeJSON(w, 401, map[string]string{"error": "invalid internal service key"})
			return
		}
		next(w, r)
	}
}
