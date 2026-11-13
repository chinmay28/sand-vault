package com.sandvault.tv

import android.app.Activity
import android.content.Intent
import android.graphics.Color
import android.graphics.drawable.GradientDrawable
import android.net.Uri
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.os.SystemClock
import android.text.TextUtils
import android.view.Gravity
import android.view.KeyEvent
import android.view.View
import android.view.ViewGroup
import android.widget.FrameLayout
import android.widget.LinearLayout
import android.widget.ProgressBar
import android.widget.TextView
import android.widget.Toast
import androidx.annotation.OptIn
import androidx.media3.common.C
import androidx.media3.common.MediaItem
import androidx.media3.common.MediaMetadata
import androidx.media3.common.PlaybackException
import androidx.media3.common.Player
import androidx.media3.common.TrackSelectionOverride
import androidx.media3.common.Tracks
import androidx.media3.common.util.UnstableApi
import androidx.media3.datasource.DataSource
import androidx.media3.datasource.DataSpec
import androidx.media3.datasource.DefaultDataSource
import androidx.media3.datasource.TransferListener
import androidx.media3.exoplayer.DefaultRenderersFactory
import androidx.media3.exoplayer.ExoPlayer
import androidx.media3.exoplayer.analytics.AnalyticsListener
import androidx.media3.exoplayer.source.DefaultMediaSourceFactory
import androidx.media3.ui.AspectRatioFrameLayout
import androidx.media3.ui.CaptionStyleCompat
import androidx.media3.ui.PlayerView
import androidx.media3.ui.SubtitleView
import java.util.concurrent.atomic.AtomicLong
import kotlin.math.abs

/**
 * Plays one film from a SAND stream link, with a player drawn for a TV.
 *
 * The link carries its own credential and is answered with ranges, so seeking
 * costs only the part of the film it lands in. ExoPlayer picks the container
 * from the bytes and hands the streams to the Fire TV's hardware decoders —
 * MKV, HEVC, AC-3, DTS passthrough — which is why this is native rather than
 * the page's own video element.
 *
 * The controls are its own rather than Media3's: those are drawn for a phone,
 * edge to edge, and a TV crops the edge of the picture, so the clock and the
 * settings ran off the screen. Everything here sits inside the TV safe area
 * (48dp at the sides, 32dp top and bottom), and the options are the ones a
 * film wants: audio track, subtitles (the film's own and the .srt beside it),
 * subtitle size, zoom, speed, what is actually playing — and Stats, which says
 * over the film as it plays what, if anything, is holding it up (see
 * PlaybackStats). The buffer is as big as the app's memory allows, and after a
 * stall the player waits for a runway of film rather than two seconds of it
 * (see BufferPlan).
 *
 * The remote:
 *   Select        show the controls / press the focused button
 *   Left, Right   seek; held, it gathers pace (10s, 30s, 1m, 3m, 5m)
 *   Play/Pause, Rewind, Fast-forward   what they say, any time
 *   Back          close a menu, then the controls, then the film
 *
 * Zoom, subtitle size and the languages last picked are remembered for the
 * next film. When it closes it hands back where it got to, so the TV page can
 * offer to resume.
 */
// The renderer fallback, the subtitle view and the resize modes are marked
// unstable in Media3 — stable in practice, but free to change between releases.
@OptIn(UnstableApi::class)
class PlayerActivity : Activity(), SeekBarView.Listener {

    private var player: ExoPlayer? = null
    private val handler = Handler(Looper.getMainLooper())
    private val prefs by lazy { getSharedPreferences("sand-player", MODE_PRIVATE) }

    private lateinit var video: PlayerView
    private lateinit var controls: FrameLayout
    private lateinit var titleView: TextView
    private lateinit var seekBar: SeekBarView
    private lateinit var positionText: TextView
    private lateinit var durationText: TextView
    private lateinit var playPause: TextView
    private lateinit var osd: TextView
    private lateinit var spinner: ProgressBar
    private lateinit var panel: OptionsPanel
    private lateinit var statsPanel: StatsOverlay
    private lateinit var runwayLabel: TextView

    /**
     * What the Stats panel says, and what it is worked out from: the bytes of
     * the film that have come in (counted on the loading thread, hence the
     * atomic), the buffer's memory, the decoders as they come and go, and the
     * times the sound ran dry. Sampled every half second whether or not the
     * panel is showing, so that it has something to say the moment it opens.
     */
    private val stats = PlaybackStats()
    private val filmBytes = AtomicLong()
    private var loadControl: RunwayLoadControl? = null

    /** How big the buffer is: as big as the memory this app is allowed makes room for. */
    private val plan by lazy { BufferPlan.forHeap(Runtime.getRuntime().maxMemory()) }
    private var videoDecoder: String? = null
    private var audioDecoder: String? = null
    private var audioUnderruns = 0L
    private var link: StatsText.Link? = null
    private var linkReadAt = 0L

    /** The button whose menu is open, to give focus back to when it closes. */
    private var opener: View? = null

    private var ended = false
    private var pendingSeek: Long? = null
    private var subtitleScale = 1f
    private var resizeMode = AspectRatioFrameLayout.RESIZE_MODE_FIT

