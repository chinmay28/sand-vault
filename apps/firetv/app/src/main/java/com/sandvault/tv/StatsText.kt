package com.sandvault.tv

import com.sandvault.tv.PlaybackStats.Limit
import java.util.Locale

/**
 * The words and numbers of the player's Stats panel, worked out from plain
 * values so they can be tested without a device.
 */
object StatsText {

    /** How the TV is connected, as far as it will say. */
    data class Link(
        val kind: Kind,
        /** The Wi-Fi channel's frequency, or 0. */
        val frequencyMhz: Int = 0,
        /** The rate the link is running at, or 0 when it does not say. */
        val linkMbps: Int = 0,
        /** Wi-Fi signal strength, or 0. */
        val rssiDbm: Int = 0,
    ) {
        enum class Kind { WIFI, ETHERNET, OTHER, NONE }
    }

    /** What is playing, and what is decoding it. */
    data class Playing(
        val videoMime: String? = null,
        val width: Int = 0,
        val height: Int = 0,
        /** The video decoder's name, or null when none is running. */
        val videoDecoder: String? = null,
        val audioMime: String? = null,
        val channels: Int = 0,
        /** The audio decoder's name, or null when none is running. */
        val audioDecoder: String? = null,
    )

    /** Who is turning a track into picture or sound. */
    enum class Decoding { HARDWARE, SOFTWARE, PASSTHROUGH, NONE }

    /**
     * Whether a decoder is the TV's own hardware or a program, by its name —
     * the same rule Media3 uses on TVs too old to say: Android's own software
     * codecs are named OMX.google.* and c2.android.*, and anything not named
     * OMX.* or c2.* at all (FFmpeg's is "ffmpeg…") is software too. A track
     * with no decoder is passed through, to the TV or a receiver, unless it is
     * PCM, which needs none.
     */
    fun decoding(decoderName: String?, mime: String?): Decoding {
        if (decoderName.isNullOrBlank()) {
            return if (mime.isNullOrBlank() || mime == "audio/raw" || !mime.startsWith("audio/")) {
                Decoding.NONE
            } else {
                Decoding.PASSTHROUGH
            }
        }
        val software = decoderName.startsWith("OMX.google.") ||
            decoderName.startsWith("c2.android.") ||
            decoderName.startsWith("c2.google.") ||
            (!decoderName.startsWith("OMX.") && !decoderName.startsWith("c2."))
        return if (software) Decoding.SOFTWARE else Decoding.HARDWARE
    }

    /** The panel's first line: what, if anything, is holding the film up. */
    fun headline(report: PlaybackStats.Report, link: Link? = null): String = when (report.limit) {
        Limit.MEASURING -> "Measuring…"
        Limit.KEEPING_UP -> when {
            report.loadingShare < 0.5 -> "Keeping up — the buffer is full"
            report.arrival != null -> "Keeping up — the film arrives at ${ratio(report.arrival)} real time"
            else -> "Keeping up"
        }
        Limit.NETWORK -> {
            val how = when {
                report.arrival == null || report.arrival <= 0.0 -> "the film is not arriving"
                else -> "the film arrives at ${ratio(report.arrival)} real time"
            }
            // The one cause the TV can name by itself: Amazon's adapter for
            // the sticks runs at 100 Mb/s, below the peaks of a UHD Blu-ray.
            val adapter = if (link?.kind == Link.Kind.ETHERNET && link.linkMbps in 1..100) {
                ", over a ${link.linkMbps} Mb/s Ethernet link"
            } else {
                ""
            }
            "The network is the limit — $how$adapter"
        }
        Limit.VIDEO -> "The TV is the limit — it is dropping frames with ${seconds(report.bufferedMs)} buffered"
        Limit.AUDIO -> "The TV is the limit — the sound runs dry with ${seconds(report.bufferedMs)} buffered"
    }

    /** Whether the headline is about a problem, for drawing it in the warning colour. */
    fun isProblem(report: PlaybackStats.Report): Boolean =
        report.limit == Limit.NETWORK || report.limit == Limit.VIDEO || report.limit == Limit.AUDIO

