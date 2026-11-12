package com.sandvault.tv

import android.annotation.SuppressLint
import android.annotation.TargetApi
import android.app.Activity
import android.content.Context
import android.content.Intent
import android.graphics.Color
import android.net.Uri
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.view.KeyEvent
import android.webkit.JavascriptInterface
import android.webkit.WebResourceError
import android.webkit.WebResourceRequest
import android.webkit.WebView
import android.webkit.WebViewClient
import org.json.JSONObject
import java.net.HttpURLConnection
import java.net.URL
import kotlin.concurrent.thread

/**
 * The frame around SAND's TV page.
 *
 * Everything on screen except the film itself is drawn by the server, at /tv/
 * on whichever SAND this app is pointed at, so the screens update with the
 * server rather than with the app. Before there is a server the app shows its
 * own connect page, bundled in assets, which lists the servers discovery found
 * and takes an address typed by hand.
 *
 * Both pages get [Bridge] as window.SandTVNative, for the two things a page
 * cannot do: shout on the network for servers, and play a film in a player
 * that handles MKV, HEVC and surround sound. Navigation is held to the server
 * and the app's own assets, because whatever page is showing holds that bridge.
 */
class MainActivity : Activity() {

    private lateinit var web: WebView
    private val main = Handler(Looper.getMainLooper())
    private val prefs by lazy { getSharedPreferences("sand-tv", Context.MODE_PRIVATE) }

    private var origin: String?
        get() = prefs.getString(KEY_SERVER, null)
        set(value) {
            prefs.edit().apply { if (value == null) remove(KEY_SERVER) else putString(KEY_SERVER, value) }.apply()
        }

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        web = WebView(this).apply {
            setBackgroundColor(Color.rgb(0x0a, 0x0e, 0x17))
            isFocusable = true
            isFocusableInTouchMode = true
            settings.javaScriptEnabled = true
            settings.domStorageEnabled = true
            settings.mediaPlaybackRequiresUserGesture = false
            settings.allowFileAccess = false
            settings.allowContentAccess = false
            addJavascriptInterface(Bridge(), "SandTVNative")
            webViewClient = Client()
        }
        setContentView(web)
        web.requestFocus()

