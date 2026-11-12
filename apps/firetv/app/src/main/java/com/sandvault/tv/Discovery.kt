package com.sandvault.tv

import android.content.Context
import android.net.wifi.WifiManager
import org.json.JSONArray
import org.json.JSONObject
import java.net.DatagramPacket
import java.net.DatagramSocket
import java.net.InetAddress
import java.net.NetworkInterface
import java.net.SocketTimeoutException

/**
 * Finds SAND servers on the local network.
 *
 * The counterpart of internal/tvdiscover on the server: broadcast the query on
 * the discovery port, and collect the answers that arrive in the next couple of
 * seconds. Each answer names the web server's port; the host is wherever the
 * answer came from.
 */
class Discovery(private val context: Context) {

    data class Server(val name: String, val host: String, val port: Int, val secure: Boolean, val version: String) {
        val origin: String
            get() = "${if (secure) "https" else "http"}://$host:$port"
    }

    /** Blocks for up to [timeoutMs]; call it off the main thread. */
    fun find(timeoutMs: Int = 2000): List<Server> {
        val wifi = context.applicationContext.getSystemService(Context.WIFI_SERVICE) as? WifiManager
        val lock = wifi?.createMulticastLock("sand-tv-discovery")?.apply {
            setReferenceCounted(false)
            acquire()
        }
        try {
            DatagramSocket().use { socket ->
                socket.broadcast = true
                socket.soTimeout = 250
                val query = QUERY.toByteArray(Charsets.UTF_8)
                for (target in broadcastAddresses()) {
                    try {
                        socket.send(DatagramPacket(query, query.size, target, PORT))
                    } catch (_: Exception) {
                        // One interface refusing a broadcast is no reason not to try the rest.
                    }
                }

                val found = LinkedHashMap<String, Server>()
                val buf = ByteArray(2048)
                val deadline = System.currentTimeMillis() + timeoutMs
                while (System.currentTimeMillis() < deadline) {
                    val packet = DatagramPacket(buf, buf.size)
                    try {
                        socket.receive(packet)
                    } catch (_: SocketTimeoutException) {
                        continue
                    }
                    val server = parse(String(packet.data, 0, packet.length, Charsets.UTF_8), packet.address) ?: continue
                    found[server.origin] = server
                }
                return found.values.toList()
            }
        } finally {
            lock?.release()
        }
    }

    private fun parse(body: String, from: InetAddress): Server? = try {
        val json = JSONObject(body)
        val port = json.optInt("port", 0)
        if (json.optString("service") != SERVICE || port !in 1..65535) {
            null
        } else {
            val host = from.hostAddress ?: return null
            Server(
                name = json.optString("name").ifBlank { host },
                host = host,
                port = port,
                secure = json.optBoolean("secure", false),
                version = json.optString("version"),
            )
        }
    } catch (_: Exception) {
        null
    }

    /**
     * Every interface's own broadcast address, plus the limited broadcast
     * address, which some routers forward and some drop.
     */
    private fun broadcastAddresses(): List<InetAddress> {
        val out = LinkedHashSet<InetAddress>()
        try {
            val ifaces = NetworkInterface.getNetworkInterfaces()
            while (ifaces != null && ifaces.hasMoreElements()) {
                val iface = ifaces.nextElement()
                if (!iface.isUp || iface.isLoopback) continue
                for (addr in iface.interfaceAddresses) {
                    addr.broadcast?.let { out.add(it) }
                }
            }
        } catch (_: Exception) {
            // Fall through to the limited broadcast address alone.
        }
        out.add(InetAddress.getByName("255.255.255.255"))
        return out.toList()
    }

    companion object {
        // These three must match internal/tvdiscover.
        const val PORT = 8124
        const val QUERY = "SAND-TV-DISCOVER/1"
        const val SERVICE = "sand-vault"

        fun toJson(servers: List<Server>): String {
            val arr = JSONArray()
            for (s in servers) {
                arr.put(
                    JSONObject()
                        .put("name", s.name)
                        .put("origin", s.origin)
                        .put("version", s.version),
                )
            }
            return arr.toString()
        }
    }
}
