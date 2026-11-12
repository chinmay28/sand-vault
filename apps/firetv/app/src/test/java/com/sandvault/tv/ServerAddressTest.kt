package com.sandvault.tv

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class ServerAddressTest {

    @Test
    fun everySpellingOfAServerMeansTheSameOrigin() {
        for (typed in listOf(
            "192.168.1.20",
            "192.168.1.20:8123",
            " http://192.168.1.20:8123/ ",
            "http://192.168.1.20:8123/tv/",
            "HTTP://192.168.1.20:8123",
        )) {
            assertEquals(typed, "http://192.168.1.20:8123", ServerAddress.normalize(typed))
        }
    }

    @Test
    fun hostsAreLowercasedAndPortsKept() {
        assertEquals("http://nas.local:9000", ServerAddress.normalize("NAS.local:9000"))
        assertEquals("http://nas.local:8123", ServerAddress.normalize("nas.local"))
    }

    @Test
    fun theDefaultPortForASchemeIsDropped() {
        assertEquals("http://sand.example", ServerAddress.normalize("http://sand.example:80"))
        assertEquals("https://sand.example", ServerAddress.normalize("https://sand.example"))
        assertEquals("https://sand.example:8443", ServerAddress.normalize("https://sand.example:8443/tv"))
    }

    @Test
    fun ipv6LiteralsKeepTheirBrackets() {
        assertEquals("http://[fe80::1]:8123", ServerAddress.normalize("[fe80::1]:8123"))
        assertEquals("http://[fe80::1]:8123", ServerAddress.normalize("[FE80::1]"))
    }

    @Test
    fun nonsenseIsRefused() {
        for (typed in listOf("", "   ", "ftp://nas", "nas:notaport", "nas:0", "nas:70000", "http://", "na s", "[fe80::1")) {
            assertNull(typed, ServerAddress.normalize(typed))
        }
    }

    @Test
    fun onlyTheServerAndTheAppsOwnPagesAreOwn() {
        val origin = "http://192.168.1.20:8123"
        assertTrue(ServerAddress.isOwn("http://192.168.1.20:8123/tv/", origin))
        assertTrue(ServerAddress.isOwn("http://192.168.1.20:8123/stream/abc/film.mkv", origin))
        assertTrue(ServerAddress.isOwn("file:///android_asset/connect.html", origin))
        assertTrue(ServerAddress.isOwn("file:///android_asset/connect.html", null))
        // A host that merely starts with the server's is somebody else.
        assertFalse(ServerAddress.isOwn("http://192.168.1.20:81234/", origin))
        assertFalse(ServerAddress.isOwn("http://192.168.1.20:8123.evil.example/", origin))
        assertFalse(ServerAddress.isOwn("https://example.com/", origin))
        assertFalse(ServerAddress.isOwn("http://192.168.1.20:8123/tv/", null))
    }

    @Test
    fun theTVPageIsUnderTheOrigin() {
        assertEquals("http://nas:8123/tv/", ServerAddress.tvPage("http://nas:8123"))
    }
}
