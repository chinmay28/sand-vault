package com.sandvault.tv

/**
 * What is holding a film up, worked out from what the player says every half
 * second — kept apart from the player, as [PlayerText] is, so that it can be
 * tested without a device.
 *
 * A film stops to buffer for one of three reasons, and each wants a different
 * cure: the network, or the PC behind it, is not bringing the film in as fast
 * as it plays; the TV is not decoding the picture as fast as it plays; or it is
 * not decoding the sound. From the sofa the three look the same. What tells
 * them apart:
 *
 *  - **How fast the film arrives while the player is fetching it**, against
 *    how fast it plays. A player with a full buffer stops fetching, so only
 *    the stretches it spends fetching say anything about the link. A film's
 *    bitrate is not known up front — an MKV does not say — so it is measured
 *    as well: the bytes it took to buffer each second of film. Seconds of film
 *    that arrive per second of fetching is the one number that settles it:
 *    below the playing speed, the buffer is draining and the link is the
 *    limit.
 *  - **Frames dropped, or the sound running dry, with plenty buffered**: a
 *    decoder that cannot keep up while the network does its part.
 *  - **Stalls**: how many, and how long in all — waits for the film once it
 *    has started, not the wait to start or the wait after a seek.
 *
 * Rates are over a window of the last few seconds ([windowMs]), so they
 * follow the stretch of film playing now; stalls and dropped frames are kept
 * for the whole film as well.
 */
class PlaybackStats(private val windowMs: Long = WINDOW_MS) {

    /** One look at the player. Counts are totals since the film was opened. */
    data class Sample(
        /** When, on a clock that only goes forward (SystemClock.elapsedRealtime). */
        val atMs: Long,
        /** Bytes of the film read off the network so far. */
        val filmBytes: Long,
        /** Whether the player is fetching more of the film. */
        val loading: Boolean,
        /** How far into the film is buffered, in film time. */
        val bufferedToMs: Long,
        /** Where playback is, in film time. */
        val positionMs: Long,
        /** Whether the player wants to play and is waiting for the film to arrive. */
        val waiting: Boolean,
        /** Whether the picture is moving. */
        val playing: Boolean,
        /** How fast it plays: 1 is real time. */
        val speed: Float = 1f,
        val framesRendered: Long = 0,
        val framesDropped: Long = 0,
        val audioUnderruns: Long = 0,
        /** How much memory the player's buffer holds. */
        val bufferBytes: Long = 0,
    )

    /** What is holding the film up, if anything. */
    enum class Limit {
        /** Too little seen yet to say. */
        MEASURING,

        /** Nothing: the film arrives at least as fast as it plays. */
        KEEPING_UP,

        /** The film is not arriving as fast as it plays. */
        NETWORK,

        /** Frames are being dropped with plenty buffered. */
        VIDEO,

        /** The sound is running dry with plenty buffered. */
        AUDIO,
    }

    data class Report(
        val limit: Limit,
        /** Bits per second that arrived while the player was fetching; null when it fetched too little to say. */
        val downloadBitsPerSecond: Long?,
        /** How much of the window the player spent fetching, 0 to 1. */
        val loadingShare: Double,
        /** The bitrate of the stretch being fetched: the bits it took per second of film; null when too little film arrived to say. */
        val filmBitsPerSecond: Long?,
        /** Seconds of film that arrived per second of fetching; null when it fetched too little to say. */
        val arrival: Double?,
        /** How much is buffered ahead of where playback is. */
        val bufferedMs: Long,
        val bufferBytes: Long,
        val stalls: Int,
        val stalledMs: Long,
        /** Frames the decoder dealt with in the window, shown or dropped. */
        val framesInWindow: Long,
        val droppedInWindow: Long,
        val droppedTotal: Long,
        val underrunsInWindow: Long,
        val underrunsTotal: Long,
        /** How long the window looked over. */
        val windowMs: Long,
    )

    private val window = ArrayDeque<Sample>()
    private var last: Sample? = null

    private var started = false
    private var afterSeek = false
    private var inStall = false
    private var stalls = 0
    private var stalledMs = 0L
    private var droppedTotal = 0L
    private var underrunsTotal = 0L

    /** Takes one look at the player. Samples are expected in time order; one that is not is ignored. */
    fun add(sample: Sample) {
        val previous = last
        if (previous != null && sample.atMs < previous.atMs) return

        if (previous != null && inStall) stalledMs += sample.atMs - previous.atMs
        droppedTotal += since(previous?.framesDropped ?: 0, sample.framesDropped)
        underrunsTotal += since(previous?.audioUnderruns ?: 0, sample.audioUnderruns)

        // A wait is a stall once the film has started and has not just been
        // sent somewhere else: the wait to start and the wait after a seek are
        // what anybody would expect, and counting them would blame the
        // network for a viewer pressing Right.
        if (sample.playing) {
            started = true
            afterSeek = false
        }
        val stalledNow = started && !afterSeek && sample.waiting
        if (stalledNow && !inStall) stalls++
        inStall = stalledNow
        last = sample

        window.addLast(sample)
        while (window.size > 1 && sample.atMs - window.first().atMs > windowMs) window.removeFirst()
    }

