package com.sandvault.tv

import androidx.annotation.OptIn
import androidx.media3.common.C
import androidx.media3.common.util.UnstableApi
import androidx.media3.extractor.DefaultExtractorInput
import androidx.media3.extractor.DefaultExtractorsFactory
import androidx.media3.extractor.Extractor
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.EOFException

/**
 * Which kinds of file the player can open, checked against Media3 itself
 * rather than against documentation.
 *
 * Each fixture in resources/containers is a one-second clip made with ffmpeg
 * in that container, with the codecs it usually carries (see the table). The
 * test runs every one of Media3's extractors' sniff() over it — the same check
 * the player makes before a byte is decoded — and asserts which extractor
 * claims it. A container nothing claims is one the player cannot open at all,
 * whatever codecs are inside.
 *
 * Decoding is the TV's business, and cannot be tested off a device: video by
 * its hardware decoders, audio by those, HDMI passthrough, or FFmpeg.
 */
@OptIn(UnstableApi::class)
class ContainerSupportTest {

    /** fixture → the extractor that must claim it, or null for "nothing can open this". */
    private val expected = linkedMapOf(
        "sample.mp4" to "Mp4Extractor", // H.264 + AAC
        "sample.m4v" to "Mp4Extractor", // H.264 + AAC
        "sample.mov" to "Mp4Extractor", // H.264 + AAC
        "sample.3gp" to "Mp4Extractor", // H.264 baseline + AAC mono
        "sample.mkv" to "MatroskaExtractor", // HEVC + AC-3
        "sample.webm" to "MatroskaExtractor", // VP9 + Opus
        "sample.avi" to "AviExtractor", // MPEG-4 part 2 (Xvid/DivX) + MP3
        "sample.ts" to "TsExtractor", // H.264 + AAC
        "sample.mpg" to "PsExtractor", // MPEG-2 + MP2
        "sample.vob" to "PsExtractor", // MPEG-2 + AC-3
        "sample.flv" to "FlvExtractor", // H.264 + AAC
        // Not openable by Media3, and so not by the player:
        "sample.ogv" to null, // Theora + Vorbis — Ogg is read for audio only
        "sample.wmv" to null, // WMV + WMA in ASF — no ASF extractor exists
        "sample.m2ts" to null, // H.264 + AC-3 in Blu-ray M2TS — 192-byte packets, not TS's 188
    )

    private fun fixture(name: String): ByteArray =
        requireNotNull(javaClass.classLoader?.getResourceAsStream("containers/$name")) { "missing fixture $name" }
            .use { it.readBytes() }

    /** The extractors that recognise the bytes, by class name. */
    private fun claimants(bytes: ByteArray): List<String> {
        val out = mutableListOf<String>()
        for (extractor in DefaultExtractorsFactory().createExtractors()) {
            if (sniffs(extractor, bytes)) out += extractor.javaClass.simpleName
            extractor.release()
        }
        return out
    }

    private fun sniffs(extractor: Extractor, bytes: ByteArray): Boolean {
        var at = 0
        val reader = androidx.media3.common.DataReader { buffer, offset, length ->
            if (at >= bytes.size) {
                C.RESULT_END_OF_INPUT
            } else {
                val n = minOf(length, bytes.size - at)
                System.arraycopy(bytes, at, buffer, offset, n)
                at += n
                n
            }
        }
        return try {
            extractor.sniff(DefaultExtractorInput(reader, 0, bytes.size.toLong()))
        } catch (_: EOFException) {
            false
        }
    }

    @Test
    fun everyMajorContainerIsClaimedByTheRightExtractor() {
        val report = StringBuilder()
        val wrong = mutableListOf<String>()
        for ((name, want) in expected) {
            val got = claimants(fixture(name))
            report.append(String.format("%-12s %s%n", name, if (got.isEmpty()) "— nothing" else got.joinToString()))
            val ok = if (want == null) got.isEmpty() else want in got
            if (!ok) wrong += "$name: want ${want ?: "nothing"}, got ${got.ifEmpty { listOf("nothing") }}"
        }
        // The whole matrix, in the test log, so a change in Media3 is visible
        // at a glance rather than one assertion at a time.
        println(report)
        assertTrue("containers not handled as expected:\n" + wrong.joinToString("\n") + "\n\n" + report, wrong.isEmpty())
    }

    @Test
    fun theMatrixCoversEveryFixture() {
        val names = javaClass.classLoader?.getResource("containers")?.let { java.io.File(it.toURI()).list()?.sorted() }
        assertEquals("a fixture without an expectation, or the other way round", expected.keys.sorted(), names)
    }
}
