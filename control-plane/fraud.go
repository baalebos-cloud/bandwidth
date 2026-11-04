package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"time"
)

var fraudServiceURL = envOr("FRAUD_SERVICE_URL", "http://127.0.0.1:8082")

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// scoreSessionForFraud gathers the features the model needs from Postgres,
// calls the fraud-scoring service, and returns (score, flagged, ok). ok is
// false if scoring couldn't happen for any reason (service down, bad node
// id, etc.) — the caller (FinalizeSession) treats that as "leave unscored"
// rather than blocking or failing the session. A fraud model being briefly
// unavailable must never be able to stop a legitimate payout from settling.
func scoreSessionForFraud(sessionID, nodeID string, gatewayBytes, clientBytes uint64, s *Store) (score float64, flagged bool, ok bool) {
	var trustScore float64
	var isDatacenter bool
	var createdAt time.Time
	row := s.db.QueryRow(`SELECT trust_score, is_datacenter_asn, created_at FROM nodes WHERE id = $1`, nodeID)
	if err := row.Scan(&trustScore, &isDatacenter, &createdAt); err != nil {
		return 0, false, false
	}
	nodeAgeHours := time.Since(createdAt).Hours()

	var sessionsLastHour int
	s.db.QueryRow(
		`SELECT COUNT(*) FROM bandwidth_sessions WHERE node_id = $1 AND started_at > now() - interval '1 hour'`,
		nodeID).Scan(&sessionsLastHour)

	reqBody, _ := json.Marshal(map[string]any{
		"gateway_confirmed_bytes": gatewayBytes,
		"client_reported_bytes":   clientBytes,
		"node_trust_score":        trustScore,
		"node_age_hours":          nodeAgeHours,
		"sessions_last_hour":      sessionsLastHour,
		"is_datacenter_asn":       isDatacenter,
	})

	client := http.Client{Timeout: 2 * time.Second} // fraud scoring must never meaningfully delay finalization
	resp, err := client.Post(fraudServiceURL+"/score", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return 0, false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, false, false
	}

	var out struct {
		Score        float64        `json:"score"`
		Flagged      bool           `json:"flagged"`
		ModelVersion string         `json:"model_version"`
		Features     map[string]any `json:"features"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, false, false
	}

	// Log every scoring call for audit, independent of what ends up on the
	// session row — this is what lets you reconstruct model behavior later
	// even if a session gets rescored.
	featuresJSON, _ := json.Marshal(out.Features)
	s.db.Exec(
		`INSERT INTO fraud_score_log (session_id, node_id, model_version, score, flagged, features_json)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		sessionID, nodeID, out.ModelVersion, out.Score, out.Flagged, featuresJSON)

	return out.Score, out.Flagged, true
}