    /**
     * The viewer sent the film somewhere else. What was measured before is about
     * another stretch of film, and the wait that follows is the seek's.
     */
    fun seeked() {
        window.clear()
        afterSeek = true
        inStall = false
    }

    /** What the window says now. */
    fun report(): Report {
        var span = 0L
        var loadingMs = 0L
        var loadingBytes = 0L
        var loadingFilmMs = 0L
        var frames = 0L
        var dropped = 0L
        var underruns = 0L
        var previous: Sample? = null
        for (sample in window) {
            val a = previous
            previous = sample
            if (a == null) continue
            val dt = sample.atMs - a.atMs
            if (dt <= 0) continue
            span += dt
            // A stretch counts as fetching only if the player was fetching at
            // both ends of it: half a second either side of a full buffer says
            // nothing about the link.
            if (a.loading && sample.loading) {
                loadingMs += dt
                loadingBytes += since(a.filmBytes, sample.filmBytes)
                loadingFilmMs += (sample.bufferedToMs - a.bufferedToMs).coerceAtLeast(0)
            }
            val droppedHere = since(a.framesDropped, sample.framesDropped)
            dropped += droppedHere
            frames += since(a.framesRendered, sample.framesRendered) + droppedHere
            underruns += since(a.audioUnderruns, sample.audioUnderruns)
        }

        val latest = window.lastOrNull()
        val bufferedMs = latest?.let { (it.bufferedToMs - it.positionMs).coerceAtLeast(0) } ?: 0
        val share = if (span > 0) loadingMs.toDouble() / span else 0.0
        val fetchedEnough = loadingMs >= MIN_FETCHING_MS
        val download = if (fetchedEnough) loadingBytes * 8 * 1000 / loadingMs else null
        val arrival = if (fetchedEnough) loadingFilmMs.toDouble() / loadingMs else null
        val film = if (loadingFilmMs >= MIN_FILM_MS) loadingBytes * 8 * 1000 / loadingFilmMs else null

        return Report(
            limit = judge(latest, span, share, arrival, bufferedMs, frames, dropped, underruns),
            downloadBitsPerSecond = download,
            loadingShare = share,
            filmBitsPerSecond = film,
            arrival = arrival,
            bufferedMs = bufferedMs,
            bufferBytes = latest?.bufferBytes ?: 0,
            stalls = stalls,
            stalledMs = stalledMs,
            framesInWindow = frames,
            droppedInWindow = dropped,
            droppedTotal = droppedTotal,
            underrunsInWindow = underruns,
            underrunsTotal = underrunsTotal,
            windowMs = span,
        )
    }

    private fun judge(
        latest: Sample?,
        span: Long,
        share: Double,
        arrival: Double?,
        bufferedMs: Long,
        frames: Long,
        dropped: Long,
        underruns: Long,
    ): Limit {
        if (latest == null || span < MIN_SPAN_MS) return Limit.MEASURING

        // A decoder is only to blame when the film is there to decode: frames
        // dropped while the player waits for data, or in the scramble after
        // one, are the network's doing.
        val cushioned = bufferedMs >= CUSHION_MS && !latest.waiting
        if (cushioned && dropped >= MIN_DROPPED && dropped * 100 >= frames * DROPPED_PERCENT) return Limit.VIDEO
        if (cushioned && underruns > 0) return Limit.AUDIO

        // Fetching flat out and still falling behind. The margin is for the
        // buffered position, which moves a frame at a time.
        val needed = latest.speed.toDouble().coerceAtLeast(0.01)
        if (share >= FLAT_OUT && arrival != null && arrival < needed * (1 - MARGIN)) return Limit.NETWORK
        if (started && latest.waiting && share >= FLAT_OUT) return Limit.NETWORK
        return Limit.KEEPING_UP
    }

    companion object {
        /** How far back the rates look. */
        const val WINDOW_MS = 10_000L

        /** Less than this looked at says nothing. */
        const val MIN_SPAN_MS = 3_000L

        /** Less than this spent fetching gives no rate. */
        const val MIN_FETCHING_MS = 1_500L

        /** Less than this much film arriving gives no bitrate. */
        const val MIN_FILM_MS = 1_000L

        /** Buffered ahead, enough that a decoder falling behind is not a network waiting. */
        const val CUSHION_MS = 5_000L

        /** Dropped frames, at least this many and this share of them, are a decoder falling behind. */
        const val MIN_DROPPED = 5L
        const val DROPPED_PERCENT = 2L

        /** Fetching this much of the time is fetching flat out. */
        const val FLAT_OUT = 0.8

        /** How far below the playing speed arrival has to be to count as falling behind. */
        const val MARGIN = 0.03

        /** What a count gained between two looks, reading a count that started again as counted since. */
        internal fun since(before: Long, after: Long): Long = if (after >= before) after - before else after
    }
}
