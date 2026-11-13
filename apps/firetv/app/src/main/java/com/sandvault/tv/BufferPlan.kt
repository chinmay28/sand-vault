package com.sandvault.tv

import kotlin.math.max

/**
 * How much film the player keeps buffered, worked out from plain values, like
 * [PlaybackStats], so that it can be tested without a device.
 *
 * Media3's default is 50 seconds or about 144 MB, whichever comes first: fifty
 * seconds of a 1080p film, but only twenty-five of a UHD Blu-ray at 45 Mb/s.
 * The buffer is held in the app's memory, so the app asks Android for a large
 * heap and the buffer takes the room that makes, less a reserve for everything
 * else in the app — and up to three minutes of film rather than fifty seconds,
 * so that a high-bitrate film can use the bytes. A stick that grants no more
 * memory keeps Media3's 144 MB, which is what its heap already carried.
 */
object BufferPlan {

    /** What the player's load control is built with. */
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

    /** The buffer for an app allowed [maxHeapBytes] of memory (Runtime.maxMemory). */
    fun forHeap(maxHeapBytes: Long): Plan {
        val reserve = max(RESERVE_BYTES, (maxHeapBytes * RESERVE_SHARE).toLong())
        val room = (maxHeapBytes - reserve).coerceIn(MEDIA3_BYTES.toLong(), MOST_BYTES)
        return Plan(targetBytes = (room / SEGMENT_BYTES * SEGMENT_BYTES).toInt(), maxBufferMs = MAX_BUFFER_MS)
    }
}
