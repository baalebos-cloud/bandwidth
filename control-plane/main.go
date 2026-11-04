package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

var store = NewStore()

func main() {
	mux := http.NewServeMux()

	// --- User auth (OTP login) ---
	mux.HandleFunc("/v1/auth/request-otp", handleRequestOTP)
	mux.HandleFunc("/v1/auth/verify-otp", handleVerifyOTP)

	// --- User-authenticated endpoints (Bearer: user token from verify-otp) ---
	mux.HandleFunc("/v1/nodes/register", requireUserAuth(handleRegisterNode))
	mux.HandleFunc("/v1/wallet/", requireUserAuth(handleWallet))
	mux.HandleFunc("/v1/ledger/history/", requireUserAuth(handleLedgerHistory))

	// --- Enterprise self-serve (no auth — registering IS how you get a key) ---
	mux.HandleFunc("/v1/enterprise/register", handleRegisterEnterprise)

	// --- Enterprise-authenticated (Bearer: that enterprise's own API key) ---
	mux.HandleFunc("/v1/enterprise/whoami", handleEnterpriseWhoAmI)
	mux.HandleFunc("/v1/sessions/mine", requireEnterpriseAuth(store, handleListMySessions))

	// --- Admin-authenticated (Bearer: ADMIN_API_KEY) ---
	mux.HandleFunc("/v1/nodes", requireAdminAuth(handleListNodes))
	mux.HandleFunc("/v1/enterprise/approve", requireAdminAuth(handleApproveEnterprise))
	mux.HandleFunc("/v1/enterprise/reject", requireAdminAuth(handleRejectEnterprise))
	mux.HandleFunc("/v1/enterprise", requireAdminAuth(handleListEnterprise))
	mux.HandleFunc("/v1/sessions/list", requireAdminAuth(handleListSessions))

	// --- Session creation: enterprise-authenticated, resolves enterprise
	//     id from the API key itself rather than trusting a body field ---
	mux.HandleFunc("/v1/sessions", requireEnterpriseAuth(store, handleCreateSession))

	// --- Internal service-to-service (Bearer: INTERNAL_SERVICE_KEY) ---
	mux.HandleFunc("/v1/sessions/validate", requireInternalService(handleValidateSession)) // called by the gateway
	mux.HandleFunc("/v1/sessions/finalize", requireInternalService(handleFinalizeSession)) // called by the gateway
	mux.HandleFunc("/v1/ledger/reserve", requireInternalService(handleLedgerReserve))       // called by the payout engine
	mux.HandleFunc("/v1/ledger/reverse", requireInternalService(handleLedgerReverse))       // called by the payout engine

	log.Println("control-plane listening on :8080")
	if adminAPIKey == "" {
		log.Println("NOTE: ADMIN_API_KEY not set — admin endpoints (KYB approval, node/session listing) are unreachable, not open. Set it to enable them.")
	}
	log.Fatal(http.ListenAndServe(":8080", withCORS(mux)))
}

// withCORS lets the static admin/wallet/enterprise frontends (served from a
// different origin/port) call this API directly from the browser.
func withCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------------
// Auth handlers
// ---------------------------------------------------------------------

