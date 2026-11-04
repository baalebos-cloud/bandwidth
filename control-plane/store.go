package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	_ "github.com/lib/pq"
)

// This is the persistent replacement for the earlier in-memory Store.
// Every method keeps the same name and behavior contract as before —
// main.go is unchanged — but state now survives process restarts, which
// the in-memory version explicitly could not do. The core guarantees are
// unchanged: the ledger is append-only (no UPDATE ever touches a balance),
// and CreateSession is still the single KYB/eligibility enforcement point.

type User struct {
	ID          string
	PhoneNumber string
	CountryCode string
	Status      string
}

type Node struct {
	ID              string
	UserID          string
	DeviceFP        string
	TrustScore      float64
	Status          string
	IsDatacenterASN bool
	LastHeartbeat   *time.Time
	CreatedAt       time.Time
}

type EnterpriseClient struct {
	ID              string
	CompanyName     string
	KYBStatus       string
	DeclaredUseCase string
	RateLimitGBDay  int
	RejectionReason string
	APIKey          string
	CreatedAt       time.Time
}

type BandwidthSession struct {
	SessionID             string
	NodeID                string
	EnterpriseClientID    string
	SessionTokenHash      string
	rawToken              string
	Status                string
	GatewayConfirmedBytes uint64
	ClientReportedBytes   uint64
	MLFraudScore          *float64
	MLFraudFlag           bool
}

func (s *BandwidthSession) RawToken() string { return s.rawToken }

type LedgerEntry struct {
	ID               int64
	EntryGroupID     string
	AccountID        string
	AccountType      string
	Direction        string
	AmountMinorUnits int64
	Currency         string
	ReferenceType    string
	ReferenceID      string
	CreatedAt        time.Time
}

type Store struct {
	db *sql.DB
}

func NewStore() *Store {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:demo_pw_local_only@127.0.0.1:5432/bandwidth_platform?sslmode=disable"
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		panic(fmt.Sprintf("failed to open database: %v", err))
	}
	if err := db.Ping(); err != nil {
		panic(fmt.Sprintf("failed to connect to database (is Postgres running and DATABASE_URL correct?): %v", err))
	}
	db.SetMaxOpenConns(20)
	return &Store{db: db}
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// RegisterNode creates the user row if it doesn't exist yet, then the node.
func (s *Store) RegisterNode(userID, deviceFP string) *Node {
	s.db.Exec(
		`INSERT INTO users (id, country_code, status) VALUES ($1, 'NGA', 'active')
		 ON CONFLICT (id) DO NOTHING`, userID)

	n := &Node{UserID: userID, DeviceFP: deviceFP, TrustScore: 50.0, Status: "pending_verification"}
	row := s.db.QueryRow(
		`INSERT INTO nodes (user_id, device_fingerprint, os_type, public_key, trust_score, status)
		 VALUES ($1, $2, 'unspecified', 'unspecified', 50.0, 'pending_verification')
		 ON CONFLICT (user_id, device_fingerprint) DO UPDATE SET device_fingerprint = EXCLUDED.device_fingerprint
		 RETURNING id, created_at`,
		userID, deviceFP)
	if err := row.Scan(&n.ID, &n.CreatedAt); err != nil {
		panic(fmt.Sprintf("RegisterNode insert failed: %v", err))
	}
	return n
}

func (s *Store) RegisterEnterpriseClient(name, useCase string) *EnterpriseClient {
	c := &EnterpriseClient{CompanyName: name, DeclaredUseCase: useCase, KYBStatus: "pending", RateLimitGBDay: 5}
	c.APIKey = "sk_test_" + randomHex(16)
	row := s.db.QueryRow(
		`INSERT INTO enterprise_clients (company_name, kyb_status, declared_use_case, rate_limit_gb_daily, api_key)
		 VALUES ($1, 'pending', $2, 5, $3)
		 RETURNING id, created_at`,
		name, useCase, c.APIKey)
	if err := row.Scan(&c.ID, &c.CreatedAt); err != nil {
		panic(fmt.Sprintf("RegisterEnterpriseClient insert failed: %v", err))
	}
	return c
}