    /** The panel's lines below the headline, as name and value. */
    fun rows(
        report: PlaybackStats.Report,
        playing: Playing,
        link: Link?,
        heapUsedBytes: Long,
        heapMaxBytes: Long,
    ): List<Pair<String, String>> {
        val full = report.loadingShare < 0.5
        val out = mutableListOf<Pair<String, String>>()

        out += "Download" to when {
            report.downloadBitsPerSecond != null ->
                "${mbps(report.downloadBitsPerSecond)} while fetching · fetching ${percent(report.loadingShare)} of the time"
            full -> "— not fetching: the buffer is full"
            else -> "—"
        }
        out += "Film" to (report.filmBitsPerSecond?.let { "${mbps(it)} here" } ?: "—")
        out += "Buffer" to buildString {
            append("${seconds(report.bufferedMs)} ahead")
            if (report.bufferBytes > 0) append(" · ${megabytes(report.bufferBytes)}")
            if (full) append(" · full")
        }
        out += "Stalls" to if (report.stalls == 0) "none" else "${report.stalls} · ${seconds(report.stalledMs)} in all"
        out += "Frames" to if (report.framesInWindow > 0) {
            "${report.droppedInWindow} dropped of ${report.framesInWindow} in ${seconds(report.windowMs)} · ${report.droppedTotal} in all"
        } else {
            "${report.droppedTotal} dropped in all"
        }
        out += "Underruns" to if (report.underrunsTotal == 0L) {
            "none"
        } else {
            "${report.underrunsInWindow} in ${seconds(report.windowMs)} · ${report.underrunsTotal} in all"
        }
        out += "Video" to track(PlayerText.codec(playing.videoMime), dimensions(playing.width, playing.height),
            playing.videoDecoder, playing.videoMime)
        out += "Audio" to track(PlayerText.codec(playing.audioMime), PlayerText.channels(playing.channels),
            playing.audioDecoder, playing.audioMime)
        out += "Network" to link(link)
        if (heapMaxBytes > 0) out += "Memory" to "app ${megabytes(heapUsedBytes)} of ${megabytes(heapMaxBytes)}"
        return out
    }

    /** "Wi-Fi 5 GHz · 433 Mb/s link · −58 dBm", "Ethernet · 100 Mb/s link". */
    fun link(link: Link?): String = when (link?.kind) {
        null -> "—"
        Link.Kind.NONE -> "not connected"
        Link.Kind.OTHER -> "connected"
        Link.Kind.ETHERNET -> if (link.linkMbps > 0) "Ethernet · ${link.linkMbps} Mb/s link" else "Ethernet"
        Link.Kind.WIFI -> listOfNotNull(
            listOfNotNull("Wi-Fi", band(link.frequencyMhz)).joinToString(" "),
            link.linkMbps.takeIf { it > 0 }?.let { "$it Mb/s link" },
            // -127 is what Android reports for a signal it has not measured.
            link.rssiDbm.takeIf { it in -126..-1 }?.let { "−${-it} dBm" },
        ).joinToString(" · ")
    }

    /** The Wi-Fi band a channel is in, or null when it is not known. */
    fun band(frequencyMhz: Int): String? = when {
        frequencyMhz <= 0 -> null
        frequencyMhz < 2_500 -> "2.4 GHz"
        frequencyMhz < 5_925 -> "5 GHz"
        frequencyMhz <= 7_125 -> "6 GHz"
        else -> null
    }

    /** 0.72 → "0.72×", 1.5 → "1.5×", 2 → "2×", 12.3 → "12×". */
    fun ratio(value: Double): String {
        if (value >= 10) return "%.0f×".format(Locale.ROOT, value)
        val text = "%.2f".format(Locale.ROOT, value).trimEnd('0').trimEnd('.')
        return "${text.ifEmpty { "0" }}×"
    }

    /** "60.0 Mb/s", "950 kb/s". */
    fun mbps(bitsPerSecond: Long): String = when {
        bitsPerSecond >= 1_000_000 -> "%.1f Mb/s".format(Locale.ROOT, bitsPerSecond / 1_000_000.0)
        else -> "${(bitsPerSecond / 1000).coerceAtLeast(0)} kb/s"
    }

    /** "3.7 s" below ten seconds, "21 s" above. */
    fun seconds(ms: Long): String {
        val s = ms.coerceAtLeast(0) / 1000.0
        return if (s < 10) "%.1f s".format(Locale.ROOT, s).replace(".0 s", " s") else "%.0f s".format(Locale.ROOT, s)
    }

    /** "55%". */
    fun percent(share: Double): String = "%.0f%%".format(Locale.ROOT, share.coerceIn(0.0, 1.0) * 100)

    /** Decimal megabytes, as a buffer is counted: "144 MB". */
    fun megabytes(bytes: Long): String = "%.0f MB".format(Locale.ROOT, bytes.coerceAtLeast(0) / 1_000_000.0)

    private fun dimensions(width: Int, height: Int): String = if (width > 0 && height > 0) "$width×$height" else ""

    /** "HEVC 3840×2160 · hardware (OMX.MTK.VIDEO.DECODER.HEVC)", "TrueHD 7.1 · passed through". */
    private fun track(codec: String, shape: String, decoderName: String?, mime: String?): String {
        val what = listOf(codec, shape).filter { it.isNotBlank() }.joinToString(" ")
        val how = when (decoding(decoderName, mime)) {
            Decoding.HARDWARE -> "hardware ($decoderName)"
            Decoding.SOFTWARE -> if (decoderName?.startsWith("ffmpeg") == true) "software (FFmpeg)" else "software ($decoderName)"
            Decoding.PASSTHROUGH -> "passed through"
            Decoding.NONE -> ""
        }
        return listOf(what, how).filter { it.isNotBlank() }.joinToString(" · ").ifEmpty { "—" }
    }
}
