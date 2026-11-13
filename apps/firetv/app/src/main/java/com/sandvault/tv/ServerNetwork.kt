package com.sandvault.tv

import org.json.JSONObject
import java.net.HttpURLConnection
import java.net.URL

/**
 * Asks SAND how the server is connected — the link of the interface the film
 * goes out through, and how fast it is sending (GET /api/stream/{ticket}/network)
 * — for the Stats panel to set beside the TV's own link.
 *
 * It blocks, so it is called off the main thread. Anything that goes wrong —
 * no answer in time, a server too old to know the question, a ticket that has
 * gone — is an answer it did not get, not an error.
 */
internal fun fetchServerLink(url: String): StatsText.ServerLink? {
    val conn = try {
        URL(url).openConnection() as HttpURLConnection
    } catch (_: Exception) {
        return null
    }
    return try {
        conn.connectTimeout = SERVER_LINK_TIMEOUT_MS
        conn.readTimeout = SERVER_LINK_TIMEOUT_MS
        conn.useCaches = false
        if (conn.responseCode != HttpURLConnection.HTTP_OK) return null
        val json = JSONObject(conn.inputStream.bufferedReader().use { it.readText() })
        StatsText.ServerLink(
            kind = StatsText.serverKind(json.optString("kind")),
            name = json.optString("interface"),
            linkMbps = json.optInt("link_mbps"),
            frequencyMhz = json.optInt("frequency_mhz"),
            signalDbm = json.optInt("signal_dbm"),
            sendBitsPerSecond = if (json.has("send_bps")) json.optLong("send_bps") else null,
        )
    } catch (_: Exception) {
        null
    } finally {
        conn.disconnect()
    }
}

/** Long enough for a server across a busy network, short enough not to hold up the next question. */
private const val SERVER_LINK_TIMEOUT_MS = 3_000
