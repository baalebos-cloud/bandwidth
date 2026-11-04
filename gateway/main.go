// Bandwidth-platform gateway — the enforcement + metering point of the
// whole system. Two rules this file exists to guarantee:
//
//  1. No bytes get relayed for a session the control plane hasn't
//     explicitly issued (which itself required enterprise KYB approval
//     and node eligibility — see control-plane/store.go CreateSession).
//  2. The byte count that gets paid out is the one THIS process measures,
//     never anything the client claims.
package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync/atomic"
)

var controlPlaneURL = flag.String("control-plane", "http://127.0.0.1:8080", "control plane base URL")

// Shared secret authenticating the gateway to the control plane as a
// trusted internal service — must match INTERNAL_SERVICE_KEY on the
// control plane. Without this, session validate/finalize calls are
// rejected with 401, matching the fail-closed posture the rest of this
// gateway already follows for an unvalidated session.
var internalServiceKey = os.Getenv("INTERNAL_SERVICE_KEY")

func init() {
	if internalServiceKey == "" {
		internalServiceKey = "DEMO-ONLY-INSECURE-INTERNAL-KEY-CHANGE-ME"
		log.Println("WARNING: INTERNAL_SERVICE_KEY not set — using the control-plane's own hardcoded demo default. Set this before deploying beyond localhost.")
	}
}

func doAuthedPost(url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+internalServiceKey)
	return http.DefaultClient.Do(req)
}

type countingConn struct {
	net.Conn
	bytesRead atomic.Uint64
}

func (c *countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.bytesRead.Add(uint64(n))
	}
	return n, err
}

func main() {
	flag.Parse()

	cert, err := tls.LoadX509KeyPair("gateway-cert.pem", "gateway-key.pem")
	if err != nil {
		log.Fatalf("failed to load TLS cert (run scripts/gen-cert.sh first): %v", err)
	}
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}}

	listener, err := tls.Listen("tcp", ":9443", tlsConfig)
	if err != nil {
		log.Fatalf("listen failed: %v", err)
	}
	log.Printf("gateway listening on :9443, control-plane at %s", *controlPlaneURL)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("accept error: %v", err)
			continue
		}
		go handleSession(conn)
	}
}

// wire framing for this demo: [2-byte len][session_id][2-byte len][token]
// then raw relay bytes until EOF. A production build carries this in the
// WireGuard/session-establishment handshake instead of an ad hoc prefix.
func readLenPrefixed(r io.Reader) (string, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return "", err
	}
	n := int(lenBuf[0])<<8 | int(lenBuf[1])
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func handleSession(conn net.Conn) {
	defer conn.Close()
	cc := &countingConn{Conn: conn}

	sessionID, err := readLenPrefixed(cc)
	if err != nil {
		log.Printf("failed to read session id: %v", err)
		return
	}
	token, err := readLenPrefixed(cc)
	if err != nil {
		log.Printf("failed to read token: %v", err)
		return
	}

	if !validateSessionWithControlPlane(sessionID, token) {
		log.Printf("session %s REJECTED — invalid or unauthorized (no KYB / not eligible)", sessionID)
		return // refuse to relay a single byte for an unvalidated session
	}
	log.Printf("session %s validated — relaying begins", sessionID)

	buf := make([]byte, 32*1024)
	for {
		_, err := cc.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("session %s read error: %v", sessionID, err)
			break
		}
	}

	confirmed := cc.bytesRead.Load()
	log.Printf("session %s GATEWAY_CONFIRMED_BYTES=%d", sessionID, confirmed)
	finalizeSessionWithControlPlane(sessionID, confirmed)
}

func validateSessionWithControlPlane(sessionID, token string) bool {
	body, _ := json.Marshal(map[string]string{"SessionID": sessionID, "Token": token})
	resp, err := doAuthedPost(*controlPlaneURL+"/v1/sessions/validate", body)
	if err != nil {
		log.Printf("control-plane validate call failed: %v", err)
		return false // fail closed, not open
	}
	defer resp.Body.Close()
	var out struct{ Valid bool }
	json.NewDecoder(resp.Body).Decode(&out)
	return out.Valid
}

func finalizeSessionWithControlPlane(sessionID string, gatewayConfirmedBytes uint64) {
	body, _ := json.Marshal(map[string]any{
		"SessionID":             sessionID,
		"GatewayConfirmedBytes": gatewayConfirmedBytes,
		"ClientReportedBytes":   gatewayConfirmedBytes, // demo client is honest; see rust-relay for the field's real purpose
	})
	resp, err := doAuthedPost(*controlPlaneURL+"/v1/sessions/finalize", body)
	if err != nil {
		log.Printf("control-plane finalize call failed: %v", err)
		return
	}
	defer resp.Body.Close()
}
