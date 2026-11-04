// Reference client for the bandwidth-platform demo — now going through the
// real auth flow rather than trusting caller-supplied identity:
//
//  1. Phone-based OTP login (request-otp -> verify-otp) to get a user
//     Bearer token. No real SMS provider is wired up in this reference
//     build, so the OTP is read directly out of the request-otp response
//     (see control-plane's handleRequestOTP for the same honest caveat).
//  2. Register a node AS that authenticated user (the token, not a
//     request field, determines whose node this is).
//  3. Request a session using the ENTERPRISE'S OWN API KEY — the
//     enterprise_client_id is no longer a value this program states, it's
//     resolved server-side from the key.
//  4. Relay through the gateway exactly as before.

use rustls::{ClientConfig, RootCertStore, ClientConnection, StreamOwned};
use std::io::{Read, Write};
use std::net::TcpStream;
use std::sync::Arc;
use std::fs;
use std::env;

fn http_post_json(base_url: &str, path: &str, body: &str, bearer: Option<&str>) -> String {
    // Minimal hand-rolled HTTP/1.1 client over plain TCP — avoids pulling in
    // a full HTTP client crate for what is otherwise a handful of calls.
    let url = base_url.trim_start_matches("http://");
    let (host, _) = url.split_once('/').unwrap_or((url, ""));
    let mut stream = TcpStream::connect(host).expect("connect to control-plane failed");
    let auth_header = match bearer {
        Some(t) => format!("Authorization: Bearer {}\r\n", t),
        None => String::new(),
    };
    let request = format!(
        "POST {path} HTTP/1.1\r\nHost: {host}\r\nContent-Type: application/json\r\n{auth}Content-Length: {len}\r\nConnection: close\r\n\r\n{body}",
        path = path, host = host, auth = auth_header, len = body.len(), body = body
    );
    stream.write_all(request.as_bytes()).expect("write failed");
    let mut resp = String::new();
    stream.read_to_string(&mut resp).expect("read failed");
    resp.split("\r\n\r\n").nth(1).unwrap_or("").to_string()
}

fn write_len_prefixed(stream: &mut impl Write, s: &str) -> std::io::Result<()> {
    let bytes = s.as_bytes();
    let len = bytes.len() as u16;
    stream.write_all(&len.to_be_bytes())?;
    stream.write_all(bytes)
}

fn extract_json_field(json: &str, field: &str) -> Option<String> {
    let needle = format!("\"{}\":\"", field);
    let start = json.find(&needle)? + needle.len();
    let end = json[start..].find('"')? + start;
    Some(json[start..end].to_string())
}

fn main() -> anyhow::Result<()> {
    let control_plane = env::var("CONTROL_PLANE_URL").unwrap_or_else(|_| "http://127.0.0.1:8080".into());
    let phone_number = env::var("DEMO_PHONE_NUMBER").unwrap_or_else(|_| "+2348000000001".into());
    let enterprise_api_key = env::var("DEMO_ENTERPRISE_API_KEY")
        .expect("set DEMO_ENTERPRISE_API_KEY (an APPROVED enterprise client's own API key, from its registration response)");

    // 1a. Request an OTP.
    let otp_req_body = format!(r#"{{"PhoneNumber":"{}"}}"#, phone_number);
    let otp_resp = http_post_json(&control_plane, "/v1/auth/request-otp", &otp_req_body, None);
    let code = extract_json_field(&otp_resp, "code_for_demo_only")
        .expect("request-otp did not return a demo code — see control-plane logs");
    println!("Requested OTP for {} (reference build reads it back directly: {})", phone_number, code);

    // 1b. Verify it and get a user token.
    let verify_body = format!(r#"{{"PhoneNumber":"{}","Code":"{}"}}"#, phone_number, code);
    let verify_resp = http_post_json(&control_plane, "/v1/auth/verify-otp", &verify_body, None);
    let user_token = extract_json_field(&verify_resp, "token")
        .expect("verify-otp failed — check the code and try again");
    let user_id = extract_json_field(&verify_resp, "user_id").unwrap_or_else(|| phone_number.clone());
    println!("Logged in as {}", user_id);

    // 2. Register a node — the server derives ownership from user_token,
    //    not from anything this program states about itself.
    let device_fp = format!("rust-relay-{}", std::process::id());
    let node_body = format!(r#"{{"DeviceFingerprint":"{}"}}"#, device_fp);
    let node_resp = http_post_json(&control_plane, "/v1/nodes/register", &node_body, Some(&user_token));
    let node_id = extract_json_field(&node_resp, "ID")
        .expect("node registration failed — check the user token");
    println!("Registered node {}", node_id);

    // 3. Request a session using the enterprise's own API key — this
    //    fails closed if that enterprise isn't KYB-approved or this node
    //    isn't eligible.
    let sess_body = format!(r#"{{"NodeID":"{}"}}"#, node_id);
    let sess_resp = http_post_json(&control_plane, "/v1/sessions", &sess_body, Some(&enterprise_api_key));
    println!("control-plane /v1/sessions response: {}", sess_resp);
    let session_id = extract_json_field(&sess_resp, "session_id")
        .expect("session request was rejected — check enterprise KYB status, API key, and node eligibility");
    let session_token = extract_json_field(&sess_resp, "token").unwrap();

    // 4. Connect to the gateway and present the session token (unrelated
    //    to the user's login token — this is the gateway-issued,
    //    session-scoped credential from step 3).
    let chunk = vec![0xABu8; 32 * 1024];
    let chunks_to_send: usize = 320; // 10MB known payload
    let mut client_reported_bytes: u64 = 0;

    let mut root_store = RootCertStore::empty();
    let cert_pem = fs::read("gateway-cert.pem")
        .expect("copy gateway-cert.pem from the gateway/ directory into this one first");
    let mut certs = rustls_pemfile::certs(&mut cert_pem.as_slice())?;
    for c in certs.drain(..) {
        root_store.add(&rustls::Certificate(c))?;
    }
    let config = ClientConfig::builder()
        .with_safe_defaults()
        .with_root_certificates(root_store)
        .with_no_client_auth();
    let server_name = "localhost".try_into().expect("invalid DNS name");
    let conn = ClientConnection::new(Arc::new(config), server_name)?;
    let sock = TcpStream::connect("127.0.0.1:9443")?;
    let mut tls_stream = StreamOwned::new(conn, sock);

    write_len_prefixed(&mut tls_stream, &session_id)?;
    write_len_prefixed(&mut tls_stream, &session_token)?;

    for _ in 0..chunks_to_send {
        tls_stream.write_all(&chunk)?;
        client_reported_bytes += chunk.len() as u64;
    }
    tls_stream.flush()?;

    println!("session_id             = {}", session_id);
    println!("CLIENT_REPORTED_BYTES   = {}", client_reported_bytes);
    println!("Check the gateway log for GATEWAY_CONFIRMED_BYTES for this session — that,");
    println!("not this number, is what the control plane just credited to user {}.", user_id);
    println!("Query balance: curl -H \"Authorization: Bearer {}\" {}/v1/wallet/{}", user_token, control_plane, user_id);

    Ok(())
}
