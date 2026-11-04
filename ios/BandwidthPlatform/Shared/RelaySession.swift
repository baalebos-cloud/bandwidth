import Foundation
import Network

/// Relays through the gateway using the exact same wire protocol as the
/// desktop Rust client and the Android client: a length-prefixed
/// session_id, a length-prefixed token, then raw payload bytes. Built on
/// `Network.framework` (`NWConnection`) rather than `URLSession`, since a
/// raw TLS byte-stream relay — not a series of HTTP requests — is what
/// this actually is, matching the transport-level nature of the desktop
/// and Android implementations.
///
/// Like `RelaySession.kt` on Android and the current `gateway/main.go`,
/// this sends a synthetic payload rather than bridging real bidirectional
/// enterprise traffic — consistent with where the rest of this reference
/// implementation currently stops (see the gateway's own comment about
/// its drain-only relay loop). All three clients are intentionally
/// protocol-compatible with each other and with that same current scope.
final class RelaySession {
    enum RelayError: Error { case cancelled, connectionFailed(Error), notReady }

    private let host: NWEndpoint.Host
    private let port: NWEndpoint.Port
    private var connection: NWConnection?
    private var isCancelled = false
    private let queue = DispatchQueue(label: "com.bandwidthplatform.relay-session")

    init(gatewayHost: String, gatewayPort: UInt16) {
        self.host = NWEndpoint.Host(gatewayHost)
        self.port = NWEndpoint.Port(rawValue: gatewayPort)!
    }

    /// Runs the relay. `onBytesSent` is called on an arbitrary queue after
    /// each chunk — callers needing UI updates must hop back to the main
    /// actor themselves, matching Swift concurrency norms rather than this
    /// class making an assumption about its caller's context.
    func run(sessionId: String, token: String, onBytesSent: @escaping (Int) -> Void) async throws {
        isCancelled = false

        let tlsOptions = NWProtocolTLS.Options()
        // Reference-build trust handling: accepts the demo/self-signed
        // gateway cert the same way the desktop client's gen-cert.sh setup
        // does. Production should remove this verify block entirely and
        // rely on the platform default trust store with a real CA-issued
        // certificate — see the note in gateway-setup in ../../wireguard
        // and the desktop client's own cert-handling comments for the
        // same caveat stated there.
        sec_protocol_options_set_verify_block(
            tlsOptions.securityProtocolOptions,
            { _, _, complete in complete(true) },
            queue
        )

        let params = NWParameters(tls: tlsOptions, tcp: .init())
        let conn = NWConnection(host: host, port: port, using: params)
        self.connection = conn

        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
            var resumed = false
            conn.stateUpdateHandler = { state in
                switch state {
                case .ready:
                    if !resumed { resumed = true; continuation.resume() }
                case .failed(let error):
                    if !resumed { resumed = true; continuation.resume(throwing: RelayError.connectionFailed(error)) }
                case .cancelled:
                    if !resumed { resumed = true; continuation.resume(throwing: RelayError.cancelled) }
                default:
                    break
                }
            }
            conn.start(queue: queue)
        }

        try await sendLenPrefixed(sessionId, on: conn)
        try await sendLenPrefixed(token, on: conn)

        let chunk = Data(repeating: 0xAB, count: 32 * 1024)
        let chunksToSend = 320 // 10MB, matching the desktop/Android reference payload
        var totalSent = 0

        for _ in 0..<chunksToSend {
            if isCancelled { throw RelayError.cancelled }
            try await send(chunk, on: conn)
            totalSent += chunk.count
            onBytesSent(totalSent)
        }

        conn.cancel()
    }

    /// Called from the network-policy hard-interrupt path (see
    /// `PacketTunnelProvider`) — cancels the live connection immediately
    /// rather than waiting for the send loop to next check `isCancelled`
    /// on its own schedule.
    func cancel() {
        isCancelled = true
        connection?.cancel()
    }

    private func sendLenPrefixed(_ s: String, on conn: NWConnection) async throws {
        let bytes = Array(s.utf8)
        var lenPrefix = UInt16(bytes.count).bigEndian
        var data = Data(bytes: &lenPrefix, count: 2)
        data.append(contentsOf: bytes)
        try await send(data, on: conn)
    }

    private func send(_ data: Data, on conn: NWConnection) async throws {
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
            conn.send(content: data, completion: .contentProcessed { error in
                if let error = error {
                    continuation.resume(throwing: RelayError.connectionFailed(error))
                } else {
                    continuation.resume()
                }
            })
        }
    }
}
