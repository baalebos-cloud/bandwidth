import Foundation
import NetworkExtension

/// The main app never runs the relay itself — it only installs and
/// toggles the `NETunnelProviderManager` VPN configuration that activates
/// `PacketTunnelProvider` (see that file for why this mechanism is
/// required on iOS at all). This mirrors how any iOS VPN-style app is
/// structured: a lightweight app target for configuration/UI, and an
/// extension target that actually runs in the background.
@MainActor
final class TunnelManager: ObservableObject {
    @Published var isActive = false
    @Published var statusText = "Not sharing"

    private var manager: NETunnelProviderManager?

    struct Config {
        let controlPlaneURL: String
        let gatewayHost: String
        let gatewayPort: String
        let userId: String
        let enterpriseClientId: String
    }

    func loadOrCreateConfiguration(_ config: Config) async throws {
        let managers = try await NETunnelProviderManager.loadAllFromPreferences()
        let mgr = managers.first ?? NETunnelProviderManager()

        let proto = NETunnelProviderProtocol()
        proto.providerBundleIdentifier = "com.bandwidthplatform.client.tunnel"
        proto.serverAddress = config.gatewayHost
        proto.providerConfiguration = [
            "controlPlaneURL": config.controlPlaneURL,
            "gatewayHost": config.gatewayHost,
            "gatewayPort": config.gatewayPort,
            "userId": config.userId,
            "enterpriseClientId": config.enterpriseClientId,
        ]

        mgr.protocolConfiguration = proto
        mgr.localizedDescription = "Bandwidth Platform"
        mgr.isEnabled = true

        try await mgr.saveToPreferences()
        try await mgr.loadFromPreferences() // reload to get a fully-populated manager after saving
        self.manager = mgr
    }

    func start() async throws {
        guard let manager = manager else {
            throw NSError(domain: "com.bandwidthplatform.client", code: 1,
                           userInfo: [NSLocalizedDescriptionKey: "call loadOrCreateConfiguration first"])
        }
        try manager.connection.startVPNTunnel()
        isActive = true
        statusText = "Starting…"
    }

    func stop() {
        manager?.connection.stopVPNTunnel()
        isActive = false
        statusText = "Not sharing"
    }
}
