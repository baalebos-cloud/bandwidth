package com.bandwidthplatform.client

import android.content.Context
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.os.BatteryManager

/**
 * Enforces the platform's core trust guarantee on the client side: sharing
 * only ever happens on Wi-Fi/ethernet, never cellular (including metered
 * "Wi-Fi" like mobile hotspots), and only above a minimum battery level
 * unless charging.
 *
 * Critically, [onIneligible] fires immediately on a network-capability
 * change — this is registered as an OS callback, not polled — because the
 * one thing this platform can never do is let a user discover after the
 * fact that their cellular data was silently consumed. See the platform
 * blueprint §3.3 ("Smart Network Controls") for why this is a hard
 * interrupt rather than a periodic check.
 */
class NetworkPolicyMonitor(
    private val context: Context,
    private val minBatteryPct: Int = 20,
) {
    private val connectivityManager =
        context.getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager
    private val batteryManager =
        context.getSystemService(Context.BATTERY_SERVICE) as BatteryManager

    private var callback: ConnectivityManager.NetworkCallback? = null

    fun start(onEligible: () -> Unit, onIneligible: (reason: String) -> Unit) {
        val request = NetworkRequest.Builder()
            .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
            .build()

        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) {
                evaluate(network, onEligible, onIneligible)
            }

            override fun onLost(network: Network) {
                onIneligible("network lost")
            }

            override fun onCapabilitiesChanged(network: Network, caps: NetworkCapabilities) {
                // This fires on ANY capability change on the active network,
                // including a phone silently falling back from Wi-Fi to
                // cellular — that's exactly the case this exists to catch,
                // and it must tear down immediately, not on the next
                // heartbeat interval.
                evaluate(network, onEligible, onIneligible)
            }
        }
        callback = cb
        connectivityManager.registerNetworkCallback(request, cb)
    }

    fun stop() {
        callback?.let { connectivityManager.unregisterNetworkCallback(it) }
        callback = null
    }

    private fun evaluate(
        network: Network,
        onEligible: () -> Unit,
        onIneligible: (String) -> Unit,
    ) {
        val caps = connectivityManager.getNetworkCapabilities(network)
        val isWifiOrEthernet = caps?.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) == true ||
            caps?.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET) == true
        val isMetered = !(caps?.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_METERED) ?: false)

        if (!isWifiOrEthernet) {
            onIneligible("not on Wi-Fi/ethernet")
            return
        }
        if (isMetered) {
            // Covers mobile hotspots that present as Wi-Fi but are actually
            // backed by a cellular data plan — treated the same as
            // cellular, per the blueprint's explicit callout of this case.
            onIneligible("network is metered (e.g. mobile hotspot)")
            return
        }
        if (!isBatteryOk()) {
            onIneligible("battery below ${minBatteryPct}% and not charging")
            return
        }
        onEligible()
    }

    private fun isBatteryOk(): Boolean {
        val pct = batteryManager.getIntProperty(BatteryManager.BATTERY_PROPERTY_CAPACITY)
        val isCharging = batteryManager.getIntProperty(BatteryManager.BATTERY_PROPERTY_STATUS) ==
            BatteryManager.BATTERY_STATUS_CHARGING
        return isCharging || pct >= minBatteryPct
    }
}