func handleRequestOTP(w http.ResponseWriter, r *http.Request) {
	var req struct{ PhoneNumber string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.PhoneNumber == "" {
		writeJSON(w, 400, map[string]string{"error": "phone_number required"})
		return
	}
	code, err := store.CreateOTP(req.PhoneNumber)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	// Reference-build bypass: no SMS provider is wired up (same honest gap
	// as the untested VTpass credentials elsewhere in this project), so the
	// code is returned directly instead of sent by SMS. A production build
	// removes "code" from this response entirely.
	log.Printf("OTP for %s: %s (reference build — no real SMS delivery)", req.PhoneNumber, code)
	writeJSON(w, 200, map[string]string{"status": "sent", "code_for_demo_only": code})
}

func handleVerifyOTP(w http.ResponseWriter, r *http.Request) {
	var req struct{ PhoneNumber, Code string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	userID, err := store.VerifyOTP(req.PhoneNumber, req.Code)
	if err != nil {
		writeJSON(w, 401, map[string]string{"error": err.Error()})
		return
	}
	token := generateUserToken(userID, 30*24*time.Hour)
	writeJSON(w, 200, map[string]string{"token": token, "user_id": userID})
}

func handleEnterpriseWhoAmI(w http.ResponseWriter, r *http.Request) {
	requireEnterpriseAuth(store, func(w http.ResponseWriter, r *http.Request, enterpriseID string) {
		c, ok := store.GetEnterpriseClient(enterpriseID)
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, 200, c)
	})(w, r)
}

// ---------------------------------------------------------------------
// Node / wallet / ledger (user-authenticated)
// ---------------------------------------------------------------------

func handleRegisterNode(w http.ResponseWriter, r *http.Request) {
	authedUserID := r.Header.Get("X-Authenticated-User-ID")
	var req struct{ DeviceFingerprint string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	// UserID comes from the verified token now, never from the request
	// body — a caller can no longer register a node "as" someone else.
	n := store.RegisterNode(authedUserID, req.DeviceFingerprint)
	writeJSON(w, 201, n)
}

func handleWallet(w http.ResponseWriter, r *http.Request) {
	authedUserID := r.Header.Get("X-Authenticated-User-ID")
	pathUserID := strings.TrimPrefix(r.URL.Path, "/v1/wallet/")
	if pathUserID != authedUserID {
		writeJSON(w, 403, map[string]string{"error": "cannot view another user's wallet"})
		return
	}
	bal := store.Balance(authedUserID)
	writeJSON(w, 200, map[string]any{"user_id": authedUserID, "balance_minor_units": bal, "currency": "NGN"})
}

func handleLedgerHistory(w http.ResponseWriter, r *http.Request) {
	authedUserID := r.Header.Get("X-Authenticated-User-ID")
	pathAccountID := strings.TrimPrefix(r.URL.Path, "/v1/ledger/history/")
	if pathAccountID != authedUserID {
		writeJSON(w, 403, map[string]string{"error": "cannot view another user's ledger history"})
		return
	}
	writeJSON(w, 200, store.LedgerHistory(authedUserID))
}

// ---------------------------------------------------------------------
// Enterprise
// ---------------------------------------------------------------------

func handleRegisterEnterprise(w http.ResponseWriter, r *http.Request) {
	var req struct{ CompanyName, UseCase string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	c := store.RegisterEnterpriseClient(req.CompanyName, req.UseCase)
	writeJSON(w, 201, c) // includes the API key — this response IS the only time it's handed out in the clear
}

func handleApproveEnterprise(w http.ResponseWriter, r *http.Request) {
	var req struct{ ID string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if err := store.ApproveEnterpriseClient(req.ID); err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "approved"})
}

func handleRejectEnterprise(w http.ResponseWriter, r *http.Request) {
	var req struct{ ID, Reason string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if err := store.RejectEnterpriseClient(req.ID, req.Reason); err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "rejected"})
}

func handleListEnterprise(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, store.ListEnterpriseClients())
}

func handleListNodes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, store.ListNodes())
}

func handleListSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, store.ListSessions())
}

func handleListMySessions(w http.ResponseWriter, r *http.Request, enterpriseID string) {
	writeJSON(w, 200, store.ListSessionsForEnterprise(enterpriseID))
}

// ---------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------

func handleCreateSession(w http.ResponseWriter, r *http.Request, enterpriseID string) {
	var req struct{ NodeID string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	// enterpriseID came from the verified API key, not the request body —
	// an enterprise client can no longer create sessions "as" a different,
	// possibly-unapproved enterprise client just by naming its id.
	sess, err := store.CreateSession(req.NodeID, enterpriseID)
	if err != nil {
		writeJSON(w, 403, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 201, map[string]string{
		"session_id": sess.SessionID,
		"token":      sess.RawToken(),
	})
}

func handleValidateSession(w http.ResponseWriter, r *http.Request) {
	var req struct{ SessionID, Token string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	valid := store.ValidateSessionToken(req.SessionID, req.Token)
	writeJSON(w, 200, map[string]bool{"valid": valid})
}

func handleFinalizeSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID             string
		GatewayConfirmedBytes uint64
		ClientReportedBytes   uint64
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if err := store.FinalizeSession(req.SessionID, req.GatewayConfirmedBytes, req.ClientReportedBytes); err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "finalized"})
}

// ---------------------------------------------------------------------
// Ledger (internal-service-authenticated — called by the payout engine)
// ---------------------------------------------------------------------

func handleLedgerReserve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID      string
		Amount      int64
		ReferenceID string
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if err := store.ReserveForPayout(req.UserID, req.Amount, req.ReferenceID); err != nil {
		writeJSON(w, 402, map[string]string{"error": err.Error()}) // 402 Payment Required
		return
	}
	writeJSON(w, 200, map[string]string{"status": "reserved"})
}

func handleLedgerReverse(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID      string
		Amount      int64
		ReferenceID string
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	store.ReverseForPayout(req.UserID, req.Amount, req.ReferenceID)
	writeJSON(w, 200, map[string]string{"status": "reversed"})
}
