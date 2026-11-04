import SwiftUI

/// Deliberately minimal — matches the Android `MainActivity`'s own scope
/// note. The one thing this view does that isn't optional: explains, in
/// plain language, that starting sharing will show as a VPN indicator in
/// iOS's status bar and Settings. That's a direct consequence of the
/// architecture (see `PacketTunnelProvider.swift`'s header comment) and
/// hiding it from the user would be exactly the kind of non-disclosure
/// the platform blueprint's opening section rules out.
struct ContentView: View {
    @StateObject private var tunnelManager = TunnelManager()
    @State private var errorMessage: String?

    // Reference/demo configuration — a real build would pull these from
    // app settings, matching the Android reference client's own
    // compile-time-constant caveat.
    private let demoConfig = TunnelManager.Config(
        controlPlaneURL: "http://127.0.0.1:8080",
        gatewayHost: "127.0.0.1",
        gatewayPort: "9443",
        userId: "user_demo_1",
        enterpriseClientId: "" // must be a real, KYB-approved enterprise_client id
    )

    var body: some View {
        VStack(spacing: 20) {
            Text(tunnelManager.statusText)
                .font(.title3)

            Text(
                "Starting sharing activates a VPN-style connection shown in " +
                "your status bar and in Settings ▸ VPN. This is how iOS " +
                "grants background apps a persistent network connection — " +
                "it is not routing your own browsing through anything."
            )
            .font(.footnote)
            .foregroundStyle(.secondary)
            .multilineTextAlignment(.center)
            .padding(.horizontal)

            if let errorMessage {
                Text(errorMessage)
                    .font(.footnote)
                    .foregroundStyle(.red)
            }

            Button(tunnelManager.isActive ? "Stop sharing" : "Start sharing") {
                Task { await toggle() }
            }
            .buttonStyle(.borderedProminent)
        }
        .padding()
    }

    private func toggle() async {
        errorMessage = nil
        if tunnelManager.isActive {
            tunnelManager.stop()
            return
        }
        do {
            try await tunnelManager.loadOrCreateConfiguration(demoConfig)
            try await tunnelManager.start()
        } catch {
            errorMessage = error.localizedDescription
        }
    }
}

#Preview {
    ContentView()
}
