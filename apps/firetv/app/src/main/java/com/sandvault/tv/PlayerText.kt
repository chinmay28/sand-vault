package com.sandvault.tv

import java.util.Locale

/**
 * The words and numbers the player shows, worked out from plain values so they
 * can be tested without a device.
 */
object PlayerText {

    /** 0:42, 12:05, 1:02:03. */
    fun time(ms: Long): String {
        val total = (ms.coerceAtLeast(0) / 1000)
        val h = total / 3600
        val m = (total % 3600) / 60
        val s = total % 60
        return if (h > 0) "%d:%02d:%02d".format(h, m, s) else "%d:%02d".format(m, s)
    }

    /**
     * How far one press of Left or Right moves, by how many presses in a row
     * there have been (a held button counts as many). Ten seconds to begin
     * with, as a remote's arrow means everywhere; then it gathers pace, so
     * crossing a two-hour film is a few seconds of holding rather than seven
     * hundred presses.
     */
    fun seekStep(presses: Int): Long = when {
        presses < 3 -> 10_000L
        presses < 8 -> 30_000L
        presses < 16 -> 60_000L
        presses < 30 -> 180_000L
        else -> 300_000L
    }

    /** A codec as people know it, from its MIME type. */
    fun codec(mime: String?): String = when (mime) {
        null, "" -> ""
        "video/avc" -> "H.264"
        "video/hevc" -> "HEVC"
        "video/x-vnd.on2.vp9" -> "VP9"
        "video/av01" -> "AV1"
        "video/mp4v-es" -> "MPEG-4"
        "video/mpeg2" -> "MPEG-2"
        "video/dolby-vision" -> "Dolby Vision"
        "audio/mp4a-latm" -> "AAC"
        "audio/ac3" -> "AC-3"
        "audio/eac3" -> "E-AC-3"
        "audio/eac3-joc" -> "E-AC-3 Atmos"
        "audio/true-hd" -> "TrueHD"
        "audio/vnd.dts" -> "DTS"
        "audio/vnd.dts.hd" -> "DTS-HD"
        "audio/mpeg" -> "MP3"
        "audio/opus" -> "Opus"
        "audio/vorbis" -> "Vorbis"
        "audio/flac" -> "FLAC"
        "audio/raw" -> "PCM"
        "application/x-subrip" -> "SRT"
        "text/vtt" -> "WebVTT"
        "text/x-ssa" -> "SSA"
        "application/pgs" -> "PGS"
        "application/vobsub" -> "VobSub"
        "application/dvbsubs" -> "DVB"
        else -> mime.substringAfter('/').uppercase(Locale.ROOT)
    }

    /** Mono, Stereo, 5.1, 7.1 — or "6 ch" for anything else. */
    fun channels(count: Int): String = when {
        count <= 0 -> ""
        count == 1 -> "Mono"
        count == 2 -> "Stereo"
        count == 6 -> "5.1"
        count == 8 -> "7.1"
        else -> "$count ch"
    }

    /** "en" → "English", in the language the TV is set to. */
    fun language(code: String?, inLocale: Locale = Locale.getDefault()): String {
        if (code.isNullOrBlank() || code == "und") return ""
        val name = Locale.forLanguageTag(twoLetter(code)).getDisplayLanguage(inLocale)
        if (name.isBlank() || name.equals(code, ignoreCase = true)) return code
        return name.replaceFirstChar { if (it.isLowerCase()) it.titlecase(inLocale) else it.toString() }
    }

    /**
     * Matroska files name languages in three letters ("fra", "ger"), which
     * Locale does not name; the two-letter code it does is found by asking
     * each known language for its own three-letter forms.
     */
    private fun twoLetter(code: String): String {
        val lower = code.lowercase(Locale.ROOT).substringBefore('-')
        if (lower.length != 3) return code
        return threeToTwo[lower] ?: code
    }

    private val threeToTwo: Map<String, String> by lazy {
        val out = HashMap<String, String>()
        for (two in Locale.getISOLanguages()) {
            try {
                out[Locale(two).isO3Language] = two
            } catch (_: Exception) {
                // A language with no three-letter code has nothing to map.
            }
        }
        // The bibliographic codes Matroska often uses, where they differ.
        val bibliographic = mapOf(
            "fre" to "fr", "ger" to "de", "dut" to "nl", "chi" to "zh", "cze" to "cs", "gre" to "el",
            "per" to "fa", "rum" to "ro", "slo" to "sk", "alb" to "sq", "arm" to "hy", "baq" to "eu",
            "bur" to "my", "geo" to "ka", "ice" to "is", "mac" to "mk", "mao" to "mi", "may" to "ms",
            "tib" to "bo", "wel" to "cy",
        )
        out.putAll(bibliographic)
        out
    }

    /**
     * What a track is listed as in the Audio and Subtitles menus: its own name
     * if the file gives one, else its language, then what it is made of —
     * "English · E-AC-3 5.1", "Commentary · AAC Stereo". A track with nothing
     * to say for itself is "Track 2".
     */
    fun trackLabel(
        label: String?,
        language: String?,
        channelCount: Int,
        mime: String?,
        number: Int,
        inLocale: Locale = Locale.getDefault(),
    ): String {
        val name = label?.takeIf { it.isNotBlank() } ?: language(language, inLocale).ifBlank { "Track $number" }
        val detail = listOf(codec(mime), channels(channelCount)).filter { it.isNotBlank() }.joinToString(" ")
        val languageName = language(language, inLocale)
        val withLanguage = if (label != null && label.isNotBlank() && languageName.isNotBlank() &&
            !label.contains(languageName, ignoreCase = true)
        ) {
            "$name ($languageName)"
        } else {
            name
        }
        return if (detail.isBlank()) withLanguage else "$withLanguage · $detail"
    }

    /** "1920×1080 · HEVC · 23.976 fps · 8.2 Mb/s", skipping what is not known. */
    fun videoSummary(width: Int, height: Int, mime: String?, frameRate: Float, bitrate: Int): String {
        val parts = mutableListOf<String>()
        if (width > 0 && height > 0) parts += "$width×$height"
        codec(mime).takeIf { it.isNotBlank() }?.let { parts += it }
        if (frameRate > 0f) parts += "%s fps".format(trimmed(frameRate))
        bitrateText(bitrate)?.let { parts += it }
        return parts.joinToString(" · ")
    }

    fun bitrateText(bitsPerSecond: Int): String? = when {
        bitsPerSecond <= 0 -> null
        bitsPerSecond >= 1_000_000 -> "%.1f Mb/s".format(Locale.ROOT, bitsPerSecond / 1_000_000.0)
        else -> "${bitsPerSecond / 1000} kb/s"
    }

    /** 1.0 → "1×", 1.25 → "1.25×". */
    fun speed(speed: Float): String = "${trimmed(speed)}×"

    private fun trimmed(value: Float): String {
        val text = "%.3f".format(Locale.ROOT, value).trimEnd('0').trimEnd('.')
        return text.ifEmpty { "0" }
    }
}
