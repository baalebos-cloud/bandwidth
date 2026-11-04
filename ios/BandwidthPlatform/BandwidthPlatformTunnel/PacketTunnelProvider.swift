import NetworkExtension
import os.log

/// This is the real architectural answer to "why can't this just be a
/// normal background app on iOS," which the platform blueprint originally
/// deferred (§1.2: "iOS deliberately out of scope for v1... no persistent
/// background sockets, aggressive suspension, App Store policy explicitly
/// forbids proxy/VPN-selling apps that aren't a declared Network
/// Extension").
///
/// A standard iOS app has no background mode that fits "keep an arbitrary
/// outbound TLS socket open indefinitely and relay bytes for a third
/// party" — iOS suspends it within seconds of backgrounding. The one
/// sanctioned mechanism that grants a process extended background
/// execution tied to a live network connection is a Network Extension
/// activated as a VPN configuration (`NETunnelProviderManager`), which is
/// exactly what this class is. This is also, pragmatically, what other
/// apps in this same category (background bandwidth/proxy relay) actually
/// ship on iOS.
///
/// Important honesty note: `NEPacketTunnelProvider` is designed to tunnel
/// the DEVICE'S OWN traffic through a remote endpoint (that's what "VPN"
/// means to iOS and to the user, and it's presented to the user as a VPN
/// toggle in Settings). This app's actual purpose is different — the
/// device is an EGRESS relay for enterprise traffic brokered by the
/// gateway, not tunneling its own traffic anywhere. To stay honest with
/// the user and technically correct, `startTunnel` configures the tunnel
/// with settings that leave the device's own traffic essentially
/// untouched (an unused, non-routed tunnel address space) — the extension
/// process's job is really just "exist persistently in the background and
/// run RelayCoordinator," using the VPN-grade background execution as the
/// mechanism, not using the packet-tunneling capability itself. This
/// distinction MUST be disclosed to the user during onboarding (this is
/// exactly the kind of "informed consent" the blueprint's opening section
/// insists on) — see `ContentView.swift`'s consent copy.
final class PacketTunnelProvider: NEPacketTunnelProvider {
    private var coordinator: RelayCoordinator?
    private let log = OSLog(subsystem: "com.bandwidthplatform.tunnel", category: "PacketTunnelProvider")

    override func startTunnel(options: [String: NSObject]?, completionHandler: @escaping (Error?) -> Void) {
        // Minimal, non-disruptive tunnel settings — see the class-level
        // note above for why this deliberately does NOT route the
        // device's real traffic anywhere.
        let settings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: "127.0.0.1")
        settings.ipv4Settings = NEIPv4Settings(addresses: ["192.0.2.1"], subnetMasks: ["255.255.255.0"])
        settings.ipv4Settings?.includedRoutes = [] // explicitly do not capture device traffic
        settings.mtu = 1500

        setTunnelNetworkSettings(settings) { [weak self] error in
            guard let self = self else { return }
            if let error = error {
                completionHandler(error)
                return
            }

            guard
                let config = self.loadConfig(from: options),
                let userId = config["userId"], let enterpriseClientId = config["enterpriseClientId"],
                let controlPlaneURLString = config["controlPlaneURL"], let controlPlaneURL = URL(string: controlPlaneURLString),
                let gatewayHost = config["gatewayHost"]
            else {
                completionHandler(NSError(
                    domain: "com.bandwidthplatform.tunnel", code: 1,
                    userInfo: [NSLocalizedDescriptionKey: "missing required tunnel configuration"]
                ))
                return
            }

            let coordinatorConfig = RelayCoordinator.Config(
                controlPlaneURL: controlPlaneURL,
                gatewayHost: gatewayHost,
                gatewayPort: UInt16(config["gatewayPort"] ?? "9443") ?? 9443,
                userId: userId,
                deviceFingerprint: "ios-" + (self.protocolConfiguration.serverAddress ?? UUID().uuidString),
                enterpriseClientId: enterpriseClientId
            )
            let coordinator = RelayCoordinator(config: coordinatorConfig)
            self.coordinator = coordinator

            Task {
                await coordinator.start { status in
                    os_log("relay status: %{public}@", log: self.log, status)
                }
            }

            completionHandler(nil)
        }
    }

    override func stopTunnel(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        guard let coordinator = coordinator else {
            completionHandler()
            return
        }
        Task {
            await coordinator.stop()
            completionHandler()
        }
    }

    /// Reads configuration passed down from the main app via
    /// `NETunnelProviderProtocol.providerConfiguration` — this is the
    /// standard mechanism for app-to-extension configuration on iOS
    /// (there's no shared process memory between them).
    private func loadConfig(from options: [String: NSObject]?) -> [String: String]? {
        if let fromOptions = options?["config"] as? [String: String] {
            return fromOptions
        }
        guard let proto = protocolConfiguration as? NETunnelProviderProtocol,
              let stored = proto.providerConfiguration as? [String: String]
        else { return nil }
        return stored
    }
}
