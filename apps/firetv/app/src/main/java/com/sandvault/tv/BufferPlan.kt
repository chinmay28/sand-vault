package com.sandvault.tv

import kotlin.math.max
import kotlin.math.min
import kotlin.math.roundToLong

/**
 * How much film the player keeps buffered, and how much it waits for after a
 * stall — worked out from plain values, like [PlaybackStats], so that it can be
 * tested without a device.
 *
 * Media3's default is a buffer of 50 seconds or about 144 MB, whichever comes
 * first, and to play on after a stall as soon as two seconds are buffered. For
 * a 1080p film that is fifty seconds of cushion. For a UHD Blu-ray at 45 Mb/s
 * it is twenty-five, and on a link that brings the film in at three quarters of
 * the speed it plays, the two seconds after a stall last eight: the film
 * stalls, plays a moment, and stalls again.
 *
 * So, two things sized to what the stick can do:
 *
 *  - **The buffer takes the memory the app is given**, less a reserve for
 *    everything else in it. The app asks Android for a large heap; on a stick
 *    that grants one the buffer grows to two or three times Media3's, and on
 *    one that does not it stays at Media3's, which is what that heap was
 *    already carrying.
 *  - **After a stall it waits for a runway**, not for two seconds: enough film
 *    that, at the rate the film is arriving, it plays for a few minutes before
 *    it can run dry again — or as much as the buffer holds, if that is less.
 *
 * Neither makes a link that is short of the film's bitrate any less short. The
 * waiting through a stretch the link cannot keep up with adds up the same;
 * what changes is that it comes as a few waits rather than a stall every few
 * seconds, and that a bigger buffer banks more of what a quieter stretch brings
 * in ahead of the next demanding one.
 */
object BufferPlan {

    /** What the player is built with. */
    data class Plan(
        /** The most memory the buffer may take. */
        val targetBytes: Int,
        /** The most film it buffers ahead, however low the bitrate. */
        val maxBufferMs: Int,
    )

    /** The allocation unit of Media3's buffer. */
    const val SEGMENT_BYTES = 65_536

    /** Media3's own target for a video and an audio track: 2,200 segments, about 144 MB. */
    const val MEDIA3_BYTES = 2_200 * SEGMENT_BYTES

    /** Left for the rest of the app: at least this much, or this share of the heap. */
    const val RESERVE_BYTES = 128_000_000L
    const val RESERVE_SHARE = 0.35

    /** A ceiling, for a TV that grants more memory than any stick does. */
    const val MOST_BYTES = 768_000_000L

    /** Further ahead than this is not worth the memory, whatever the bitrate. */
    const val MAX_BUFFER_MS = 180_000

    /** What Media3 waits for after a stall, and the least the runway ever is. */
    const val FLOOR_MS = 2_000L

    /** How long a runway lets the film play before it can run dry again. */
    const val PLAY_FOR_MS = 180_000L

    /** Arriving at least this fast is keeping up: the margin PlaybackStats allows. */
    const val KEEPING_UP = 1 - PlaybackStats.MARGIN

    /** The buffer for an app allowed [maxHeapBytes] of memory (Runtime.maxMemory). */
    fun forHeap(maxHeapBytes: Long): Plan {
        val reserve = max(RESERVE_BYTES, (maxHeapBytes * RESERVE_SHARE).toLong())
        val room = (maxHeapBytes - reserve).coerceIn(MEDIA3_BYTES.toLong(), MOST_BYTES)
        return Plan(targetBytes = (room / SEGMENT_BYTES * SEGMENT_BYTES).toInt(), maxBufferMs = MAX_BUFFER_MS)
    }

    /** How much of a film at [filmBitsPerSecond] the buffer holds; all it may hold when the bitrate is not known. */
    fun capacityMs(plan: Plan, filmBitsPerSecond: Long?): Long {
        val cap = plan.maxBufferMs.toLong()
        if (filmBitsPerSecond == null || filmBitsPerSecond <= 0) return cap
        return min(cap, plan.targetBytes.toLong() * 8 * 1000 / filmBitsPerSecond)
    }

    /**
     * How much film to have buffered before playing on after a stall: enough to
     * play [playForMs] at the rate the film is arriving ([arrival], seconds of
     * film per second), and no more than nine tenths of what the buffer holds,
     * so that the wait ends before the buffer is full. A film that keeps up
     * waits only as long as Media3 would.
     */
    fun runwayMs(arrival: Double?, capacityMs: Long, speed: Float = 1f, playForMs: Long = PLAY_FOR_MS): Long {
        if (arrival == null || arrival >= speed * KEEPING_UP) return FLOOR_MS
        val drain = speed - arrival.coerceAtLeast(0.0)
        val needed = (playForMs * drain).roundToLong()
        return needed.coerceIn(FLOOR_MS, max(FLOOR_MS, capacityMs * 9 / 10))
    }

    /** How long [bufferedMs] of film plays for at [arrival]; null when the film keeps up. */
    fun lastsMs(bufferedMs: Long, arrival: Double?, speed: Float = 1f): Long? {
        if (arrival == null || arrival >= speed * KEEPING_UP) return null
        return (bufferedMs / (speed - arrival.coerceAtLeast(0.0))).roundToLong()
    }
}