func (s *Store) ApproveEnterpriseClient(id string) error {
	res, err := s.db.Exec(`UPDATE enterprise_clients SET kyb_status = 'approved' WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("enterprise client not found")
	}
	return nil
}

func (s *Store) RejectEnterpriseClient(id, reason string) error {
	res, err := s.db.Exec(`UPDATE enterprise_clients SET kyb_status = 'rejected', rejection_reason = $2 WHERE id = $1`, id, reason)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("enterprise client not found")
	}
	return nil
}

func (s *Store) ListNodes() []*Node {
	rows, err := s.db.Query(`SELECT id, user_id, device_fingerprint, trust_score, status, is_datacenter_asn, created_at FROM nodes ORDER BY created_at DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*Node
	for rows.Next() {
		n := &Node{}
		if err := rows.Scan(&n.ID, &n.UserID, &n.DeviceFP, &n.TrustScore, &n.Status, &n.IsDatacenterASN, &n.CreatedAt); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func (s *Store) ListEnterpriseClients() []*EnterpriseClient {
	rows, err := s.db.Query(`SELECT id, company_name, kyb_status, declared_use_case, rate_limit_gb_daily, COALESCE(rejection_reason,''), COALESCE(api_key,''), created_at FROM enterprise_clients ORDER BY created_at DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*EnterpriseClient
	for rows.Next() {
		c := &EnterpriseClient{}
		if err := rows.Scan(&c.ID, &c.CompanyName, &c.KYBStatus, &c.DeclaredUseCase, &c.RateLimitGBDay, &c.RejectionReason, &c.APIKey, &c.CreatedAt); err == nil {
			out = append(out, c)
		}
	}
	return out
}

func (s *Store) GetEnterpriseClient(id string) (*EnterpriseClient, bool) {
	c := &EnterpriseClient{ID: id}
	row := s.db.QueryRow(`SELECT company_name, kyb_status, declared_use_case, rate_limit_gb_daily, COALESCE(rejection_reason,''), COALESCE(api_key,''), created_at FROM enterprise_clients WHERE id = $1`, id)
	if err := row.Scan(&c.CompanyName, &c.KYBStatus, &c.DeclaredUseCase, &c.RateLimitGBDay, &c.RejectionReason, &c.APIKey, &c.CreatedAt); err != nil {
		return nil, false
	}
	return c, true
}

func (s *Store) ListSessions() []*BandwidthSession {
	return s.querySessions(`SELECT session_id, node_id, enterprise_client_id, session_token_hash, status, gateway_confirmed_bytes, COALESCE(client_reported_bytes,0), ml_fraud_score, ml_fraud_flag FROM bandwidth_sessions ORDER BY started_at DESC LIMIT 200`)
}

// ListSessionsForEnterprise scopes results to one enterprise client — used
// by the enterprise portal, which should only ever see its own traffic,
// never the platform-wide view the admin console gets.
func (s *Store) ListSessionsForEnterprise(enterpriseID string) []*BandwidthSession {
	return s.querySessions(
		`SELECT session_id, node_id, enterprise_client_id, session_token_hash, status, gateway_confirmed_bytes, COALESCE(client_reported_bytes,0), ml_fraud_score, ml_fraud_flag
		 FROM bandwidth_sessions WHERE enterprise_client_id = $1 ORDER BY started_at DESC LIMIT 200`,
		enterpriseID)
}

func (s *Store) querySessions(query string, args ...any) []*BandwidthSession {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*BandwidthSession
	for rows.Next() {
		sess := &BandwidthSession{}
		var score sql.NullFloat64
		if err := rows.Scan(&sess.SessionID, &sess.NodeID, &sess.EnterpriseClientID, &sess.SessionTokenHash, &sess.Status, &sess.GatewayConfirmedBytes, &sess.ClientReportedBytes, &score, &sess.MLFraudFlag); err == nil {
			if score.Valid {
				sess.MLFraudScore = &score.Float64
			}
			out = append(out, sess)
		}
	}
	return out
}

// CreateSession is still the hard KYB + eligibility gate — enforced here
// against real persisted node/enterprise state, not an in-memory map.
func (s *Store) CreateSession(nodeID, enterpriseID string) (*BandwidthSession, error) {
	var isDatacenter bool
	var nodeStatus string
	row := s.db.QueryRow(`SELECT is_datacenter_asn, status FROM nodes WHERE id = $1`, nodeID)
	if err := row.Scan(&isDatacenter, &nodeStatus); err != nil {
		return nil, fmt.Errorf("unknown node")
	}
	if isDatacenter {
		return nil, fmt.Errorf("node rejected: datacenter ASN, not eligible to earn")
	}
	if nodeStatus == "banned" {
		return nil, fmt.Errorf("node is banned")
	}

	var kybStatus string
	row = s.db.QueryRow(`SELECT kyb_status FROM enterprise_clients WHERE id = $1`, enterpriseID)
	if err := row.Scan(&kybStatus); err != nil {
		return nil, fmt.Errorf("unknown enterprise client")
	}
	if kybStatus != "approved" {
		return nil, fmt.Errorf("enterprise client not approved (kyb_status=%s) — no session issued", kybStatus)
	}

	rawToken := randomHex(24)
	tokenHash := hashToken(rawToken)

	sess := &BandwidthSession{NodeID: nodeID, EnterpriseClientID: enterpriseID, Status: "open", rawToken: rawToken, SessionTokenHash: tokenHash}
	row = s.db.QueryRow(
		`INSERT INTO bandwidth_sessions (session_id, node_id, enterprise_client_id, session_token_hash, status)
		 VALUES (gen_random_uuid(), $1, $2, $3, 'open') RETURNING session_id`,
		nodeID, enterpriseID, tokenHash)
	if err := row.Scan(&sess.SessionID); err != nil {
		return nil, fmt.Errorf("failed to create session: %v", err)
	}
	return sess, nil
}

func (s *Store) ValidateSessionToken(sessionID, token string) bool {
	var storedHash, status string
	row := s.db.QueryRow(`SELECT session_token_hash, status FROM bandwidth_sessions WHERE session_id = $1`, sessionID)
	if err := row.Scan(&storedHash, &status); err != nil {
		return false
	}
	if status != "open" {
		return false
	}
	return storedHash == hashToken(token)
}

// FinalizeSession is still the ONLY place bytes become ledger credit, using
// only the gateway-confirmed figure. It also now calls the fraud-scoring
// service (if configured) and persists the result either way — scoring
// failures never block the session from finalizing, they just mean no
// score gets recorded, which the ops console surfaces as "unscored".
func (s *Store) FinalizeSession(sessionID string, gatewayConfirmedBytes, clientReportedBytes uint64) error {
	var nodeID string
	row := s.db.QueryRow(`SELECT node_id FROM bandwidth_sessions WHERE session_id = $1`, sessionID)
	if err := row.Scan(&nodeID); err != nil {
		return fmt.Errorf("unknown session")
	}

	score, flagged, scored := scoreSessionForFraud(sessionID, nodeID, gatewayConfirmedBytes, clientReportedBytes, s)

	if scored {
		s.db.Exec(
			`UPDATE bandwidth_sessions SET status='finalized', gateway_confirmed_bytes=$2, client_reported_bytes=$3, ml_fraud_score=$4, ml_fraud_flag=$5 WHERE session_id=$1`,
			sessionID, gatewayConfirmedBytes, clientReportedBytes, score, flagged)
	} else {
		s.db.Exec(
			`UPDATE bandwidth_sessions SET status='finalized', gateway_confirmed_bytes=$2, client_reported_bytes=$3 WHERE session_id=$1`,
			sessionID, gatewayConfirmedBytes, clientReportedBytes)
	}

	if flagged {
		// Graduated response: drop trust score, don't auto-ban on one flag.
		s.db.Exec(`UPDATE nodes SET trust_score = GREATEST(0, trust_score - 15) WHERE id = $1`, nodeID)
	}

	// Pay per GB — trivial demo rate, matching the in-memory version's scale.
	const payoutPerGBMinorUnits = 40
	gb := float64(gatewayConfirmedBytes) / (1024 * 1024 * 1024)
	amount := int64(gb * payoutPerGBMinorUnits * 1000)
	if amount <= 0 {
		amount = 1
	}

	var userID string
	s.db.QueryRow(`SELECT user_id FROM nodes WHERE id = $1`, nodeID).Scan(&userID)
	if userID == "" {
		return nil // node vanished between checks — nothing to credit
	}

	groupID := randomHex(16)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(
		`INSERT INTO ledger_entries (entry_group_id, account_id, account_type, direction, amount_minor_units, currency, reference_type, reference_id)
		 VALUES ($1, $2, 'user_wallet', 'credit', $3, 'NGN', 'bandwidth_earning', $4)`,
		groupID, userID, amount, sessionID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(
		`INSERT INTO ledger_entries (entry_group_id, account_id, account_type, direction, amount_minor_units, currency, reference_type, reference_id)
		 VALUES ($1, 'platform_revenue', 'platform_revenue', 'debit', $2, 'NGN', 'bandwidth_earning', $3)`,
		groupID, amount, sessionID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Balance(accountID string) int64 {
	var bal sql.NullInt64
	row := s.db.QueryRow(
		`SELECT SUM(CASE WHEN direction='credit' THEN amount_minor_units ELSE -amount_minor_units END)
		 FROM ledger_entries WHERE account_id = $1`, accountID)
	row.Scan(&bal)
	return bal.Int64
}

func (s *Store) ReserveForPayout(userID string, amount int64, referenceID string) error {
	if s.Balance(userID) < amount {
		return fmt.Errorf("insufficient balance")
	}
	groupID := randomHex(16)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT INTO ledger_entries (entry_group_id, account_id, account_type, direction, amount_minor_units, currency, reference_type, reference_id)
		 VALUES ($1, $2, 'user_wallet', 'debit', $3, 'NGN', 'payout', $4)`, groupID, userID, amount, referenceID); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO ledger_entries (entry_group_id, account_id, account_type, direction, amount_minor_units, currency, reference_type, reference_id)
		 VALUES ($1, 'payout_clearing', 'payout_clearing', 'credit', $2, 'NGN', 'payout', $3)`, groupID, amount, referenceID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ReverseForPayout(userID string, amount int64, referenceID string) {
	groupID := randomHex(16)
	tx, err := s.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	tx.Exec(
		`INSERT INTO ledger_entries (entry_group_id, account_id, account_type, direction, amount_minor_units, currency, reference_type, reference_id)
		 VALUES ($1, $2, 'user_wallet', 'credit', $3, 'NGN', 'payout_reversal', $4)`, groupID, userID, amount, referenceID)
	tx.Exec(
		`INSERT INTO ledger_entries (entry_group_id, account_id, account_type, direction, amount_minor_units, currency, reference_type, reference_id)
		 VALUES ($1, 'payout_clearing', 'payout_clearing', 'debit', $2, 'NGN', 'payout_reversal', $3)`, groupID, amount, referenceID)
	tx.Commit()
}

func (s *Store) LedgerHistory(accountID string) []LedgerEntry {
	rows, err := s.db.Query(
		`SELECT id, entry_group_id, account_id, account_type, direction, amount_minor_units, currency, reference_type, reference_id, created_at
		 FROM ledger_entries WHERE account_id = $1 ORDER BY created_at DESC LIMIT 200`, accountID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []LedgerEntry
	for rows.Next() {
		var e LedgerEntry
		if err := rows.Scan(&e.ID, &e.EntryGroupID, &e.AccountID, &e.AccountType, &e.Direction, &e.AmountMinorUnits, &e.Currency, &e.ReferenceType, &e.ReferenceID, &e.CreatedAt); err == nil {
			out = append(out, e)
		}
	}
	return out
}

func hashToken(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// ---------------------------------------------------------------------
// Auth-related store methods
// ---------------------------------------------------------------------

// EnterpriseIDForAPIKey resolves an API key to its owning enterprise
// client id — the enforcement point that replaces trusting a caller-
// supplied EnterpriseClientID in a request body.
func (s *Store) EnterpriseIDForAPIKey(apiKey string) (string, bool) {
	var id string
	row := s.db.QueryRow(`SELECT id FROM enterprise_clients WHERE api_key = $1`, apiKey)
	if err := row.Scan(&id); err != nil {
		return "", false
	}
	return id, true
}

// CreateOTP issues a one-time code for a phone number. Returns the plain
// code ONLY because this reference build has no real SMS provider wired
// up (see the same honest pattern used for VTpass credentials elsewhere
// in this project) — a production build sends this via an SMS API and
// never returns or logs the plain code in the HTTP response at all.
func (s *Store) CreateOTP(phone string) (string, error) {
	code := generateOTPCode()
	_, err := s.db.Exec(
		`INSERT INTO otp_requests (phone_number, code_hash, expires_at) VALUES ($1, $2, now() + interval '5 minutes')`,
		phone, hashOTP(phone, code))
	if err != nil {
		return "", err
	}
	return code, nil
}

// VerifyOTP checks the most recent unconsumed code for a phone number,
// enforces a small attempt cap per code to blunt brute-forcing a 6-digit
// space over its 5-minute window, and on success creates the user (if
// new) and consumes the code so it can't be replayed.
func (s *Store) VerifyOTP(phone, code string) (string, error) {
	var id int64
	var codeHash string
	var attempts int
	row := s.db.QueryRow(
		`SELECT id, code_hash, attempt_count FROM otp_requests
		 WHERE phone_number = $1 AND consumed_at IS NULL AND expires_at > now()
		 ORDER BY created_at DESC LIMIT 1`, phone)
	if err := row.Scan(&id, &codeHash, &attempts); err != nil {
		return "", fmt.Errorf("no active code for this number — request a new one")
	}
	if attempts >= 5 {
		return "", fmt.Errorf("too many attempts — request a new code")
	}
	if codeHash != hashOTP(phone, code) {
		s.db.Exec(`UPDATE otp_requests SET attempt_count = attempt_count + 1 WHERE id = $1`, id)
		return "", fmt.Errorf("incorrect code")
	}
	s.db.Exec(`UPDATE otp_requests SET consumed_at = now() WHERE id = $1`, id)

	// The user's id IS their phone number — consistent with how UserID
	// has been treated as an opaque caller-chosen key everywhere else in
	// this project, and it's a natural fit for a phone-based login.
	s.db.Exec(
		`INSERT INTO users (id, phone_number, country_code, status) VALUES ($1, $1, 'NGA', 'active')
		 ON CONFLICT (id) DO NOTHING`, phone)
	return phone, nil
}
