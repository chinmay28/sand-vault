package com.sandvault.tv

import android.content.Context
import android.net.ConnectivityManager
import android.net.wifi.WifiManager
import android.os.Build
import java.io.File

/**
 * How the TV is connected, for the Stats panel: Wi-Fi with its band, link rate
 * and signal, or Ethernet with its link rate where the system will say.
 *
 * Asked the old way, which every Fire OS answers: Fire OS 5 is API 22, before
 * ConnectivityManager could name the active network. Anything that fails — a
 * permission a build does not grant, a file the system will not let an app
 * read — is a link it does not know, not an error.
 */
@Suppress("DEPRECATION")
internal fun readNetworkLink(context: Context): StatsText.Link = try {
    val connectivity = context.applicationContext.getSystemService(Context.CONNECTIVITY_SERVICE) as? ConnectivityManager
    val active = connectivity?.activeNetworkInfo
    when {
        active == null || !active.isConnected -> StatsText.Link(StatsText.Link.Kind.NONE)
        active.type == ConnectivityManager.TYPE_WIFI -> wifiLink(context)
        active.type == ConnectivityManager.TYPE_ETHERNET -> StatsText.Link(StatsText.Link.Kind.ETHERNET, linkMbps = ethernetMbps())
        else -> StatsText.Link(StatsText.Link.Kind.OTHER)
    }
} catch (_: Exception) {
    StatsText.Link(StatsText.Link.Kind.OTHER)
}

@Suppress("DEPRECATION")
private fun wifiLink(context: Context): StatsText.Link {
    val wifi = context.applicationContext.getSystemService(Context.WIFI_SERVICE) as? WifiManager
        ?: return StatsText.Link(StatsText.Link.Kind.WIFI)
    val info = wifi.connectionInfo ?: return StatsText.Link(StatsText.Link.Kind.WIFI)
    // The rate the TV receives at, where the system tells it apart from the
    // rate it sends at: a film only comes one way.
    val receive = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) info.rxLinkSpeedMbps else -1
    return StatsText.Link(
        StatsText.Link.Kind.WIFI,
        frequencyMhz = info.frequency,
        linkMbps = if (receive > 0) receive else info.linkSpeed.coerceAtLeast(0),
        rssiDbm = info.rssi,
    )
}

/**
 * The Ethernet link's rate as the kernel has it, where an app may read it —
 * the one way to tell a 10/100 adapter from a gigabit one from here.
 */
private fun ethernetMbps(): Int = try {
    File("/sys/class/net/eth0/speed").readText().trim().toIntOrNull()?.takeIf { it > 0 } ?: 0
} catch (_: Exception) {
    0
}