    private val controlsVisible: Boolean get() = controls.visibility == View.VISIBLE

    private val ticker = object : Runnable {
        override fun run() {
            refreshTime()
            sampleStats()
            handler.postDelayed(this, 500)
        }
    }
    private val autoHide = Runnable {
        if (player?.isPlaying == true && !panel.isOpen && pendingSeek == null) setControlsVisible(false)
    }
    private val hideOsd = Runnable { osd.visibility = View.GONE }
    private val commitSeek = Runnable { commitPendingSeek() }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        val url = intent.getStringExtra(EXTRA_URL)
        if (url == null) {
            finish()
            return
        }
        val title = intent.getStringExtra(EXTRA_TITLE) ?: ""
        val start = intent.getLongExtra(EXTRA_START_MS, 0)

        subtitleScale = prefs.getFloat(KEY_SUBTITLE_SCALE, 1f)
        resizeMode = prefs.getInt(KEY_RESIZE, AspectRatioFrameLayout.RESIZE_MODE_FIT)

        setContentView(buildUi(title))

        // The TV's own decoders and HDMI passthrough first, then FFmpeg for
        // the audio they cannot do (DTS, TrueHD, AC-3 on sticks without it);
        // and a decoder that will not start falls through to the next.
        val renderers = DefaultRenderersFactory(this)
            .setExtensionRendererMode(DefaultRenderersFactory.EXTENSION_RENDERER_MODE_ON)
            .setEnableDecoderFallback(true)
        // The load control is Media3's with a buffer sized to the memory and a
        // runway after a stall (BufferPlan); the media source is the builder's
        // default, made here so that it counts the film's bytes as they come in.
        val filmUri = Uri.parse(url)
        val load = RunwayLoadControl(plan)
        loadControl = load
        val dataSource = DefaultDataSource.Factory(this).setTransferListener(FilmBytes(filmUri, filmBytes))
        val exo = ExoPlayer.Builder(this, renderers)
            .setLoadControl(load)
            .setMediaSourceFactory(DefaultMediaSourceFactory(dataSource))
            .build()
        player = exo
        video.player = exo
        exo.trackSelectionParameters = rememberedLanguages(exo.trackSelectionParameters.buildUpon()).build()
        exo.addListener(PlayerEvents())
        exo.addAnalyticsListener(DecoderEvents())

        val item = MediaItem.Builder()
            .setUri(filmUri)
            .setMediaMetadata(MediaMetadata.Builder().setTitle(title).build())
            .setSubtitleConfigurations(sidecarSubtitles())
            .build()
        exo.setMediaItem(item, start.coerceAtLeast(0))
        exo.prepare()
        exo.playWhenReady = true

