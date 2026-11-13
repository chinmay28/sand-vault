package com.sandvault.tv

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
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
    fun whatTheBufferHoldsDependsOnTheBitrate() {
        val plan = BufferPlan.forHeap(512 * mib)
        // About a minute of the UHD Blu-ray on the screenshot.
        assertEquals(61_212L, BufferPlan.capacityMs(plan, 45_600_000))
        // A 1080p film is held to the time limit, not the memory.
        assertEquals(180_000L, BufferPlan.capacityMs(plan, 4_400_000))
        assertEquals(180_000L, BufferPlan.capacityMs(plan, null))
        assertEquals(25_294L, BufferPlan.capacityMs(BufferPlan.forHeap(192 * mib), 45_600_000))
    }

    @Test
    fun aFilmThatKeepsUpWaitsNoLongerThanMedia3Would() {
        assertEquals(2_000L, BufferPlan.runwayMs(null, 61_212))
        assertEquals(2_000L, BufferPlan.runwayMs(1.4, 61_212))
        assertEquals(2_000L, BufferPlan.runwayMs(0.98, 61_212))
    }

    @Test
    fun aShortLinkWaitsForThreeMinutesOfPlay() {
        // Arriving at 0.757× real time, three minutes of play cost 0.243 × 180 s.
        assertEquals(43_740L, BufferPlan.runwayMs(0.757, 61_212))
        // A wait of 43.7 s of film buys three minutes before the next stall,
        // where two seconds bought eight.
        assertEquals(180_000L, BufferPlan.lastsMs(43_740, 0.757))
        assertEquals(8_230L, BufferPlan.lastsMs(2_000, 0.757))
    }

    @Test
    fun theRunwayIsNeverMoreThanTheBufferHolds() {
        // Media3's buffer holds 25 s of a 45.6 Mb/s film: the runway stops short
        // of filling it, or the wait would end only when the buffer did.
        assertEquals(22_764L, BufferPlan.runwayMs(0.757, 25_294))
        assertEquals(55_090L, BufferPlan.runwayMs(0.0, 61_212))
        assertEquals(2_000L, BufferPlan.runwayMs(0.5, 1_000))
    }

    @Test
    fun aFasterSpeedNeedsMoreRunway() {
        assertEquals(2_000L, BufferPlan.runwayMs(1.5, 120_000, speed = 1f))
        assertEquals(90_000L, BufferPlan.runwayMs(1.5, 120_000, speed = 2f))
        assertEquals(60_000L, BufferPlan.lastsMs(30_000, 1.5, speed = 2f))
    }

    @Test
    fun howLongTheBufferLasts() {
        assertEquals(86_420L, BufferPlan.lastsMs(21_000, 0.757))
        assertEquals(21_000L, BufferPlan.lastsMs(21_000, 0.0))
        assertNull(BufferPlan.lastsMs(21_000, 1.1))
        assertNull(BufferPlan.lastsMs(21_000, null))
    }
}
