package com.sandvault.tv

import com.sandvault.tv.PlaybackStats.Limit
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class PlaybackStatsTest {

    /**
     * A film being fetched and played, looked at every half second the way the
     * player is: each step brings in what a link of [linkMbps] carries in that
     * time, which is that much film at [filmMbps].
     */
    private class Film(val stats: PlaybackStats = PlaybackStats()) {
        var at = 0L
        var bytes = 0L
        var bufferedTo = 20_000L
        var position = 0L
        var rendered = 0L
        var dropped = 0L
        var underruns = 0L

        fun step(
            linkMbps: Double = 60.0,
            filmMbps: Double = 40.0,
            loading: Boolean = true,
            playing: Boolean = true,
            waiting: Boolean = false,
            speed: Float = 1f,
            frames: Long = 12,
            droppedFrames: Long = 0,
            underrun: Long = 0,
            ms: Long = 500,
        ) {
            at += ms
            if (loading) {
                val got = (linkMbps * 1_000_000 / 8 * ms / 1000).toLong()
                bytes += got
                bufferedTo += (got * 8 * 1000 / (filmMbps * 1_000_000)).toLong()
            }
            if (playing) {
                position += (ms * speed).toLong()
                rendered += frames
            }
            dropped += droppedFrames
            underruns += underrun
            stats.add(
                PlaybackStats.Sample(
                    atMs = at,
                    filmBytes = bytes,
                    loading = loading,
                    bufferedToMs = bufferedTo,
                    positionMs = position,
                    waiting = waiting,
                    playing = playing,
                    speed = speed,
                    framesRendered = rendered,
                    framesDropped = dropped,
                    audioUnderruns = underruns,
                    bufferBytes = 144_179_200,
                ),
            )
        }

        /** [count] steps of [each] — named apart from step, which a call inside would otherwise mean. */
        fun steps(count: Int, each: Film.() -> Unit) = repeat(count) { each() }
    }

    @Test
    fun measuresTheLinkAndTheFilmWhileFetching() {
        val film = Film()
        film.steps(20) { step(linkMbps = 60.0, filmMbps = 40.0) }

        val report = film.stats.report()
        assertEquals(60_000_000L, report.downloadBitsPerSecond)
        assertEquals(40_000_000L, report.filmBitsPerSecond)
        assertEquals(1.5, report.arrival!!, 1e-9)
        assertEquals(1.0, report.loadingShare, 1e-9)
        assertEquals(Limit.KEEPING_UP, report.limit)
        assertEquals(144_179_200L, report.bufferBytes)
    }

    @Test
    fun aLinkSlowerThanTheFilmIsTheNetwork() {
        val film = Film()
        film.steps(20) { step(linkMbps = 30.0, filmMbps = 40.0) }

        val report = film.stats.report()
        assertEquals(Limit.NETWORK, report.limit)
        assertEquals(0.75, report.arrival!!, 1e-9)
        assertEquals(30_000_000L, report.downloadBitsPerSecond)
        assertEquals(40_000_000L, report.filmBitsPerSecond)
    }

    @Test
    fun aLinkThatOnlyJustKeepsUpIsNotBlamed() {
        // The buffered position moves a frame at a time, so a link exactly as
        // fast as the film can read a hair slow; that is not falling behind.
        val film = Film()
        film.steps(20) { step(linkMbps = 39.2, filmMbps = 40.0) }
        assertEquals(Limit.KEEPING_UP, film.stats.report().limit)

        val slower = Film()
        slower.steps(20) { step(linkMbps = 37.0, filmMbps = 40.0) }
        assertEquals(Limit.NETWORK, slower.stats.report().limit)
    }

    @Test
    fun aFullBufferSaysNothingAgainstTheLink() {
        // A player with a full buffer fetches in short bursts between long
        // waits. None of it is a measure of the link, however little arrives.
        val film = Film()
        for (i in 0 until 20) film.step(linkMbps = 5.0, filmMbps = 40.0, loading = i % 5 == 0)

        val report = film.stats.report()
        assertEquals(Limit.KEEPING_UP, report.limit)
        assertNull("no rate from bursts too short to measure", report.downloadBitsPerSecond)
        assertTrue(report.loadingShare < 0.5)
    }

    @Test
    fun aFasterSpeedNeedsTheFilmFaster() {
        val atNormal = Film()
        atNormal.steps(20) { step(linkMbps = 60.0, filmMbps = 40.0, speed = 1f) }
        assertEquals(Limit.KEEPING_UP, atNormal.stats.report().limit)

        val atDouble = Film()
        atDouble.steps(20) { step(linkMbps = 60.0, filmMbps = 40.0, speed = 2f) }
        assertEquals("1.5× real time cannot feed a film played at 2×", Limit.NETWORK, atDouble.stats.report().limit)
    }

    @Test
    fun nothingArrivingWhileFetchingIsTheNetwork() {
        val film = Film()
        film.steps(6) { step() }
        // Long enough that the window holds nothing but the wait.
        film.steps(24) { step(linkMbps = 0.0, playing = false, waiting = true) }

        val report = film.stats.report()
        assertEquals(Limit.NETWORK, report.limit)
        assertEquals(0.0, report.arrival!!, 1e-9)
    }

    @Test
    fun framesDroppedWithPlentyBufferedAreTheTV() {
        val film = Film()
        film.steps(20) { step(frames = 12, droppedFrames = 3) }

        val report = film.stats.report()
        assertEquals(Limit.VIDEO, report.limit)
        assertEquals(57L, report.droppedInWindow)
        assertTrue(report.framesInWindow > report.droppedInWindow)
        assertEquals(60L, report.droppedTotal)
    }

    @Test
    fun framesDroppedWhileWaitingForTheFilmAreNotTheTV() {
        // The scramble to catch up after a wait drops frames on every TV; it is
        // the network's doing, not the decoder's.
        val film = Film()
        film.steps(6) { step() }
        film.steps(14) { step(linkMbps = 30.0, waiting = true, droppedFrames = 3) }
        assertNotEquals(Limit.VIDEO, film.stats.report().limit)

        // And a frame or two now and then is nothing at all.
        val steady = Film()
        steady.steps(20) { step(droppedFrames = if (at % 5_000 == 0L) 1 else 0) }
        assertEquals(Limit.KEEPING_UP, steady.stats.report().limit)
    }

    @Test
    fun theSoundRunningDryWithPlentyBufferedIsTheTV() {
        val film = Film()
        film.steps(10) { step() }
        film.step(underrun = 1)
        film.steps(9) { step() }

        val report = film.stats.report()
        assertEquals(Limit.AUDIO, report.limit)
        assertEquals(1L, report.underrunsInWindow)
        assertEquals(1L, report.underrunsTotal)
    }

    @Test
    fun stallsAreCountedOnceTheFilmHasStarted() {
        val film = Film()
        // The wait to start is not a stall.
        film.steps(3) { step(playing = false, waiting = true) }
        film.steps(4) { step() }
        // Three seconds of waiting, then one.
        film.steps(6) { step(playing = false, waiting = true) }
        film.steps(2) { step() }
        film.steps(2) { step(playing = false, waiting = true) }
        film.step()

        val report = film.stats.report()
        assertEquals(2, report.stalls)
        assertEquals(4_000L, report.stalledMs)
    }

    @Test
    fun aSeekIsNotAStallAndStartsTheMeasurementAgain() {
        val film = Film()
        film.steps(10) { step(linkMbps = 30.0, filmMbps = 40.0) }

        film.stats.seeked()
        film.position = 600_000
        film.bufferedTo = 600_000
        film.steps(2) { step(linkMbps = 60.0, filmMbps = 40.0, playing = false, waiting = true) }
        assertEquals("a second after a seek is too soon to say", Limit.MEASURING, film.stats.report().limit)

        film.steps(4) { step(linkMbps = 60.0, filmMbps = 40.0, playing = false, waiting = true) }
        film.steps(10) { step(linkMbps = 60.0, filmMbps = 40.0) }

        val report = film.stats.report()
        assertEquals(0, report.stalls)
        // Only the stretch after the seek is measured: the jump of ten minutes
        // in the buffered position is not ten minutes of film arriving.
        assertEquals(1.5, report.arrival!!, 1e-9)
        assertEquals(Limit.KEEPING_UP, report.limit)
    }

    @Test
    fun aStallAfterTheSeekHasPlayedOnIsAStall() {
        val film = Film()
        film.steps(4) { step() }
        film.stats.seeked()
        film.steps(2) { step(playing = false, waiting = true) }
        film.steps(2) { step() }
        film.steps(2) { step(playing = false, waiting = true) }
        film.step()

        assertEquals(1, film.stats.report().stalls)
    }

    @Test
    fun aCountThatStartsAgainIsReadFromZero() {
        // A new decoder brings new counters, which start again at nothing.
        val film = Film()
        film.steps(10) { step(droppedFrames = 5) }
        film.dropped = 0
        film.rendered = 0
        film.step(droppedFrames = 3)

        val report = film.stats.report()
        assertEquals(53L, report.droppedTotal)
        assertTrue(report.droppedInWindow >= 0)
        assertTrue(report.framesInWindow >= 0)
    }

    @Test
    fun tooLittleSeenIsMeasuring() {
        val film = Film()
        film.steps(4) { step() }
        assertEquals(Limit.MEASURING, film.stats.report().limit)
        assertEquals(Limit.MEASURING, PlaybackStats().report().limit)
    }

    @Test
    fun ratesFollowTheStretchPlayingNow() {
        val film = Film()
        film.steps(30) { step(linkMbps = 30.0, filmMbps = 40.0) }
        film.steps(30) { step(linkMbps = 60.0, filmMbps = 40.0) }

        val report = film.stats.report()
        assertEquals(60_000_000L, report.downloadBitsPerSecond)
        assertTrue(report.windowMs <= PlaybackStats.WINDOW_MS)
        assertNotNull(report.arrival)
    }

    @Test
    fun aLookFromThePastIsIgnored() {
        val film = Film()
        film.steps(10) { step() }
        val before = film.stats.report()
        film.stats.add(PlaybackStats.Sample(atMs = 100, filmBytes = 0, loading = false, bufferedToMs = 0, positionMs = 0,
            waiting = true, playing = false))
        assertEquals(before, film.stats.report())
    }
}
