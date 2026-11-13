package com.sandvault.tv

import androidx.annotation.OptIn
import androidx.media3.common.C
import androidx.media3.common.util.UnstableApi
import androidx.media3.exoplayer.DefaultLoadControl
import androidx.media3.exoplayer.LoadControl
import androidx.media3.exoplayer.upstream.DefaultAllocator

/**
 * Media3's load control with a buffer the size [BufferPlan] gives it and, after
 * a stall, a wait for [runwayUs] of film rather than for two seconds.
 *
 * Only a stall waits for the runway. The start of a film and a seek play as
 * soon as a second is buffered, as before; so does a film the viewer paused
 * and set going again, since Media3 stops counting a wait as a stall when play
 * is pressed. A buffer that is full plays on whatever the runway says, as the
 * player itself would: it does not wait for film it has stopped fetching.
 *
 * The runway is set on the main thread, from what PlaybackStats measures, and
 * read on the playback thread.
 */
@OptIn(UnstableApi::class)
internal class RunwayLoadControl(private val plan: BufferPlan.Plan) : DefaultLoadControl(
    DefaultAllocator(/* trimOnReset = */ true, C.DEFAULT_BUFFER_SEGMENT_SIZE),
    plan.maxBufferMs,
    plan.maxBufferMs,
    DefaultLoadControl.DEFAULT_BUFFER_FOR_PLAYBACK_MS,
    DefaultLoadControl.DEFAULT_BUFFER_FOR_PLAYBACK_AFTER_REBUFFER_MS,
    plan.targetBytes,
    /* prioritizeTimeOverSizeThresholds = */ false,
    DefaultLoadControl.DEFAULT_BACK_BUFFER_DURATION_MS,
    DefaultLoadControl.DEFAULT_RETAIN_BACK_BUFFER_FROM_KEYFRAME,
) {

    /** How much film, in film time, to have buffered before playing on after a stall. */
    @Volatile
    var runwayUs: Long = BufferPlan.FLOOR_MS * 1000

    override fun shouldStartPlayback(parameters: LoadControl.Parameters): Boolean {
        if (!parameters.rebuffering) return super.shouldStartPlayback(parameters)
        val full = allocator.totalBytesAllocated >= plan.targetBytes ||
            parameters.bufferedDurationUs >= plan.maxBufferMs * 1000L
        return full || parameters.bufferedDurationUs >= runwayUs
    }
}
