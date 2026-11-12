package com.sandvault.tv

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.util.Locale

class PlayerTextTest {

    @Test
    fun timesReadTheWayAPlayerShowsThem() {
        assertEquals("0:00", PlayerText.time(0))
        assertEquals("0:42", PlayerText.time(42_999))
        assertEquals("12:05", PlayerText.time(725_000))
        assertEquals("1:02:03", PlayerText.time(3_723_000))
        assertEquals("0:00", PlayerText.time(-5_000))
    }

    @Test
    fun holdingAnArrowGathersPace() {
        assertEquals(10_000L, PlayerText.seekStep(0))
        assertEquals(10_000L, PlayerText.seekStep(2))
        assertEquals(30_000L, PlayerText.seekStep(3))
        assertEquals(300_000L, PlayerText.seekStep(100))
        var last = 0L
        for (n in 0..40) {
            val step = PlayerText.seekStep(n)
            assertTrue("step shrank at $n", step >= last)
            last = step
        }
    }

    @Test
    fun codecsHaveTheirEverydayNames() {
        assertEquals("HEVC", PlayerText.codec("video/hevc"))
        assertEquals("E-AC-3", PlayerText.codec("audio/eac3"))
        assertEquals("", PlayerText.codec(null))
        assertEquals("X-WHATEVER", PlayerText.codec("audio/x-whatever"))
    }

    @Test
    fun channelLayouts() {
        assertEquals("Stereo", PlayerText.channels(2))
        assertEquals("5.1", PlayerText.channels(6))
        assertEquals("3 ch", PlayerText.channels(3))
        assertEquals("", PlayerText.channels(-1))
    }

    @Test
    fun languagesAreNamed() {
        assertEquals("English", PlayerText.language("en", Locale.ENGLISH))
        assertEquals("French", PlayerText.language("fra", Locale.ENGLISH))
        assertEquals("German", PlayerText.language("ger", Locale.ENGLISH))
        assertEquals("Japanese", PlayerText.language("jpn", Locale.ENGLISH))
        assertEquals("", PlayerText.language("und", Locale.ENGLISH))
        assertEquals("", PlayerText.language(null, Locale.ENGLISH))
    }

    @Test
    fun trackLabelsSayWhatTheTrackIs() {
        assertEquals("English · E-AC-3 5.1", PlayerText.trackLabel(null, "en", 6, "audio/eac3", 1, Locale.ENGLISH))
        assertEquals(
            "Commentary (English) · AAC Stereo",
            PlayerText.trackLabel("Commentary", "en", 2, "audio/mp4a-latm", 2, Locale.ENGLISH),
        )
        // A label that already names the language is not told it twice.
        assertEquals("English SDH · SRT", PlayerText.trackLabel("English SDH", "en", -1, "application/x-subrip", 1, Locale.ENGLISH))
        assertEquals("Track 3", PlayerText.trackLabel(null, null, -1, null, 3, Locale.ENGLISH))
    }

    @Test
    fun videoSummarySkipsWhatIsNotKnown() {
        assertEquals("1920×1080 · HEVC · 23.976 fps · 8.2 Mb/s", PlayerText.videoSummary(1920, 1080, "video/hevc", 23.976f, 8_200_000))
        assertEquals("H.264 · 25 fps", PlayerText.videoSummary(-1, -1, "video/avc", 25f, -1))
        assertEquals("", PlayerText.videoSummary(-1, -1, null, -1f, -1))
    }

    @Test
    fun speeds() {
        assertEquals("1×", PlayerText.speed(1f))
        assertEquals("1.25×", PlayerText.speed(1.25f))
        assertEquals("0.5×", PlayerText.speed(0.5f))
    }
}
