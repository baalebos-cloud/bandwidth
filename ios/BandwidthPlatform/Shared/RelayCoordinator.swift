import Foundation

/// Orchestrates the same sequence as Android's `RelayForegroundService`:
/// watch network/battery eligibility, register with the control plane,
/// request a session, run the relay, and tear down immediately (not just
/// "stop starting new ones") the moment eligibility is lost. Shared
/// between the tunnel extension (where this actually runs in production —
/// see `PacketTunnelProvider`) and the main app (which can drive it for
/// local testing/preview without installing the extension).
actor RelayCoordinator {
    struct Config {
        let controlPlaneURL: URL
        let gatewayHost: String
        let gatewayPort: UInt16
        let userId: String
        let deviceFingerprint: String
        let enterpriseClientId: String
    }

    private let config: Config
    private let controlPlane: ControlPlaneClient
    private var monitor: NetworkPolicyMonitor?
    private var currentRelay: RelaySession?
    private var isEligible = false
    private var statusHandler: ((String) -> Void)?

    init(config: Config) {
        self.config = config
        self.controlPlane = ControlPlaneClient(baseURL: config.controlPlaneURL)
    }

    func start(onStatus: @escaping (String) -> Void) {
        statusHandler = onStatus
        let m = NetworkPolicyMonitor()
        monitor = m
        m.start(
            onEligible: { [weak self] in Task { await self?.handleEligible() } },
            onIneligible: { [weak self] reason in Task { await self?.handleIneligible(reason) } }
        )
    }

    func stop() {
        monitor?.stop()
        monitor = nil
        currentRelay?.cancel()
        currentRelay = nil
        isEligible = false
    }

    private func handleEligible() async {
        guard !isEligible else { return } // already running — no duplicate relay
        isEligible = true
        report("Connected — sharing on Wi-Fi")
        await runRelayLoop()
    }

    private func handleIneligible(_ reason: String) async {
        isEligible = false
        // Hard interrupt: cancel whatever relay is actually in flight right
        // now, not just "don't start a new one." This is the same
        // non-negotiable requirement as the Android and desktop clients.
        currentRelay?.cancel()
        currentRelay = nil
        report("Paused — \(reason)")
    }

    private func runRelayLoop() async {
        do {
            let node = try await controlPlane.registerNode(
                userId: config.userId, deviceFingerprint: config.deviceFingerprint
            )
            let session = try await controlPlane.createSession(
                nodeId: node.id, enterpriseClientId: config.enterpriseClientId
            )

            let relay = RelaySession(gatewayHost: config.gatewayHost, gatewayPort: config.gatewayPort)
            currentRelay = relay
            try await relay.run(sessionId: session.sessionId, token: session.token) { [weak self] bytesSent in
                // onBytesSent fires on RelaySession's own dispatch queue, not
                // as an actor-isolated call — hop back onto the actor rather
                // than touching actor-isolated state directly from here.
                Task { await self?.report("Sharing — \(bytesSent / 1024) KB this session") }
            }
        } catch is RelaySession.RelayError {
            report("Paused — connection issue")
        } catch let error as ControlPlaneClient.ControlPlaneError {
            // Most likely cause: enterprise client not KYB-approved, or this
            // node not yet eligible — surfaced honestly, not silently
            // retried forever.
            report("Paused — \(error.localizedDescription)")
        } catch {
            report("Paused — unexpected error")
        }
    }

    private func report(_ status: String) {
        statusHandler?(status)
    }
}
