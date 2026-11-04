package com.bandwidthplatform.client

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Intent
import android.os.Build
import android.os.IBinder
import androidx.core.app.NotificationCompat
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.launch
import java.security.cert.CertificateFactory
import java.security.cert.X509Certificate

/**
 * The always-running background component. Per the platform blueprint
 * §5.1: runs as a foreground service with a persistent, honest
 * notification (never hidden or disguised — that's the non-deceptive UX
 * pattern this category of app is expected to follow), and hard-stops
 * relaying the moment [NetworkPolicyMonitor] reports ineligibility.
 */
class RelayForegroundService : Service() {

    companion object {
        const val CHANNEL_ID = "bandwidth_sharing_status"
        const val NOTIFICATION_ID = 1
        const val ACTION_STOP = "com.bandwidthplatform.client.STOP"

        // Reference/demo configuration — a real build would pull these
        // from app settings, not compile-time constants.
        const val CONTROL_PLANE_URL = "http://10.0.2.2:8080" // host machine from Android emulator
        const val GATEWAY_HOST = "10.0.2.2"
        const val GATEWAY_PORT = 9443
        const val DEMO_USER_ID = "user_demo_1"
        const val DEMO_ENTERPRISE_ID = "" // must be a real, KYB-approved enterprise_client id
    }

    private val serviceScope = CoroutineScope(Dispatchers.IO + Job())
    private lateinit var networkMonitor: NetworkPolicyMonitor
    private var currentRelay: RelaySession? = null
    private var isEligible = false
    private var totalBytesSentThisSession = 0L

    override fun onCreate() {
        super.onCreate()
        createNotificationChannel()
        networkMonitor = NetworkPolicyMonitor(this)
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            stopSharing("user requested stop")
            stopSelf()
            return START_NOT_STICKY
        }

        startForeground(NOTIFICATION_ID, buildNotification("Waiting for Wi-Fi…"))

        networkMonitor.start(
            onEligible = { onNetworkEligible() },
            onIneligible = { reason -> onNetworkIneligible(reason) },
        )

        return START_STICKY
    }

    private fun onNetworkEligible() {
        if (isEligible) return // already running — don't start a duplicate relay
        isEligible = true
        updateNotification("Connected — sharing on Wi-Fi")
        startRelayLoop()
    }

    private fun onNetworkIneligible(reason: String) {
        isEligible = false
        // This is the hard interrupt: tear down the ACTIVE socket right
        // now. Not "stop starting new sessions" — actively close whatever
        // is in flight, per the blueprint's explicit requirement that this
        // never be a polled/best-effort check.
        currentRelay?.cancel()
        updateNotification("Paused — $reason")
    }

    private fun startRelayLoop() {
        serviceScope.launch {
            try {
                val controlPlane = ControlPlaneClient(CONTROL_PLANE_URL)
                val deviceFingerprint = "android-" + Build.FINGERPRINT.hashCode()
                val node = controlPlane.registerNode(DEMO_USER_ID, deviceFingerprint)

                if (DEMO_ENTERPRISE_ID.isEmpty()) {
                    updateNotification("Reference build: set DEMO_ENTERPRISE_ID to a KYB-approved client")
                    return@launch
                }

                val session = controlPlane.createSession(node.id, DEMO_ENTERPRISE_ID)

                val cert = loadPinnedCert() ?: run {
                    updateNotification("No pinned gateway cert bundled — see mobile/README.md")
                    return@launch
                }

                val relay = RelaySession(GATEWAY_HOST, GATEWAY_PORT, cert)
                currentRelay = relay
                relay.run(session.sessionId, session.token) { bytesSent ->
                    totalBytesSentThisSession = bytesSent
                    updateNotification("Sharing — ${bytesSent / 1024} KB this session")
                }
            } catch (e: ControlPlaneClient.ControlPlaneException) {
                // Most likely cause: enterprise client not KYB-approved, or
                // this node not yet eligible — surfaced honestly rather
                // than silently retried forever.
                updateNotification("Paused — ${e.message}")
            } catch (e: Exception) {
                updateNotification("Paused — connection issue")
            }
        }
    }

    private fun loadPinnedCert(): X509Certificate? {
        return try {
            assets.open("gateway-cert.pem").use { input ->
                CertificateFactory.getInstance("X.509").generateCertificate(input) as X509Certificate
            }
        } catch (e: Exception) {
            null
        }
    }

    private fun stopSharing(reason: String) {
        isEligible = false
        currentRelay?.cancel()
        networkMonitor.stop()
    }

    override fun onDestroy() {
        stopSharing("service destroyed")
        super.onDestroy()
    }

    override fun onBind(intent: Intent?): IBinder? = null

    private fun createNotificationChannel() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val channel = NotificationChannel(
                CHANNEL_ID, "Bandwidth sharing status", NotificationManager.IMPORTANCE_LOW,
            ).apply {
                description = "Shows whether bandwidth sharing is currently active"
            }
            getSystemService(NotificationManager::class.java).createNotificationChannel(channel)
        }
    }

    private fun buildNotification(text: String): Notification {
        val stopIntent = Intent(this, RelayForegroundService::class.java).apply { action = ACTION_STOP }
        val stopPendingIntent = PendingIntent.getService(
            this, 0, stopIntent, PendingIntent.FLAG_IMMUTABLE,
        )
        return NotificationCompat.Builder(this, CHANNEL_ID)
            .setContentTitle("Bandwidth Platform")
            .setContentText(text) // always the honest current state — never disguised or hidden
            .setSmallIcon(android.R.drawable.stat_sys_upload)
            .setOngoing(true)
            .addAction(android.R.drawable.ic_media_pause, "Pause", stopPendingIntent)
            .build()
    }

    private fun updateNotification(text: String) {
        val manager = getSystemService(NotificationManager::class.java)
        manager.notify(NOTIFICATION_ID, buildNotification(text))
    }
}