        setControlsVisible(true, playPause)
        handler.post(ticker)
    }

    // --- Building the screen ------------------------------------------------

    private fun buildUi(title: String): View {
        val root = FrameLayout(this).apply { setBackgroundColor(Color.BLACK) }

        video = PlayerView(this).apply {
            useController = false
            isFocusable = false
            setShutterBackgroundColor(Color.BLACK)
            resizeMode = this@PlayerActivity.resizeMode
            subtitleView?.apply {
                // Above the bottom edge a TV crops, and outlined so it reads
                // over a bright scene.
                setBottomPaddingFraction(SUBTITLE_LIFT_HIDDEN)
                setApplyEmbeddedFontSizes(false)
                setStyle(
                    CaptionStyleCompat(
                        Color.WHITE, Color.TRANSPARENT, Color.TRANSPARENT,
                        CaptionStyleCompat.EDGE_TYPE_OUTLINE, Color.BLACK, null,
                    ),
                )
            }
        }
        applySubtitleScale()
        root.addView(video, FrameLayout.LayoutParams(MATCH, MATCH))

        spinner = ProgressBar(this).apply { isIndeterminate = true; visibility = View.GONE }
        root.addView(spinner, FrameLayout.LayoutParams(dp(56), dp(56), Gravity.CENTER))

        // Under the spinner while a stall waits for its runway, so the wait has
        // an end in sight.
        runwayLabel = label(this, 16f, Palette.TEXT, bold = true).apply { visibility = View.GONE }
        root.addView(runwayLabel, FrameLayout.LayoutParams(WRAP, WRAP, Gravity.CENTER).apply { topMargin = dp(120) })

        controls = FrameLayout(this)
        controls.addView(
            View(this).apply { background = scrim(GradientDrawable.Orientation.TOP_BOTTOM) },
            FrameLayout.LayoutParams(MATCH, dp(130), Gravity.TOP),
        )
        controls.addView(
            View(this).apply { background = scrim(GradientDrawable.Orientation.BOTTOM_TOP) },
            FrameLayout.LayoutParams(MATCH, dp(190), Gravity.BOTTOM),
        )

        titleView = label(this, 20f, Palette.TEXT, bold = true).apply {
            text = title
            maxLines = 1
            ellipsize = TextUtils.TruncateAt.END
        }
        controls.addView(titleView, FrameLayout.LayoutParams(MATCH, WRAP, Gravity.TOP).apply {
            setMargins(SAFE_X.dp, SAFE_Y.dp, SAFE_X.dp, 0)
        })

        val bottom = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL }
        val timeRow = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
            gravity = Gravity.CENTER_VERTICAL
        }
        positionText = label(this, 15f, Palette.TEXT).apply { text = PlayerText.time(0) }
        durationText = label(this, 15f, Palette.MUTED).apply { text = PlayerText.time(0) }
        seekBar = SeekBarView(this).apply { listener = this@PlayerActivity }
        timeRow.addView(positionText)
        timeRow.addView(seekBar, LinearLayout.LayoutParams(0, dp(28), 1f).apply { setMargins(dp(14), 0, dp(14), 0) })
        timeRow.addView(durationText)
        bottom.addView(timeRow)

        val buttons = LinearLayout(this).apply { orientation = LinearLayout.HORIZONTAL }
        playPause = pill(this, "Pause") { togglePlay() }
        val all = listOf(
            playPause,
            pill(this, "−10s") { seekBy(-10_000) },
            pill(this, "+30s") { seekBy(30_000) },
            menuButton("Audio") { openAudio() },
            menuButton("Subtitles") { openSubtitles() },
            menuButton("Zoom") { openZoom() },
            menuButton("Speed") { openSpeed() },
            menuButton("Info") { openInfo() },
            pill(this, "Stats") { toggleStats() },
        )
        for ((i, b) in all.withIndex()) {
            buttons.addView(b, LinearLayout.LayoutParams(WRAP, WRAP).apply {
                // A gap between the playback buttons and the menus.
                setMargins(if (i == 3) dp(22) else 0, 0, dp(10), 0)
            })
        }
        bottom.addView(buttons, LinearLayout.LayoutParams(WRAP, WRAP).apply { topMargin = dp(14) })

        controls.addView(bottom, FrameLayout.LayoutParams(MATCH, WRAP, Gravity.BOTTOM).apply {
            setMargins(SAFE_X.dp, 0, SAFE_X.dp, SAFE_Y.dp)
        })
        root.addView(controls, FrameLayout.LayoutParams(MATCH, MATCH))

        // Below the title and the line the on-screen messages use, and above
        // the controls, so it can stay up over the film and the controls both.
        statsPanel = StatsOverlay(this)
        root.addView(statsPanel, FrameLayout.LayoutParams(dp(400), WRAP, Gravity.TOP or Gravity.START).apply {
            setMargins(SAFE_X.dp, (SAFE_Y + 92).dp, 0, 0)
        })

        osd = label(this, 16f, Palette.TEXT, bold = true).apply {
            background = rounded(Color.argb(0xD0, 0x11, 0x18, 0x27), dpf(18f))
            setPadding(dp(18), dp(8), dp(18), dp(8))
            visibility = View.GONE
        }
        root.addView(osd, FrameLayout.LayoutParams(WRAP, WRAP, Gravity.TOP or Gravity.CENTER_HORIZONTAL).apply {
            topMargin = (SAFE_Y + 44).dp
        })

        panel = OptionsPanel(this).apply {
            onClosed = {
                (opener ?: playPause).requestFocus()
                scheduleHide()
            }
        }
        root.addView(panel, FrameLayout.LayoutParams(MATCH, MATCH))
        return root
    }

    private fun menuButton(text: String, open: () -> Unit): TextView {
        lateinit var button: TextView
        button = pill(this, text) {
            opener = button
            open()
        }
        return button
    }

    private fun scrim(orientation: GradientDrawable.Orientation) =
        GradientDrawable(orientation, intArrayOf(Color.argb(0xC8, 0, 0, 0), Color.TRANSPARENT))

    // --- The remote -----------------------------------------------------------

    override fun dispatchKeyEvent(event: KeyEvent): Boolean {
        val down = event.action == KeyEvent.ACTION_DOWN
        when (event.keyCode) {
            KeyEvent.KEYCODE_MEDIA_PLAY_PAUSE, KeyEvent.KEYCODE_MEDIA_PLAY, KeyEvent.KEYCODE_MEDIA_PAUSE -> {
                if (down && event.repeatCount == 0) {
                    when (event.keyCode) {
                        KeyEvent.KEYCODE_MEDIA_PLAY -> player?.play()
                        KeyEvent.KEYCODE_MEDIA_PAUSE -> player?.pause()
                        else -> togglePlay()
                    }
                    setControlsVisible(true)
                }
                return true
            }
            KeyEvent.KEYCODE_MEDIA_FAST_FORWARD -> { if (down) seekBy(30_000); return true }
            KeyEvent.KEYCODE_MEDIA_REWIND -> { if (down) seekBy(-10_000); return true }
            KeyEvent.KEYCODE_BACK -> {
                if (!down) {
                    when {
                        panel.isOpen -> panel.close()
                        controlsVisible -> setControlsVisible(false)
                        else -> finish()
                    }
                }
                return true
            }
        }

        if (panel.isOpen) return super.dispatchKeyEvent(event)

        if (!controlsVisible) {
            if (down) {
                when (event.keyCode) {
                    KeyEvent.KEYCODE_DPAD_LEFT, KeyEvent.KEYCODE_DPAD_RIGHT -> {
                        // Straight to seeking, with the bar up to show where to.
                        setControlsVisible(true, seekBar)
                        onScrub(if (event.keyCode == KeyEvent.KEYCODE_DPAD_LEFT) -1 else 1, event.repeatCount)
                        return true
                    }
                    KeyEvent.KEYCODE_DPAD_CENTER, KeyEvent.KEYCODE_ENTER, KeyEvent.KEYCODE_DPAD_UP,
                    KeyEvent.KEYCODE_DPAD_DOWN, KeyEvent.KEYCODE_MENU -> {
                        setControlsVisible(true, playPause)
                        return true
                    }
                }
            }
            return super.dispatchKeyEvent(event)
        }

        scheduleHide()
        return super.dispatchKeyEvent(event)
    }

    private fun setControlsVisible(visible: Boolean, focus: View? = null) {
        controls.visibility = if (visible) View.VISIBLE else View.GONE
        // Subtitles sit just above the cropped bottom edge, which is where
        // the controls are; while those are up, the subtitles move above them.
        video.subtitleView?.setBottomPaddingFraction(if (visible) SUBTITLE_LIFT_CONTROLS else SUBTITLE_LIFT_HIDDEN)
        if (visible) {
            refreshTime()
            when {
                focus != null -> focus.requestFocus()
                !controls.hasFocus() -> playPause.requestFocus()
            }
            scheduleHide()
        } else {
            handler.removeCallbacks(autoHide)
        }
    }

    private fun scheduleHide() {
        handler.removeCallbacks(autoHide)
        handler.postDelayed(autoHide, 5000)
    }

    private fun showOsd(text: String) {
        osd.text = text
        osd.visibility = View.VISIBLE
        handler.removeCallbacks(hideOsd)
        handler.postDelayed(hideOsd, 1800)
    }

    // --- Playing and seeking --------------------------------------------------

    private fun togglePlay() {
        val p = player ?: return
        if (p.playbackState == Player.STATE_ENDED) p.seekTo(0)
        p.playWhenReady = !p.playWhenReady
        updatePlayPause()
    }

    private fun updatePlayPause() {
        playPause.text = if (player?.playWhenReady == true) "Pause" else "Play"
    }

    private fun knownDuration(): Long? = player?.duration?.takeIf { it != C.TIME_UNSET && it > 0 }

    private fun seekBy(delta: Long) {
        val p = player ?: return
        val duration = knownDuration() ?: return
        val to = (p.currentPosition + delta).coerceIn(0, duration)
        p.seekTo(to)
        showOsd((if (delta < 0) "−" else "+") + PlayerText.time(abs(delta)) + "   " + PlayerText.time(to))
        refreshTime()
    }

    override fun onScrub(direction: Int, presses: Int) {
        val p = player ?: return
        val duration = knownDuration() ?: return
        val from = pendingSeek ?: p.currentPosition
        val to = (from + direction * PlayerText.seekStep(presses)).coerceIn(0, duration)
        pendingSeek = to
        seekBar.pending = to
        positionText.text = PlayerText.time(to)
        showOsd((if (direction < 0) "◀◀  " else "▶▶  ") + PlayerText.time(to))
        // Sent once the pressing stops, so a held arrow is one range request
        // to the server rather than one per step.
        handler.removeCallbacks(commitSeek)
        handler.postDelayed(commitSeek, 700)
        scheduleHide()
    }

    override fun onCommit() {
        handler.removeCallbacks(commitSeek)
        if (pendingSeek != null) commitPendingSeek() else togglePlay()
    }

    private fun commitPendingSeek() {
        pendingSeek?.let { player?.seekTo(it) }
        pendingSeek = null
        seekBar.pending = null
        refreshTime()
    }

    private fun refreshTime() {
        val p = player ?: return
        val duration = knownDuration() ?: 0
        seekBar.duration = duration
        seekBar.buffered = p.bufferedPosition
        durationText.text = PlayerText.time(duration)
        if (pendingSeek == null) {
            seekBar.position = p.currentPosition
            positionText.text = PlayerText.time(p.currentPosition)
        }
    }

    // --- Stats ----------------------------------------------------------------

    private fun toggleStats() {
        val showing = !statsPanel.isShowing
        statsPanel.visibility = if (showing) View.VISIBLE else View.GONE
        if (showing) showStats(stats.report())
        showOsd(if (showing) "Stats on" else "Stats off")
    }

    /**
     * One look at the player, for PlaybackStats; the runway after a stall set
     * from what it measures; and what is on screen brought up to date.
     */
    private fun sampleStats() {
        val p = player ?: return
        val video = p.videoDecoderCounters?.also { it.ensureUpdated() }
        stats.add(
            PlaybackStats.Sample(
                atMs = SystemClock.elapsedRealtime(),
                filmBytes = filmBytes.get(),
                loading = p.isLoading,
                bufferedToMs = p.bufferedPosition,
                positionMs = p.currentPosition,
                waiting = p.playbackState == Player.STATE_BUFFERING && p.playWhenReady,
                playing = p.isPlaying,
                speed = p.playbackParameters.speed,
                framesRendered = video?.renderedOutputBufferCount?.toLong() ?: 0,
                framesDropped = video?.droppedBufferCount?.toLong() ?: 0,
                audioUnderruns = audioUnderruns,
                bufferBytes = loadControl?.allocator?.totalBytesAllocated?.toLong() ?: 0,
            ),
        )
        val report = stats.report()
        val speed = p.playbackParameters.speed
        loadControl?.runwayUs =
            BufferPlan.runwayMs(report.arrival, BufferPlan.capacityMs(plan, report.filmBitsPerSecond), speed) * 1000
        showRunway(report)
        if (statsPanel.isShowing) showStats(report)
    }

    /** While a stall waits for a runway worth waiting for, how far it has got. */
    private fun showRunway(report: PlaybackStats.Report) {
        val runwayMs = (loadControl?.runwayUs ?: 0) / 1000
        val waiting = stats.stalled && runwayMs >= RUNWAY_SHOWN_FROM_MS
        runwayLabel.visibility = if (waiting) View.VISIBLE else View.GONE
        if (waiting) runwayLabel.text = StatsText.runwayProgress(report.bufferedMs, runwayMs)
    }

    private fun showStats(report: PlaybackStats.Report) {
        val p = player ?: return
        val connection = currentLink()
        val runtime = Runtime.getRuntime()
        val playing = StatsText.Playing(
            videoMime = p.videoFormat?.sampleMimeType,
            width = p.videoFormat?.width ?: 0,
            height = p.videoFormat?.height ?: 0,
            videoDecoder = videoDecoder,
            audioMime = p.audioFormat?.sampleMimeType,
            channels = p.audioFormat?.channelCount ?: 0,
            audioDecoder = audioDecoder,
        )
        statsPanel.show(
            StatsText.headline(report, connection),
            StatsText.isProblem(report),
            StatsText.rows(
                report,
                playing,
                connection,
                heapUsedBytes = runtime.totalMemory() - runtime.freeMemory(),
                heapMaxBytes = runtime.maxMemory(),
                bufferTargetBytes = plan.targetBytes.toLong(),
                speed = p.playbackParameters.speed,
            ),
        )
    }

    /** How the TV is connected, asked again every few seconds rather than twice a second. */
    private fun currentLink(): StatsText.Link? {
        val now = SystemClock.elapsedRealtime()
        if (link == null || now - linkReadAt >= LINK_EVERY_MS) {
            link = readNetworkLink(this)
            linkReadAt = now
        }
        return link
    }

    // --- The menus ------------------------------------------------------------

    /** Every supported and unsupported track of a type, in order. */
    private fun tracksOf(type: Int): List<Track> {
        val p = player ?: return emptyList()
        val out = mutableListOf<Track>()
        for (group in p.currentTracks.groups) {
            if (group.type != type) continue
            for (i in 0 until group.length) {
                val f = group.getTrackFormat(i)
                out += Track(
                    label = PlayerText.trackLabel(f.label, f.language, f.channelCount, f.sampleMimeType, out.size + 1),
                    language = f.language,
                    supported = group.isTrackSupported(i),
                    selected = group.isTrackSelected(i),
                    override = TrackSelectionOverride(group.mediaTrackGroup, i),
                )
            }
        }
        return out
    }

    private class Track(
        val label: String,
        val language: String?,
        val supported: Boolean,
        val selected: Boolean,
        val override: TrackSelectionOverride,
    )

    private fun openAudio() {
        val p = player ?: return
        val choices = tracksOf(C.TRACK_TYPE_AUDIO).map { t ->
            if (!t.supported) {
                Choice(t.label, detail = "This TV cannot play this track")
            } else {
                Choice(t.label, selected = t.selected) {
                    p.trackSelectionParameters = p.trackSelectionParameters.buildUpon()
                        .setTrackTypeDisabled(C.TRACK_TYPE_AUDIO, false)
                        .setOverrideForType(t.override)
                        .build()
                    prefs.edit().putString(KEY_AUDIO_LANGUAGE, t.language).apply()
                    showOsd("Audio: " + t.label)
                }
            }
        }
        show("Audio", choices.ifEmpty { listOf(Choice("This film has no audio track")) })
    }

    private fun openSubtitles() {
        val p = player ?: return
        val off = p.trackSelectionParameters.disabledTrackTypes.contains(C.TRACK_TYPE_TEXT)
        val tracks = tracksOf(C.TRACK_TYPE_TEXT)
        val showing = !off && tracks.any { it.selected }

        val choices = mutableListOf<Choice>()
        choices += Choice("Off", selected = !showing) {
            p.trackSelectionParameters = p.trackSelectionParameters.buildUpon()
                .setTrackTypeDisabled(C.TRACK_TYPE_TEXT, true)
                .build()
            prefs.edit().putString(KEY_TEXT_LANGUAGE, TEXT_OFF).apply()
            showOsd("Subtitles off")
        }
        for (t in tracks) {
            choices += if (!t.supported) {
                Choice(t.label, detail = "This TV cannot show these")
            } else {
                Choice(t.label, selected = !off && t.selected) {
                    p.trackSelectionParameters = p.trackSelectionParameters.buildUpon()
                        .setTrackTypeDisabled(C.TRACK_TYPE_TEXT, false)
                        .setOverrideForType(t.override)
                        .build()
                    prefs.edit().putString(KEY_TEXT_LANGUAGE, t.language ?: "").apply()
                    showOsd("Subtitles: " + t.label)
                }
            }
        }
        val size = SUBTITLE_SIZES.minByOrNull { abs(it.first - subtitleScale) } ?: SUBTITLE_SIZES[1]
        choices += Choice("Text size: " + size.second, detail = "Press OK to change", closes = false) {
            val next = SUBTITLE_SIZES[(SUBTITLE_SIZES.indexOf(size) + 1) % SUBTITLE_SIZES.size]
            subtitleScale = next.first
            prefs.edit().putFloat(KEY_SUBTITLE_SCALE, subtitleScale).apply()
            applySubtitleScale()
            openSubtitles()
            // Back on the size row, so pressing OK again keeps cycling.
            panel.focusLast()
        }
        show("Subtitles", choices)
    }

    private fun applySubtitleScale() {
        video.subtitleView?.setFractionalTextSize(SubtitleView.DEFAULT_TEXT_SIZE_FRACTION * subtitleScale)
    }

    private fun openZoom() {
        val modes = listOf(
            Triple(AspectRatioFrameLayout.RESIZE_MODE_FIT, "Fit", "The whole picture, with bars if it needs them"),
            Triple(AspectRatioFrameLayout.RESIZE_MODE_ZOOM, "Fill screen", "No bars — the edges of the picture are cut off"),
            Triple(AspectRatioFrameLayout.RESIZE_MODE_FILL, "Stretch", "Fills the screen by stretching the picture"),
        )
        show("Zoom", modes.map { (mode, name, detail) ->
            Choice(name, detail = detail, selected = resizeMode == mode) {
                resizeMode = mode
                video.resizeMode = mode
                prefs.edit().putInt(KEY_RESIZE, mode).apply()
                showOsd("Zoom: $name")
            }
        })
    }

    private fun openSpeed() {
        val p = player ?: return
        val current = p.playbackParameters.speed
        show("Speed", SPEEDS.map { speed ->
            Choice(if (speed == 1f) "Normal" else PlayerText.speed(speed), selected = abs(current - speed) < 0.01f) {
                p.setPlaybackSpeed(speed)
                showOsd("Speed " + PlayerText.speed(speed))
            }
        })
    }

    private fun openInfo() {
        val p = player ?: return
        val rows = mutableListOf<Choice>()
        rows += Choice("Playing", detail = titleView.text.toString())
        p.videoFormat?.let {
            rows += Choice("Video", detail = PlayerText.videoSummary(it.width, it.height, it.sampleMimeType, it.frameRate, it.bitrate))
        }
        p.audioFormat?.let {
            val bitrate = PlayerText.bitrateText(it.bitrate)?.let { b -> " · $b" } ?: ""
            rows += Choice("Audio", detail = PlayerText.trackLabel(it.label, it.language, it.channelCount, it.sampleMimeType, 1) + bitrate)
        }
        val subtitle = tracksOf(C.TRACK_TYPE_TEXT).firstOrNull { it.selected }
        val off = p.trackSelectionParameters.disabledTrackTypes.contains(C.TRACK_TYPE_TEXT)
        rows += Choice("Subtitles", detail = if (off || subtitle == null) "Off" else subtitle.label)
        rows += Choice(
            "Position",
            detail = PlayerText.time(p.currentPosition) + " of " + PlayerText.time(knownDuration() ?: 0) +
                "  ·  speed " + PlayerText.speed(p.playbackParameters.speed),
        )
        rows += Choice("App", detail = appVersionName(this@PlayerActivity) + "  ·  built by CM Hegday (0x434d)")
        rows += Choice("Close") {}
        show("What's playing", rows)
    }

    private fun show(heading: String, choices: List<Choice>) {
        handler.removeCallbacks(autoHide)
        // A menu with nothing to pick still needs a way out with OK.
        val closable = if (choices.any { it.onPick != null }) choices else choices + Choice("Close") {}
        panel.show(heading, closable)
    }

    // --- Remembering ------------------------------------------------------------

    private fun rememberedLanguages(
        builder: androidx.media3.common.TrackSelectionParameters.Builder,
    ): androidx.media3.common.TrackSelectionParameters.Builder {
        prefs.getString(KEY_AUDIO_LANGUAGE, null)?.takeIf { it.isNotBlank() }?.let { builder.setPreferredAudioLanguage(it) }
        when (val text = prefs.getString(KEY_TEXT_LANGUAGE, null)) {
            null -> {}
            TEXT_OFF -> builder.setTrackTypeDisabled(C.TRACK_TYPE_TEXT, true)
            "" -> builder.setTrackTypeDisabled(C.TRACK_TYPE_TEXT, false)
            else -> builder.setPreferredTextLanguage(text)
        }
        return builder
    }

    /** The subtitle files the server found beside the film. */
    private fun sidecarSubtitles(): List<MediaItem.SubtitleConfiguration> {
        val urls = intent.getStringArrayExtra(EXTRA_SUB_URLS) ?: return emptyList()
        val labels = intent.getStringArrayExtra(EXTRA_SUB_LABELS) ?: emptyArray()
        val languages = intent.getStringArrayExtra(EXTRA_SUB_LANGUAGES) ?: emptyArray()
        val mimes = intent.getStringArrayExtra(EXTRA_SUB_MIMES) ?: emptyArray()
        return urls.indices.map { i ->
            MediaItem.SubtitleConfiguration.Builder(Uri.parse(urls[i]))
                .setMimeType(mimes.getOrNull(i))
                .setLanguage(languages.getOrNull(i)?.takeIf { it.isNotBlank() })
                .setLabel(labels.getOrNull(i))
                .build()
        }
    }

    // --- What cannot be played ---------------------------------------------------

    private var warnedAboutTracks = false

    /**
     * Why a type of track cannot be played, when the film has tracks of it
     * and this TV can play none of them; null when one of them will play.
     */
    private fun unplayable(tracks: Tracks, type: Int, what: String): String? {
        val groups = tracks.groups.filter { it.type == type }
        if (groups.isEmpty() || groups.any { g -> (0 until g.length).any { g.isTrackSupported(it) } }) return null
        val codec = groups.firstNotNullOfOrNull { g ->
            (0 until g.length).map { PlayerText.codec(g.getTrackFormat(it).sampleMimeType) }.firstOrNull { it.isNotBlank() }
        }
        val named = codec?.let { " ($it)" } ?: ""
        return "This TV cannot decode this film's $what$named, so it will play without it."
    }

    /** A failure in words, rather than an error code. */
    private fun explain(error: PlaybackException): String {
        val file = intent.getStringExtra(EXTRA_URL)?.let { Uri.parse(it).lastPathSegment } ?: "this file"
        val kind = file.substringAfterLast('.', "").uppercase()
        return when (error.errorCode) {
            PlaybackException.ERROR_CODE_PARSING_CONTAINER_UNSUPPORTED ->
                "This TV cannot open ${if (kind.isNotBlank()) "$kind files" else "this kind of file"}. " +
                    "MKV, MP4, MOV, AVI, WebM, TS, MPG and FLV all play."
            PlaybackException.ERROR_CODE_PARSING_CONTAINER_MALFORMED ->
                "$file looks damaged — the player could not read it."
            PlaybackException.ERROR_CODE_DECODING_FORMAT_UNSUPPORTED ->
                "This TV has no decoder for the video in $file."
            PlaybackException.ERROR_CODE_DECODING_FORMAT_EXCEEDS_CAPABILITIES ->
                "$file is beyond what this TV can decode — too high a resolution or bitrate for it."
            PlaybackException.ERROR_CODE_DECODER_INIT_FAILED, PlaybackException.ERROR_CODE_DECODER_QUERY_FAILED ->
                "The TV's decoder would not start for $file. Try playing it again."
            PlaybackException.ERROR_CODE_AUDIO_TRACK_INIT_FAILED ->
                "The TV would not open its sound output. Try another audio track from the Audio menu."
            PlaybackException.ERROR_CODE_IO_NETWORK_CONNECTION_FAILED,
            PlaybackException.ERROR_CODE_IO_NETWORK_CONNECTION_TIMEOUT ->
                "Lost the connection to SAND. Is the server still running?"
            PlaybackException.ERROR_CODE_IO_BAD_HTTP_STATUS, PlaybackException.ERROR_CODE_IO_FILE_NOT_FOUND ->
                "SAND would not send this film — the code may have been revoked, or the vault locked."
            else -> "Could not play $file (${error.errorCodeName})."
        }
    }

    // --- Lifecycle --------------------------------------------------------------

    private inner class PlayerEvents : Player.Listener {
        override fun onPositionDiscontinuity(oldPosition: Player.PositionInfo, newPosition: Player.PositionInfo, reason: Int) {
            // What was measured before a seek is about another stretch of
            // film, and the wait after one is the seek's, not a stall.
            if (reason == Player.DISCONTINUITY_REASON_SEEK) stats.seeked()
        }

        override fun onPlaybackStateChanged(state: Int) {
            spinner.visibility = if (state == Player.STATE_BUFFERING) View.VISIBLE else View.GONE
            if (state == Player.STATE_ENDED) {
                ended = true
                finish()
            }
            refreshTime()
        }

        override fun onIsPlayingChanged(isPlaying: Boolean) {
            updatePlayPause()
            if (isPlaying) scheduleHide() else handler.removeCallbacks(autoHide)
        }

        override fun onPlayWhenReadyChanged(playWhenReady: Boolean, reason: Int) {
            updatePlayPause()
            // Paused, the controls stay up: a paused film with nothing on
            // screen looks like a frozen one.
            if (!playWhenReady) setControlsVisible(true)
        }

        override fun onPlayerError(error: PlaybackException) {
            Toast.makeText(this@PlayerActivity, explain(error), Toast.LENGTH_LONG).show()
            finish()
        }

        /**
         * A film whose video this TV cannot decode still plays its sound, to
         * a black screen that looks like a fault — and one whose audio it
         * cannot play is silent. Either way, say which and why, once.
         */
        override fun onTracksChanged(tracks: Tracks) {
            if (warnedAboutTracks) return
            val problem = unplayable(tracks, C.TRACK_TYPE_VIDEO, "picture")
                ?: unplayable(tracks, C.TRACK_TYPE_AUDIO, "sound")
                ?: return
            warnedAboutTracks = true
            Toast.makeText(this@PlayerActivity, problem, Toast.LENGTH_LONG).show()
        }
    }

    /**
     * The decoders as they come and go, and the sound running dry. A track that
     * is passed through to the TV or a receiver has no decoder at all, which is
     * how the panel tells passthrough from decoding: the one that was running
     * is released and nothing takes its place.
     */
    private inner class DecoderEvents : AnalyticsListener {
        override fun onVideoDecoderInitialized(
            eventTime: AnalyticsListener.EventTime,
            decoderName: String,
            initializedTimestampMs: Long,
            initializationDurationMs: Long,
        ) {
            videoDecoder = decoderName
        }

        override fun onVideoDecoderReleased(eventTime: AnalyticsListener.EventTime, decoderName: String) {
            if (videoDecoder == decoderName) videoDecoder = null
        }

        override fun onAudioDecoderInitialized(
            eventTime: AnalyticsListener.EventTime,
            decoderName: String,
            initializedTimestampMs: Long,
            initializationDurationMs: Long,
        ) {
            audioDecoder = decoderName
        }

        override fun onAudioDecoderReleased(eventTime: AnalyticsListener.EventTime, decoderName: String) {
            if (audioDecoder == decoderName) audioDecoder = null
        }

        override fun onAudioUnderrun(
            eventTime: AnalyticsListener.EventTime,
            bufferSize: Int,
            bufferSizeMs: Long,
            elapsedSinceLastFeedMs: Long,
        ) {
            audioUnderruns++
        }
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
            val duration = knownDuration() ?: 0
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
        handler.removeCallbacksAndMessages(null)
        player?.release()
        player = null
        super.onDestroy()
    }

    private val Int.dp: Int get() = this@PlayerActivity.dp(this)

    companion object {
        const val EXTRA_URL = "url"
        const val EXTRA_TITLE = "title"
        const val EXTRA_START_MS = "start_ms"
        const val EXTRA_ITEM_ID = "item_id"
        const val EXTRA_POSITION_MS = "position_ms"
        const val EXTRA_DURATION_MS = "duration_ms"
        const val EXTRA_SUB_URLS = "sub_urls"
        const val EXTRA_SUB_LABELS = "sub_labels"
        const val EXTRA_SUB_LANGUAGES = "sub_languages"
        const val EXTRA_SUB_MIMES = "sub_mimes"

        private const val MATCH = ViewGroup.LayoutParams.MATCH_PARENT
        private const val WRAP = ViewGroup.LayoutParams.WRAP_CONTENT

        /** The TV safe area, in dp: what a TV may crop off each edge. */
        private const val SAFE_X = 48
        private const val SAFE_Y = 32

        private const val KEY_SUBTITLE_SCALE = "subtitle_scale"
        private const val KEY_RESIZE = "resize_mode"
        private const val KEY_AUDIO_LANGUAGE = "audio_language"
        private const val KEY_TEXT_LANGUAGE = "text_language"
        private const val TEXT_OFF = "off"

        /** How far up the screen subtitles sit, without and with the controls. */
        private const val SUBTITLE_LIFT_HIDDEN = 0.1f
        private const val SUBTITLE_LIFT_CONTROLS = 0.3f

        private val SPEEDS = listOf(0.5f, 0.75f, 1f, 1.25f, 1.5f, 1.75f, 2f)
        private val SUBTITLE_SIZES = listOf(0.75f to "Small", 1f to "Normal", 1.35f to "Large", 1.7f to "Extra large")

        /** How often the Stats panel asks how the TV is connected. */
        private const val LINK_EVERY_MS = 5_000L

        /** A runway this short is not worth a line on screen: Media3 waits two seconds anyway. */
        private const val RUNWAY_SHOWN_FROM_MS = 5_000L
    }
}

/**
 * Counts the bytes of one film as the player reads them, for the Stats panel.
 * The sidecar subtitles come through the same data source; only the film
 * counts. Called on the player's loading thread.
 */
@OptIn(UnstableApi::class)
private class FilmBytes(private val film: Uri, private val count: AtomicLong) : TransferListener {
    override fun onTransferInitializing(source: DataSource, dataSpec: DataSpec, isNetwork: Boolean) {}

    override fun onTransferStart(source: DataSource, dataSpec: DataSpec, isNetwork: Boolean) {}

    override fun onBytesTransferred(source: DataSource, dataSpec: DataSpec, isNetwork: Boolean, bytesTransferred: Int) {
        if (dataSpec.uri == film) count.addAndGet(bytesTransferred.toLong())
    }

    override fun onTransferEnd(source: DataSource, dataSpec: DataSpec, isNetwork: Boolean) {}
}
