package com.bandwidthplatform.client

import java.io.DataOutputStream
import java.net.Socket
import java.security.cert.X509Certificate
import javax.net.ssl.SSLContext
import javax.net.ssl.SSLSocket
import javax.net.ssl.X509TrustManager

/**
 * Relays through the gateway using the exact same wire protocol as the
 * desktop Rust client (client/rust-relay) and the Go gateway it talks to:
 * a length-prefixed session_id, a length-prefixed token, then raw bytes.
 * Deliberately kept protocol-compatible with the already-tested desktop
 * client rather than inventing a parallel protocol for mobile.
 *
 * Like the current gateway implementation (see gateway/main.go), this is
 * still the MVP-scope version: it sends a payload rather than bridging
 * real bidirectional enterprise traffic, matching where the rest of this
 * reference implementation currently stops. Production would replace the
 * payload-sending loop with genuine bidirectional forwarding once the
 * gateway's own bridging logic is built out past the metering proof.
 *
 * Trust model note: pins to a single provided certificate for this
 * reference build (matching the desktop client's local-demo cert
 * handling) — production should use a real CA-issued cert and the
 * platform default trust store instead of a custom TrustManager.
 */
class RelaySession(
    private val gatewayHost: String,
    private val gatewayPort: Int,
    private val pinnedCert: X509Certificate,
) {
    @Volatile private var socket: Socket? = null
    @Volatile private var cancelled = false

    fun run(sessionId: String, token: String, onBytesSent: (Long) -> Unit) {
        cancelled = false
        val trustManager = object : X509TrustManager {
            override fun checkClientTrusted(chain: Array<out X509Certificate>?, authType: String?) {}
            override fun checkServerTrusted(chain: Array<out X509Certificate>?, authType: String?) {
                val presented = chain?.getOrNull(0) ?: throw SecurityException("no server cert presented")
                if (presented != pinnedCert) throw SecurityException("gateway cert does not match pinned cert")
            }
            override fun getAcceptedIssuers(): Array<X509Certificate> = arrayOf(pinnedCert)
        }
        val sslContext = SSLContext.getInstance("TLS")
        sslContext.init(null, arrayOf(trustManager), null)

        val raw = Socket(gatewayHost, gatewayPort)
        raw.soTimeout = 15000
        val tls = sslContext.socketFactory.createSocket(raw, gatewayHost, gatewayPort, true) as SSLSocket
        socket = tls
        tls.startHandshake()

        val out = DataOutputStream(tls.outputStream)
        writeLenPrefixed(out, sessionId)
        writeLenPrefixed(out, token)

        val chunk = ByteArray(32 * 1024) { 0xAB.toByte() }
        var totalSent = 0L
        val chunksToSend = 320 // 10MB, matching the desktop client's proof payload

        for (i in 0 until chunksToSend) {
            if (cancelled) break // network-policy teardown — see RelayForegroundService
            out.write(chunk)
            totalSent += chunk.size
            onBytesSent(totalSent)
        }
        out.flush()
        tls.close()
    }

    /**
     * Called from the network-policy hard-interrupt path — must close the
     * socket immediately, not wait for the current loop iteration to poll
     * a flag on its own schedule. The [cancelled] check above is a
     * best-effort courtesy; this close() is what actually guarantees the
     * teardown happens now.
     */
    fun cancel() {
        cancelled = true
        try { socket?.close() } catch (_: Exception) {}
    }

    private fun writeLenPrefixed(out: DataOutputStream, s: String) {
        val bytes = s.toByteArray(Charsets.UTF_8)
        out.writeShort(bytes.size)
        out.write(bytes)
    }
}