        val saved = origin
        if (saved == null) showConnect(null) else web.loadUrl(ServerAddress.tvPage(saved))
    }

    private fun showConnect(error: String?) {
        val page = "file:///android_asset/connect.html"
        web.loadUrl(if (error == null) page else page + "#error=" + Uri.encode(error))
    }

    /** Back is the page's first: it knows whether it has a screen to go back to. */
    @Deprecated("Back is handled through the page")
    override fun onBackPressed() {
        web.evaluateJavascript("(window.sandTV && window.sandTV.back && window.sandTV.back()) ? 1 : 0") { handled ->
            if (handled != "1") {
                @Suppress("DEPRECATION")
                super.onBackPressed()
            }
        }
    }

    override fun dispatchKeyEvent(event: KeyEvent): Boolean {
        // Some Fire TV remotes send Back as a key the WebView would otherwise
        // swallow; route it through onBackPressed like every other Back.
        if (event.keyCode == KeyEvent.KEYCODE_BACK && event.action == KeyEvent.ACTION_UP) {
            @Suppress("DEPRECATION")
            onBackPressed()
            return true
        }
        if (event.keyCode == KeyEvent.KEYCODE_BACK) return true
        return super.dispatchKeyEvent(event)
    }

    @Deprecated("Uses the platform Activity's result API, which needs no extra library")
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        @Suppress("DEPRECATION")
        super.onActivityResult(requestCode, resultCode, data)
        if (requestCode != REQUEST_PLAY || data == null) return
        val id = data.getStringExtra(PlayerActivity.EXTRA_ITEM_ID) ?: return
        val pos = data.getLongExtra(PlayerActivity.EXTRA_POSITION_MS, 0)
        val dur = data.getLongExtra(PlayerActivity.EXTRA_DURATION_MS, 0)
        web.evaluateJavascript(
            "window.sandTV && window.sandTV.onPlaybackStopped(${JSONObject.quote(id)}, $pos, $dur)",
            null,
        )
    }

    override fun onDestroy() {
        web.destroy()
        super.onDestroy()
    }

    private inner class Client : WebViewClient() {
        override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest): Boolean =
            !ServerAddress.isOwn(request.url.toString(), origin)

        // API 23; on Fire OS 5 a server that is down shows the WebView's own
        // error page instead, and Back still reaches the connect page.
        @TargetApi(23)
        override fun onReceivedError(view: WebView, request: WebResourceRequest, error: WebResourceError) {
            // Only the page itself failing means the server is unreachable; a
            // poster that did not load is the page's business.
            if (request.isForMainFrame && !request.url.toString().startsWith("file:")) {
                showConnect("Could not reach ${origin ?: "the server"} — ${error.description}")
            }
        }
    }

    /** window.SandTVNative. Called on a binder thread, so everything hops to the main one. */
    private inner class Bridge {

        @JavascriptInterface
        fun play(url: String, title: String, startMs: Long, itemId: String) {
            val server = origin ?: return
            // Only the server's own stream links: a page cannot have the app
            // play something from elsewhere.
            if (!ServerAddress.isOwn(url, server)) return
            main.post {
                val intent = Intent(this@MainActivity, PlayerActivity::class.java)
                    .putExtra(PlayerActivity.EXTRA_URL, url)
                    .putExtra(PlayerActivity.EXTRA_TITLE, title)
                    .putExtra(PlayerActivity.EXTRA_START_MS, startMs)
                    .putExtra(PlayerActivity.EXTRA_ITEM_ID, itemId)
                @Suppress("DEPRECATION")
                startActivityForResult(intent, REQUEST_PLAY)
            }
        }

        /**
         * play, with the subtitle files the server found beside the film:
         * {url, title, startMs, itemId, subtitles: [{url, label, language, mime}]}.
         */
        @JavascriptInterface
        fun playWith(json: String) {
            val server = origin ?: return
            val req = try {
                JSONObject(json)
            } catch (_: Exception) {
                return
            }
            val url = req.optString("url")
            if (!ServerAddress.isOwn(url, server)) return

            val urls = ArrayList<String>()
            val labels = ArrayList<String>()
            val languages = ArrayList<String>()
            val mimes = ArrayList<String>()
            val subs = req.optJSONArray("subtitles")
            for (i in 0 until (subs?.length() ?: 0)) {
                val sub = subs?.optJSONObject(i) ?: continue
                val subUrl = sub.optString("url")
                if (!ServerAddress.isOwn(subUrl, server)) continue
                urls += subUrl
                labels += sub.optString("label")
                languages += sub.optString("language")
                mimes += sub.optString("mime")
            }

            main.post {
                val intent = Intent(this@MainActivity, PlayerActivity::class.java)
                    .putExtra(PlayerActivity.EXTRA_URL, url)
                    .putExtra(PlayerActivity.EXTRA_TITLE, req.optString("title"))
                    .putExtra(PlayerActivity.EXTRA_START_MS, req.optLong("startMs", 0))
                    .putExtra(PlayerActivity.EXTRA_ITEM_ID, req.optString("itemId"))
                    .putExtra(PlayerActivity.EXTRA_SUB_URLS, urls.toTypedArray())
                    .putExtra(PlayerActivity.EXTRA_SUB_LABELS, labels.toTypedArray())
                    .putExtra(PlayerActivity.EXTRA_SUB_LANGUAGES, languages.toTypedArray())
                    .putExtra(PlayerActivity.EXTRA_SUB_MIMES, mimes.toTypedArray())
                @Suppress("DEPRECATION")
                startActivityForResult(intent, REQUEST_PLAY)
            }
        }

        @JavascriptInterface
        fun changeServer() {
            main.post {
                origin = null
                web.clearHistory()
                showConnect(null)
            }
        }

        @JavascriptInterface
        fun savedServer(): String = origin ?: ""

        /** Answers through window.sandConnect.onDiscovered(json). */
        @JavascriptInterface
        fun discover() {
            thread(name = "sand-discovery") {
                val found = try {
                    Discovery(this@MainActivity).find()
                } catch (_: Exception) {
                    emptyList()
                }
                val json = Discovery.toJson(found)
                main.post {
                    web.evaluateJavascript("window.sandConnect && window.sandConnect.onDiscovered($json)", null)
                }
            }
        }

        /**
         * Checks the address answers as SAND before saving it, so a typo is a
         * message on the connect page rather than a blank screen. Answers
         * through window.sandConnect.onConnectFailed(message) on failure.
         */
        @JavascriptInterface
        fun connect(typed: String) {
            val candidate = ServerAddress.normalize(typed)
            if (candidate == null) {
                fail("That does not look like an address — try something like 192.168.1.20:8123")
                return
            }
            thread(name = "sand-connect") {
                val problem = probe(candidate)
                main.post {
                    if (problem != null) {
                        fail(problem)
                    } else {
                        origin = candidate
                        web.clearHistory()
                        web.loadUrl(ServerAddress.tvPage(candidate))
                    }
                }
            }
        }

        private fun fail(message: String) {
            main.post {
                web.evaluateJavascript(
                    "window.sandConnect && window.sandConnect.onConnectFailed(${JSONObject.quote(message)})",
                    null,
                )
            }
        }
    }

    /** Null when origin answers /api/health as SAND; otherwise what went wrong. */
    private fun probe(candidate: String): String? = try {
        val conn = URL("$candidate/api/health").openConnection() as HttpURLConnection
        conn.connectTimeout = 4000
        conn.readTimeout = 4000
        try {
            if (conn.responseCode != 200) {
                "$candidate answered, but not as SAND (HTTP ${conn.responseCode})"
            } else {
                val body = conn.inputStream.bufferedReader().use { it.readText() }
                if (JSONObject(body).optString("status") == "ok") null else "$candidate is not a SAND server"
            }
        } finally {
            conn.disconnect()
        }
    } catch (e: Exception) {
        "Could not reach $candidate — is SAND running, and on the same network?"
    }

    companion object {
        private const val KEY_SERVER = "server"
        private const val REQUEST_PLAY = 1
    }
}
