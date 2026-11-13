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

    @Test
    fun theServersLinkIsAskedAboutByTheFilmsTicket() {
        val want = "http://192.168.1.20:8123/api/stream/Ab-9_xY/network"
        for (stream in listOf(
            "http://192.168.1.20:8123/stream/Ab-9_xY/The.Thing.1982.mkv",
            "http://192.168.1.20:8123/stream/Ab-9_xY/The%20Thing.mkv?from=replica",
            "http://192.168.1.20:8123/stream/Ab-9_xY",
            "http://192.168.1.20:8123/stream/Ab-9_xY/#t=10",
        )) {
            assertEquals(stream, want, ServerAddress.networkUrl(stream))
        }
        assertEquals("https://nas.example/api/stream/T0k3n/network", ServerAddress.networkUrl("https://nas.example/stream/T0k3n/x.mp4"))
        assertEquals("http://[fe80::1]:8123/api/stream/T/network", ServerAddress.networkUrl("http://[fe80::1]:8123/stream/T/x.mkv"))
    }

    @Test
    fun onlyAStreamLinkHasAServerLinkToAskAbout() {
        for (notAStream in listOf(
            "",
            "192.168.1.20:8123/stream/T/x.mkv",
            "http://192.168.1.20:8123",
            "http://192.168.1.20:8123/",
            "http://192.168.1.20:8123/tv/",
            "http://192.168.1.20:8123/stream/",
            "http://192.168.1.20:8123/stream//x.mkv",
            "http://192.168.1.20:8123/zip/T/folder.zip",
            "http://192.168.1.20:8123/x/stream/T/x.mkv",
            "http://192.168.1.20:8123/stream/../api/x",
            "http://192.168.1.20:8123/stream/a%2Fb/x.mkv",
            "http://host?q=/stream/T/x.mkv",
            "http:///stream/T/x.mkv",
        )) {
            assertNull(notAStream, ServerAddress.networkUrl(notAStream))
        }
    }
}
