import Foundation

/// Talks to the exact same control-plane API already proven working against
/// the Go control-plane, the Rust desktop client, and the Android client.
/// Uses plain `URLSession` — no third-party networking dependency, matching
/// the "no unnecessary weight" principle followed by every other client in
/// this project.
final class ControlPlaneClient {
    struct NodeRegistration { let id: String }
    struct SessionIssued { let sessionId: String; let token: String }

    struct ControlPlaneError: Error, LocalizedError {
        let httpStatus: Int
        let body: String
        var errorDescription: String? { "control plane returned \(httpStatus): \(body)" }
    }

    private let baseURL: URL
    private let session: URLSession

    init(baseURL: URL, session: URLSession = .shared) {
        self.baseURL = baseURL
        self.session = session
    }

    func registerNode(userId: String, deviceFingerprint: String) async throws -> NodeRegistration {
        let body: [String: Any] = ["UserID": userId, "DeviceFingerprint": deviceFingerprint]
        let json = try await post(path: "/v1/nodes/register", body: body)
        guard let id = json["ID"] as? String else {
            throw ControlPlaneError(httpStatus: 200, body: "missing ID field in response")
        }
        return NodeRegistration(id: id)
    }

    /// Requests a session for (nodeId, enterpriseClientId). Fails closed —
    /// a thrown error here means the enterprise client isn't KYB-approved
    /// or this node isn't eligible, and the caller must not attempt to
    /// relay anything.
    func createSession(nodeId: String, enterpriseClientId: String) async throws -> SessionIssued {
        let body: [String: Any] = ["NodeID": nodeId, "EnterpriseClientID": enterpriseClientId]
        let json = try await post(path: "/v1/sessions", body: body)
        guard let sessionId = json["session_id"] as? String, let token = json["token"] as? String else {
            throw ControlPlaneError(httpStatus: 200, body: "missing session_id/token in response")
        }
        return SessionIssued(sessionId: sessionId, token: token)
    }

    private func post(path: String, body: [String: Any]) async throws -> [String: Any] {
        var request = URLRequest(url: baseURL.appendingPathComponent(path))
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONSerialization.data(withJSONObject: body)
        request.timeoutInterval = 5

        let (data, response) = try await session.data(for: request)
        guard let http = response as? HTTPURLResponse else {
            throw ControlPlaneError(httpStatus: 0, body: "non-HTTP response")
        }
        guard (200...299).contains(http.statusCode) else {
            let text = String(data: data, encoding: .utf8) ?? "<unreadable body>"
            throw ControlPlaneError(httpStatus: http.statusCode, body: text)
        }
        guard let json = try JSONSerialization.jsonObject(with: data) as? [String: Any] else {
            throw ControlPlaneError(httpStatus: http.statusCode, body: "non-JSON response")
        }
        return json
    }
}
