package com.sandvault.tv

import android.app.Activity
import android.content.Intent
import android.net.Uri
import android.os.Bundle
import android.view.KeyEvent
import android.widget.Toast
import androidx.annotation.OptIn
import androidx.media3.common.C
import androidx.media3.common.MediaItem
import androidx.media3.common.MediaMetadata
import androidx.media3.common.PlaybackException
import androidx.media3.common.Player
import androidx.media3.common.util.UnstableApi
import androidx.media3.exoplayer.DefaultRenderersFactory
import androidx.media3.exoplayer.ExoPlayer
import androidx.media3.ui.PlayerView

/**
 * Plays one film from a SAND stream link.
 *
 * The link carries its own credential and is answered with ranges, so seeking
 * costs only the part of the film it lands in. ExoPlayer picks the container
 * from the bytes and hands the streams to the Fire TV's hardware decoders,
 * which is the whole reason this is native rather than the page's own video
 * element: MKV, HEVC, AC-3 and DTS passthrough all work here and not there.
 *
 * When it closes it hands back where it got to, so the TV page can offer to
 * resume.
 */
// The renderer fallback and the controller tweaks are marked unstable in
// Media3 — stable in practice, but free to change between releases.
@OptIn(UnstableApi::class)
class PlayerActivity : Activity() {

    private var player: ExoPlayer? = null
    private lateinit var view: PlayerView
    private var ended = false

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        val url = intent.getStringExtra(EXTRA_URL)
        if (url == null) {
            finish()
            return
        }
        val title = intent.getStringExtra(EXTRA_TITLE) ?: ""
        val start = intent.getLongExtra(EXTRA_START_MS, 0)

        view = PlayerView(this).apply {
            keepScreenOn = true
            setShowSubtitleButton(true)
            controllerShowTimeoutMs = 4000
            useController = true
        }
        setContentView(view)

        // Decoder fallback: if the hardware decoder for a stream will not
        // initialise, try the next one rather than failing the film.
        val renderers = DefaultRenderersFactory(this).setEnableDecoderFallback(true)
        val exo = ExoPlayer.Builder(this, renderers)
            // Fast-forward and rewind on the remote move this much.
            .setSeekForwardIncrementMs(30_000)
            .setSeekBackIncrementMs(10_000)
            .build()
        player = exo
        view.player = exo

        exo.addListener(object : Player.Listener {
            override fun onPlaybackStateChanged(state: Int) {
                if (state == Player.STATE_ENDED) {
                    ended = true
                    finish()
                }
            }

            override fun onPlayerError(error: PlaybackException) {
                Toast.makeText(
                    this@PlayerActivity,
                    "Could not play this file: ${error.errorCodeName}",
                    Toast.LENGTH_LONG,
                ).show()
                finish()
            }
        })

        val item = MediaItem.Builder()
            .setUri(Uri.parse(url))
            .setMediaMetadata(MediaMetadata.Builder().setTitle(title).build())
            .build()
        exo.setMediaItem(item, start.coerceAtLeast(0))
        exo.prepare()
        exo.playWhenReady = true
        view.requestFocus()
    }

    override fun dispatchKeyEvent(event: KeyEvent): Boolean {
        val exo = player ?: return super.dispatchKeyEvent(event)
        if (event.action == KeyEvent.ACTION_DOWN) {
            when (event.keyCode) {
                KeyEvent.KEYCODE_MEDIA_PLAY_PAUSE -> {
                    exo.playWhenReady = !exo.playWhenReady
                    view.showController()
                    return true
                }
                KeyEvent.KEYCODE_MEDIA_FAST_FORWARD -> {
                    exo.seekForward()
                    view.showController()
                    return true
                }
                KeyEvent.KEYCODE_MEDIA_REWIND -> {
                    exo.seekBack()
                    view.showController()
                    return true
                }
                KeyEvent.KEYCODE_DPAD_CENTER, KeyEvent.KEYCODE_ENTER -> {
                    // The first press shows the controls; with them up, the
                    // controls have focus and take the press themselves.
                    if (!view.isControllerFullyVisible) {
                        view.showController()
                        return true
                    }
                }
            }
        }
        return view.dispatchKeyEvent(event) || super.dispatchKeyEvent(event)
    }

    override fun onStop() {
        super.onStop()
        // Home, or the screensaver: pause rather than keep streaming to a TV
        // nobody is watching. Back to the app resumes from here.
        player?.playWhenReady = false
    }

    override fun finish() {
        val exo = player
        if (exo != null) {
            val duration = exo.duration.takeIf { it != C.TIME_UNSET } ?: 0
            val position = if (ended) duration else exo.currentPosition
            setResult(
                RESULT_OK,
                Intent()
                    .putExtra(EXTRA_ITEM_ID, intent.getStringExtra(EXTRA_ITEM_ID))
                    .putExtra(EXTRA_POSITION_MS, position)
                    .putExtra(EXTRA_DURATION_MS, duration),
            )
        }
        super.finish()
    }

    override fun onDestroy() {
        player?.release()
        player = null
        super.onDestroy()
    }

    companion object {
        const val EXTRA_URL = "url"
        const val EXTRA_TITLE = "title"
        const val EXTRA_START_MS = "start_ms"
        const val EXTRA_ITEM_ID = "item_id"
        const val EXTRA_POSITION_MS = "position_ms"
        const val EXTRA_DURATION_MS = "duration_ms"
    }
}
