package com.sandvault.tv

import com.sandvault.tv.PlaybackStats.Limit
import com.sandvault.tv.StatsText.Decoding
import com.sandvault.tv.StatsText.Link
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class StatsTextTest {

    private fun report(
        limit: Limit = Limit.KEEPING_UP,
        download: Long? = 96_100_000,
        share: Double = 0.55,
        film: Long? = 52_300_000,
        arrival: Double? = 1.84,
        buffered: Long = 21_400,
        bufferBytes: Long = 144_179_200,
        stalls: Int = 0,
        stalledMs: Long = 0,
        frames: Long = 240,
        dropped: Long = 0,
        droppedTotal: Long = 0,
        underruns: Long = 0,
        underrunsTotal: Long = 0,
    ) = PlaybackStats.Report(
        limit = limit,
        downloadBitsPerSecond = download,
        loadingShare = share,
        filmBitsPerSecond = film,
        arrival = arrival,
        bufferedMs = buffered,
        bufferBytes = bufferBytes,
        stalls = stalls,
        stalledMs = stalledMs,
        framesInWindow = frames,
        droppedInWindow = dropped,
        droppedTotal = droppedTotal,
        underrunsInWindow = underruns,
        underrunsTotal = underrunsTotal,
        windowMs = 10_000,
    )

    @Test
    fun theHeadlineSaysWhatIsHoldingTheFilmUp() {
        assertEquals("Measuring…", StatsText.headline(report(limit = Limit.MEASURING)))
        assertEquals("Keeping up — the film arrives at 1.84× real time", StatsText.headline(report()))
        assertEquals("Keeping up — the buffer is full", StatsText.headline(report(share = 0.1)))
        assertEquals(
            "The network is the limit — the film arrives at 0.72× real time",
            StatsText.headline(report(limit = Limit.NETWORK, share = 1.0, arrival = 0.72)),
        )
        assertEquals(
            "The network is the limit — the film is not arriving",
            StatsText.headline(report(limit = Limit.NETWORK, share = 1.0, arrival = 0.0)),
        )
        assertEquals(
            "The TV is the limit — it is dropping frames with 21 s buffered",
            StatsText.headline(report(limit = Limit.VIDEO)),
        )
        assertEquals(
            "The TV is the limit — the sound runs dry with 21 s buffered",
            StatsText.headline(report(limit = Limit.AUDIO)),
        )
    }

    @Test
    fun aSlowEthernetAdapterIsNamed() {
        val network = report(limit = Limit.NETWORK, share = 1.0, arrival = 0.9)
        assertEquals(
            "The network is the limit — the film arrives at 0.9× real time, over a 100 Mb/s Ethernet link",
            StatsText.headline(network, Link(Link.Kind.ETHERNET, linkMbps = 100)),
        )
        // A gigabit link, or one that does not say, is not blamed by name.
        assertFalse(StatsText.headline(network, Link(Link.Kind.ETHERNET, linkMbps = 1000)).contains("Ethernet"))
        assertFalse(StatsText.headline(network, Link(Link.Kind.ETHERNET)).contains("Ethernet"))
        assertFalse(StatsText.headline(network, Link(Link.Kind.WIFI, linkMbps = 100)).contains("Ethernet"))
    }

    @Test
    fun onlyAProblemIsAProblem() {
        assertTrue(StatsText.isProblem(report(limit = Limit.NETWORK)))
        assertTrue(StatsText.isProblem(report(limit = Limit.VIDEO)))
        assertTrue(StatsText.isProblem(report(limit = Limit.AUDIO)))
        assertFalse(StatsText.isProblem(report(limit = Limit.KEEPING_UP)))
        assertFalse(StatsText.isProblem(report(limit = Limit.MEASURING)))
    }

    @Test
    fun decodersAreToldApartByName() {
        assertEquals(Decoding.HARDWARE, StatsText.decoding("OMX.MTK.VIDEO.DECODER.HEVC", "video/hevc"))
        assertEquals(Decoding.HARDWARE, StatsText.decoding("c2.mtk.hevc.decoder", "video/hevc"))
        assertEquals(Decoding.HARDWARE, StatsText.decoding("OMX.amlogic.hevc.decoder.awesome2", "video/hevc"))
        assertEquals(Decoding.SOFTWARE, StatsText.decoding("OMX.google.hevc.decoder", "video/hevc"))
        assertEquals(Decoding.SOFTWARE, StatsText.decoding("c2.android.aac.decoder", "audio/mp4a-latm"))
        assertEquals(Decoding.SOFTWARE, StatsText.decoding("ffmpegn6.0-truehd", "audio/true-hd"))
        // No decoder: sound sent on as it is, or PCM, which needs none.
        assertEquals(Decoding.PASSTHROUGH, StatsText.decoding(null, "audio/true-hd"))
        assertEquals(Decoding.PASSTHROUGH, StatsText.decoding(null, "audio/eac3-joc"))
        assertEquals(Decoding.NONE, StatsText.decoding(null, "audio/raw"))
        assertEquals(Decoding.NONE, StatsText.decoding(null, "video/hevc"))
        assertEquals(Decoding.NONE, StatsText.decoding(null, null))
    }

    @Test
    fun linksReadTheWayTheyAreSold() {
        assertEquals("Wi-Fi 5 GHz · 433 Mb/s link · −58 dBm", StatsText.link(Link(Link.Kind.WIFI, 5_180, 433, -58)))
        assertEquals("Wi-Fi 2.4 GHz · 72 Mb/s link", StatsText.link(Link(Link.Kind.WIFI, 2_437, 72)))
        assertEquals("Wi-Fi 6 GHz · 1201 Mb/s link · −49 dBm", StatsText.link(Link(Link.Kind.WIFI, 5_975, 1201, -49)))
        assertEquals("Wi-Fi", StatsText.link(Link(Link.Kind.WIFI)))
        assertEquals("Ethernet · 100 Mb/s link", StatsText.link(Link(Link.Kind.ETHERNET, linkMbps = 100)))
        assertEquals("Ethernet", StatsText.link(Link(Link.Kind.ETHERNET)))
        assertEquals("not connected", StatsText.link(Link(Link.Kind.NONE)))
        assertEquals("connected", StatsText.link(Link(Link.Kind.OTHER)))
        assertEquals("—", StatsText.link(null))
    }

    @Test
    fun wifiBands() {
        assertEquals("2.4 GHz", StatsText.band(2_412))
        assertEquals("5 GHz", StatsText.band(5_745))
        assertEquals("5 GHz", StatsText.band(5_885))
        assertEquals("6 GHz", StatsText.band(6_115))
        assertEquals(null, StatsText.band(0))
        assertEquals(null, StatsText.band(60_000))
    }

    @Test
    fun numbersReadAsAPersonWouldSayThem() {
        assertEquals("0.72×", StatsText.ratio(0.72))
        assertEquals("1.5×", StatsText.ratio(1.5))
        assertEquals("2×", StatsText.ratio(2.0))
        assertEquals("0×", StatsText.ratio(0.0))
        assertEquals("12×", StatsText.ratio(12.34))
        assertEquals("60.0 Mb/s", StatsText.mbps(60_000_000))
        assertEquals("950 kb/s", StatsText.mbps(950_000))
        assertEquals("3.7 s", StatsText.seconds(3_700))
        assertEquals("3 s", StatsText.seconds(3_000))
        assertEquals("21 s", StatsText.seconds(21_400))
        assertEquals("0 s", StatsText.seconds(-5))
        assertEquals("55%", StatsText.percent(0.553))
        assertEquals("144 MB", StatsText.megabytes(144_179_200))
    }

    @Test
    fun theRowsSayWhatThePlayerSees() {
        val rows = StatsText.rows(
            report(stalls = 2, stalledMs = 14_200, dropped = 3, droppedTotal = 12, underrunsTotal = 0),
            StatsText.Playing(
                videoMime = "video/hevc", width = 3840, height = 2160, videoDecoder = "OMX.MTK.VIDEO.DECODER.HEVC",
                audioMime = "audio/true-hd", channels = 8, audioDecoder = null,
            ),
            Link(Link.Kind.WIFI, 5_180, 433, -58),
            heapUsedBytes = 212_000_000,
            heapMaxBytes = 256_000_000,
        ).toMap()

        assertEquals("96.1 Mb/s while fetching · fetching 55% of the time", rows["Download"])
        assertEquals("52.3 Mb/s here", rows["Film"])
        assertEquals("21 s ahead · 144 MB", rows["Buffer"])
        assertEquals("2 · 14 s in all", rows["Stalls"])
        assertEquals("3 dropped of 240 in 10 s · 12 in all", rows["Frames"])
        assertEquals("none", rows["Underruns"])
        assertEquals("HEVC 3840×2160 · hardware (OMX.MTK.VIDEO.DECODER.HEVC)", rows["Video"])
        assertEquals("TrueHD 7.1 · passed through", rows["Audio"])
        assertEquals("Wi-Fi 5 GHz · 433 Mb/s link · −58 dBm", rows["Network"])
        assertEquals("app 212 MB of 256 MB", rows["Memory"])
    }

    @Test
    fun rowsWhenThereIsLittleToSay() {
        val rows = StatsText.rows(
            report(download = null, film = null, arrival = null, share = 0.1, frames = 0, underruns = 1, underrunsTotal = 3),
            StatsText.Playing(audioMime = "audio/true-hd", channels = 8, audioDecoder = "ffmpegn6.0-truehd"),
            link = null,
            heapUsedBytes = 0,
            heapMaxBytes = 0,
        ).toMap()

        assertEquals("— not fetching: the buffer is full", rows["Download"])
        assertEquals("—", rows["Film"])
        assertEquals("21 s ahead · 144 MB · full", rows["Buffer"])
        assertEquals("0 dropped in all", rows["Frames"])
        assertEquals("1 in 10 s · 3 in all", rows["Underruns"])
        assertEquals("—", rows["Video"])
        assertEquals("TrueHD 7.1 · software (FFmpeg)", rows["Audio"])
        assertEquals("—", rows["Network"])
        assertFalse("no memory line when the heap is not known", rows.containsKey("Memory"))
    }
}
