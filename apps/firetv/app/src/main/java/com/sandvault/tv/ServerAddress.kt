package com.sandvault.tv

/**
 * Turns what somebody typed with a remote into the origin of a SAND server.
 *
 * People type "192.168.1.20", "192.168.1.20:8123", "http://nas.local:8123/" or
 * the address the owner's dialog showed them with "/tv/" on the end. All of
 * those mean the same server, and the app wants one spelling of it: a scheme,
 * a host and a port, with no path — the TV page is always at /tv/ beneath it.
 */
object ServerAddress {
    /** The port `sand serve` listens on unless told otherwise. */
    const val DEFAULT_PORT = 8123

    /**
     * The normalized origin, or null when the text cannot be an address at all.
     */
    fun normalize(typed: String): String? {
        var text = typed.trim()
        if (text.isEmpty()) return null

        var scheme = "http"
        val sep = text.indexOf("://")
        if (sep >= 0) {
            scheme = text.substring(0, sep).lowercase()
            if (scheme != "http" && scheme != "https") return null
            text = text.substring(sep + 3)
        }

        // Anything after the host and port — "/tv/", "/" — is dropped.
        val slash = text.indexOf('/')
        if (slash >= 0) text = text.substring(0, slash)
        if (text.isEmpty()) return null

        val host: String
        var port: Int? = null
        if (text.startsWith("[")) {
            // An IPv6 literal, [::1]:8123.
            val close = text.indexOf(']')
            if (close < 0) return null
            host = text.substring(0, close + 1)
            val rest = text.substring(close + 1)
            if (rest.isNotEmpty()) {
                if (!rest.startsWith(":")) return null
                port = rest.substring(1).toIntOrNull() ?: return null
            }
        } else {
            val colon = text.lastIndexOf(':')
            if (colon >= 0) {
                host = text.substring(0, colon)
                port = text.substring(colon + 1).toIntOrNull() ?: return null
            } else {
                host = text
            }
        }

        if (host.isEmpty() || host.any { it.isWhitespace() }) return null
        val p = port ?: if (scheme == "https" && sep >= 0) 443 else DEFAULT_PORT
        if (p !in 1..65535) return null

        val defaultForScheme = (scheme == "http" && p == 80) || (scheme == "https" && p == 443)
        return if (defaultForScheme) "$scheme://${host.lowercase()}" else "$scheme://${host.lowercase()}:$p"
    }

    /** The address of the TV page on a normalized origin. */
    fun tvPage(origin: String): String = "$origin/tv/"

    /**
     * Where the server says how it is connected, for the film at a stream link:
     * http://nas:8123/stream/TICKET/Film.mkv → http://nas:8123/api/stream/TICKET/network.
     * The ticket is the credential there, as it is for the film. Null for
     * anything that is not a stream link.
     */
    fun networkUrl(streamUrl: String): String? {
        val sep = streamUrl.indexOf("://")
        if (sep < 0) return null
        val pathAt = streamUrl.indexOf('/', sep + 3)
        if (pathAt < 0) return null
        val authority = streamUrl.substring(sep + 3, pathAt)
        if (authority.isEmpty() || authority.any { it == '?' || it == '#' }) return null
        val parts = streamUrl.substring(pathAt).substringBefore('?').substringBefore('#').split('/')
        if (parts.size < 3 || parts[0].isNotEmpty() || parts[1] != "stream") return null
        val ticket = parts[2]
        if (ticket.isEmpty() || !ticket.all { it in 'A'..'Z' || it in 'a'..'z' || it in '0'..'9' || it == '-' || it == '_' }) {
            return null
        }
        return "${streamUrl.substring(0, pathAt)}/api/stream/$ticket/network"
    }

    /**
     * Whether a URL the WebView is about to open belongs to the server or to
     * the app's own pages. Anything else is refused, because the page the
     * WebView is showing is handed the native bridge.
     */
    fun isOwn(url: String, origin: String?): Boolean {
        if (url.startsWith("file:///android_asset/")) return true
        if (origin == null) return false
        return url == origin || url.startsWith("$origin/")
    }
}
