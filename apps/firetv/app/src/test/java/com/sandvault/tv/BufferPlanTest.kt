package com.sandvault.tv

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class BufferPlanTest {

    private val mib = 1L shl 20

    @Test
    fun aStickThatGrantsNoMoreMemoryKeepsMedia3sBuffer() {
        // 192 MiB is what a stick gives an app that does not ask for more: the
        // "201 MB" the Stats panel shows. Media3's buffer is what it already
        // carries, and the plan does not shrink it.
        val plan = BufferPlan.forHeap(192 * mib)
        assertEquals(144_179_200, plan.targetBytes)
        assertEquals(BufferPlan.MEDIA3_BYTES, plan.targetBytes)
        assertEquals(144_179_200, BufferPlan.forHeap(0).targetBytes)
        assertEquals(144_179_200, BufferPlan.forHeap(256 * mib).targetBytes)
    }

    @Test
    fun aLargeHeapBuysASeveralTimesBiggerBuffer() {
        val plan = BufferPlan.forHeap(512 * mib)
        // 512 MiB less 35% of it, cut to whole 64 KiB segments.
        assertEquals(348_913_664, plan.targetBytes)
        assertEquals(0, plan.targetBytes % BufferPlan.SEGMENT_BYTES)
        assertTrue(plan.targetBytes > 2.4 * BufferPlan.MEDIA3_BYTES)
        // At least the reserve is always left for the rest of the app.
        val heap384 = 384 * mib
        assertTrue(heap384 - BufferPlan.forHeap(heap384).targetBytes >= BufferPlan.RESERVE_BYTES)
    }

    @Test
    fun theBufferHasACeiling() {
        assertEquals(767_950_848, BufferPlan.forHeap(8L shl 30).targetBytes)
    }

    @Test
    fun theTimeLimitLeavesAHighBitrateFilmTheBytes() {
        // Media3's fifty seconds would stop a 45 Mb/s film at 281 MB of a
        // 349 MB buffer. Three minutes leaves the bytes to decide for anything
        // above about 16 Mb/s.
        val plan = BufferPlan.forHeap(512 * mib)
        val bytesInTimeLimitAt16Mbps = plan.maxBufferMs.toLong() * 16_000_000 / 8 / 1000
        assertTrue(bytesInTimeLimitAt16Mbps >= plan.targetBytes)
        assertEquals(180_000, plan.maxBufferMs)
    }
}
