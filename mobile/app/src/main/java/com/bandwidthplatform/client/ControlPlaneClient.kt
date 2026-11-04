package com.bandwidthplatform.client

import org.json.JSONObject
import java.io.BufferedReader
import java.io.InputStreamReader
import java.io.OutputStreamWriter
import java.net.HttpURLConnection
import java.net.URL

/**
 * Minimal control-plane client — deliberately dependency-free (no
 * Retrofit/OkHttp) to mirror the same "no unnecessary weight" principle
 * the desktop Rust client follows. Talks to the same control-plane API
 * already proven working against the Go control-plane and Rust client.
 */
class ControlPlaneClient(private val baseUrl: String) {

    data class NodeRegistration(val id: String)
    data class SessionIssued(val sessionId: String, val token: String)

    fun registerNode(userId: String, deviceFingerprint: String): NodeRegistration {
        val body = JSONObject().apply {
            put("UserID", userId)
            put("DeviceFingerprint", deviceFingerprint)
        }
        val resp = post("/v1/nodes/register", body)
        return NodeRegistration(id = resp.getString("ID"))
    }

    /**
     * Requests a session for (nodeId, enterpriseClientId). This call fails
     * closed — an IOException/non-2xx here means the enterprise client
     * isn't KYB-approved or this node isn't eligible, and the caller must
     * not attempt to relay anything.
     */
    fun createSession(nodeId: String, enterpriseClientId: String): SessionIssued {
        val body = JSONObject().apply {
            put("NodeID", nodeId)
            put("EnterpriseClientID", enterpriseClientId)
        }
        val resp = post("/v1/sessions", body)
        return SessionIssued(
            sessionId = resp.getString("session_id"),
            token = resp.getString("token")
        )
    }

    private fun post(path: String, body: JSONObject): JSONObject {
        val conn = URL(baseUrl + path).openConnection() as HttpURLConnection
        conn.requestMethod = "POST"
        conn.doOutput = true
        conn.setRequestProperty("Content-Type", "application/json")
        conn.connectTimeout = 5000
        conn.readTimeout = 5000

        OutputStreamWriter(conn.outputStream).use { it.write(body.toString()) }

        val status = conn.responseCode
        val stream = if (status in 200..299) conn.inputStream else conn.errorStream
        val responseText = BufferedReader(InputStreamReader(stream)).use { it.readText() }

        if (status !in 200..299) {
            throw ControlPlaneException(status, responseText)
        }
        return JSONObject(responseText)
    }

    class ControlPlaneException(val httpStatus: Int, val body: String) :
        Exception("control plane returned $httpStatus: $body")
}
