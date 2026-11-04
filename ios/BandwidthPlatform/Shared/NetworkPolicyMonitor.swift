import Foundation
import Network
#if canImport(UIKit)
import UIKit
#endif

/// Enforces the same guarantee as the Android `NetworkPolicyMonitor` and the
/// desktop clients: sharing only happens on Wi-Fi, never cellular (including
/// personal-hotspot connections that present as Wi-Fi to the device but are
/// backed by a cellular plan), and only above a minimum battery level unless
/// charging. `NWPathMonitor`'s callback fires on every path change — this is
/// an OS push notification, not a poll, which is what makes the teardown in
/// `RelayCoordinator` a true hard interrupt rather than a best-effort check.
final class NetworkPolicyMonitor {
    private let monitor = NWPathMonitor()
    private let queue = DispatchQueue(label: "com.bandwidthplatform.network-policy")
    private let minBatteryPct: Float

    init(minBatteryPct: Float = 0.20) {
        self.minBatteryPct = minBatteryPct
        #if canImport(UIKit)
        UIDevice.current.isBatteryMonitoringEnabled = true
        #endif
    }

    func start(onEligible: @escaping () -> Void, onIneligible: @escaping (String) -> Void) {
        monitor.pathUpdateHandler = { [weak self] path in
            guard let self = self else { return }
            self.evaluate(path: path, onEligible: onEligible, onIneligible: onIneligible)
        }
        monitor.start(queue: queue)
    }

    func stop() {
        monitor.cancel()
    }

    private func evaluate(
        path: NWPath,
        onEligible: @escaping () -> Void,
        onIneligible: @escaping (String) -> Void
    ) {
        guard path.status == .satisfied else {
            onIneligible("no network connection")
            return
        }
        guard path.usesInterfaceType(.wifi) else {
            onIneligible("not on Wi-Fi")
            return
        }
        // Personal Hotspot connections and some constrained/expensive paths
        // present as Wi-Fi at the interface level but are backed by
        // cellular data — treat them the same as cellular, matching the
        // Android client's explicit handling of this exact case.
        if path.isExpensive || path.isConstrained {
            onIneligible("network is metered (e.g. personal hotspot)")
            return
        }
        if !isBatteryOk() {
            onIneligible("battery below \(Int(minBatteryPct * 100))% and not charging")
            return
        }
        onEligible()
    }

    private func isBatteryOk() -> Bool {
        #if canImport(UIKit)
        let level = UIDevice.current.batteryLevel // -1.0 if monitoring unavailable (e.g. simulator)
        let state = UIDevice.current.batteryState
        if level < 0 { return true } // can't determine — fail open rather than never running at all
        return state == .charging || state == .full || level >= minBatteryPct
        #else
        return true
        #endif
    }
}
