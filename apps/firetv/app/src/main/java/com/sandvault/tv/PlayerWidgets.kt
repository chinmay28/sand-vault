package com.sandvault.tv

import android.content.Context
import android.content.res.ColorStateList
import android.graphics.Canvas
import android.graphics.Color
import android.graphics.Paint
import android.graphics.RectF
import android.graphics.Typeface
import android.graphics.drawable.GradientDrawable
import android.graphics.drawable.StateListDrawable
import android.text.TextUtils
import android.util.TypedValue
import android.view.Gravity
import android.view.KeyEvent
import android.view.View
import android.view.ViewGroup
import android.widget.FrameLayout
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView
import kotlin.math.roundToInt

/*
 * The player's on-screen pieces, drawn for a TV: big enough to read from the
 * sofa, every one reachable with the D-pad, and focus impossible to miss.
 */

internal object Palette {
    val ACCENT = Color.rgb(0xf5, 0x9e, 0x0b)
    val TEXT = Color.rgb(0xe2, 0xe8, 0xf0)
    val MUTED = Color.rgb(0x94, 0xa3, 0xb8)
    val PANEL = Color.argb(0xF0, 0x11, 0x18, 0x27)
    val PILL = Color.argb(0x40, 0xff, 0xff, 0xff)
    val INK = Color.rgb(0x11, 0x11, 0x11)
}

internal fun Context.dp(value: Int): Int = (value * resources.displayMetrics.density).roundToInt()

internal fun Context.dpf(value: Float): Float = value * resources.displayMetrics.density

/** A rounded rectangle in one colour. */
internal fun rounded(color: Int, radius: Float): GradientDrawable = GradientDrawable().apply {
    shape = GradientDrawable.RECTANGLE
    cornerRadius = radius
    setColor(color)
}

/**
 * A button as the player draws one: a pill of text that turns amber with dark
 * text when it has focus.
 */
internal fun pill(context: Context, label: String, onClick: () -> Unit): TextView = TextView(context).apply {
    text = label
    setTextSize(TypedValue.COMPLEX_UNIT_SP, 15f)
    typeface = Typeface.DEFAULT_BOLD
    gravity = Gravity.CENTER
    isFocusable = true
    isClickable = true
    setPadding(context.dp(16), context.dp(8), context.dp(16), context.dp(8))
    val radius = context.dpf(20f)
    background = StateListDrawable().apply {
        addState(intArrayOf(android.R.attr.state_focused), rounded(Palette.ACCENT, radius))
        addState(intArrayOf(), rounded(Palette.PILL, radius))
    }
    setTextColor(
        ColorStateList(
            arrayOf(intArrayOf(android.R.attr.state_focused), intArrayOf()),
            intArrayOf(Palette.INK, Palette.TEXT),
        ),
    )
    setOnClickListener { onClick() }
}

internal fun label(context: Context, sizeSp: Float, color: Int, bold: Boolean = false): TextView = TextView(context).apply {
    setTextSize(TypedValue.COMPLEX_UNIT_SP, sizeSp)
    setTextColor(color)
    if (bold) typeface = Typeface.DEFAULT_BOLD
    setShadowLayer(context.dpf(3f), 0f, context.dpf(1f), Color.argb(0xC0, 0, 0, 0))
}

/**
 * The time bar. It can take focus: with it focused, Left and Right scrub
 * through the film — a little at a time, then faster the longer they are
 * held — and the press of Select (or a pause in pressing) seeks there.
 */
internal class SeekBarView(context: Context) : View(context) {

    interface Listener {
        /** Left (-1) or Right (+1), and how many presses in a row so far. */
        fun onScrub(direction: Int, presses: Int)
        fun onCommit()
    }

    var listener: Listener? = null

    var duration = 0L
        set(value) { field = value; invalidate() }
    var position = 0L
        set(value) { field = value; invalidate() }
    var buffered = 0L
        set(value) { field = value; invalidate() }

    /** Where a scrub has got to, drawn as the knob while it is unsent. */
    var pending: Long? = null
        set(value) { field = value; invalidate() }

    private val paint = Paint(Paint.ANTI_ALIAS_FLAG)
    private val rect = RectF()
    private var presses = 0
    private var lastPress = 0L

    init {
        isFocusable = true
        minimumHeight = context.dp(28)
    }

    override fun onKeyDown(keyCode: Int, event: KeyEvent): Boolean {
        val direction = when (keyCode) {
            KeyEvent.KEYCODE_DPAD_LEFT -> -1
            KeyEvent.KEYCODE_DPAD_RIGHT -> 1
            else -> 0
        }
        if (direction != 0) {
            val now = event.eventTime
            // Presses close together count as one long press, the same as holding.
            presses = if (event.repeatCount > 0 || now - lastPress < 900) presses + 1 else 0
            lastPress = now
            listener?.onScrub(direction, presses)
            return true
        }
        if (keyCode == KeyEvent.KEYCODE_DPAD_CENTER || keyCode == KeyEvent.KEYCODE_ENTER) {
            listener?.onCommit()
            return true
        }
        return super.onKeyDown(keyCode, event)
    }

    override fun onDraw(canvas: Canvas) {
        val h = height.toFloat()
        val w = width.toFloat()
        val thick = if (isFocused) context.dpf(6f) else context.dpf(4f)
        val top = (h - thick) / 2
        fun fraction(ms: Long) = if (duration > 0) (ms.toFloat() / duration).coerceIn(0f, 1f) else 0f

        paint.color = Color.argb(0x55, 0xff, 0xff, 0xff)
        rect.set(0f, top, w, top + thick)
        canvas.drawRoundRect(rect, thick, thick, paint)

        paint.color = Color.argb(0x88, 0xff, 0xff, 0xff)
        rect.set(0f, top, w * fraction(buffered), top + thick)
        canvas.drawRoundRect(rect, thick, thick, paint)

        paint.color = Palette.ACCENT
        rect.set(0f, top, w * fraction(position), top + thick)
        canvas.drawRoundRect(rect, thick, thick, paint)

        val knobAt = w * fraction(pending ?: position)
        paint.color = if (isFocused) Palette.ACCENT else Palette.TEXT
        canvas.drawCircle(knobAt, h / 2, if (isFocused) context.dpf(10f) else context.dpf(6f), paint)
        if (pending != null) {
            paint.color = Palette.TEXT
            canvas.drawCircle(knobAt, h / 2, context.dpf(4f), paint)
        }
    }
}

/**
 * The Stats panel: what, if anything, is holding the film up, and the figures
 * that say so — in the top corner, inside the safe area, over the film as it
 * plays. A headline in the warning colour when something is, and a line per
 * figure beneath it.
 */
internal class StatsOverlay(context: Context) : LinearLayout(context) {

    private val headline = label(context, 15f, Palette.TEXT, bold = true)
    private val lines = LinearLayout(context).apply { orientation = VERTICAL }

    val isShowing: Boolean get() = visibility == VISIBLE

    init {
        orientation = VERTICAL
        visibility = GONE
        background = rounded(Color.argb(0xC8, 0x11, 0x18, 0x27), context.dpf(12f))
        setPadding(context.dp(14), context.dp(10), context.dp(14), context.dp(12))
        headline.setPadding(0, 0, 0, context.dp(6))
        addView(headline)
        addView(lines)
    }

    fun show(title: String, problem: Boolean, rows: List<Pair<String, String>>) {
        headline.text = title
        headline.setTextColor(if (problem) Palette.ACCENT else Palette.TEXT)
        while (lines.childCount > rows.size) lines.removeViewAt(lines.childCount - 1)
        while (lines.childCount < rows.size) lines.addView(line())
        rows.forEachIndexed { i, (name, value) ->
            val line = lines.getChildAt(i) as LinearLayout
            (line.getChildAt(0) as TextView).text = name
            (line.getChildAt(1) as TextView).text = value
        }
    }

    private fun line(): LinearLayout = LinearLayout(context).apply {
        orientation = HORIZONTAL
        addView(label(context, 13f, Palette.MUTED), LayoutParams(context.dp(86), LayoutParams.WRAP_CONTENT))
        addView(
            label(context, 13f, Palette.TEXT).apply {
                maxLines = 1
                ellipsize = TextUtils.TruncateAt.END
            },
            LayoutParams(0, LayoutParams.WRAP_CONTENT, 1f),
        )
    }
}

/** One line in an options panel. */
internal data class Choice(
    val label: String,
    val detail: String? = null,
    val selected: Boolean = false,
    /** Closes the panel once picked, unless this is false. */
    val closes: Boolean = true,
    val onPick: (() -> Unit)? = null,
)

/**
 * The side panel the Audio, Subtitles, Zoom, Speed and Info buttons open: a
 * titled list down the right of the screen, inside the safe area, with the
 * current choice ticked.
 */
internal class OptionsPanel(context: Context) : FrameLayout(context) {

    private val title = label(context, 20f, Palette.TEXT, bold = true)
    private val list = LinearLayout(context).apply { orientation = LinearLayout.VERTICAL }
    private val card = LinearLayout(context)

    var onClosed: (() -> Unit)? = null

    val isOpen: Boolean get() = visibility == VISIBLE

    init {
        visibility = GONE
        // Darkens the film behind the panel a little, so the panel reads.
        setBackgroundColor(Color.argb(0x66, 0, 0, 0))
        card.orientation = LinearLayout.VERTICAL
        card.background = rounded(Palette.PANEL, context.dpf(16f))
        card.setPadding(context.dp(20), context.dp(18), context.dp(20), context.dp(18))
        title.setPadding(context.dp(8), 0, 0, context.dp(10))
        card.addView(title)
        val scroll = ScrollView(context).apply {
            isVerticalScrollBarEnabled = false
            addView(list)
        }
        card.addView(scroll, LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, 0, 1f))
        addView(
            card,
            LayoutParams(context.dp(380), ViewGroup.LayoutParams.WRAP_CONTENT, Gravity.END or Gravity.CENTER_VERTICAL).apply {
                // The safe area: TVs crop the edge of the picture.
                setMargins(context.dp(48), context.dp(32), context.dp(48), context.dp(32))
            },
        )
    }

    fun show(heading: String, choices: List<Choice>) {
        title.text = heading
        list.removeAllViews()
        val rows = choices.map { row(it) }
        rows.forEach { list.addView(it) }
        // Focus opens on the current choice, so OK straight away keeps it.
        val at = choices.indices.firstOrNull { choices[it].selected && rows[it].isFocusable }
            ?: choices.indices.firstOrNull { rows[it].isFocusable }
        visibility = VISIBLE
        at?.let { rows[it].requestFocus() }
    }

    /** Moves focus to the last row that can take it. */
    fun focusLast() {
        for (i in list.childCount - 1 downTo 0) {
            val row = list.getChildAt(i)
            if (row.isFocusable) {
                row.requestFocus()
                return
            }
        }
    }

    fun close() {
        if (!isOpen) return
        visibility = GONE
        onClosed?.invoke()
    }

    private fun row(choice: Choice): View {
        val ctx = context
        val box = LinearLayout(ctx).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(ctx.dp(12), ctx.dp(9), ctx.dp(12), ctx.dp(9))
        }
        val text = TextView(ctx).apply {
            text = (if (choice.selected) "✓  " else "     ") + choice.label
            setTextSize(TypedValue.COMPLEX_UNIT_SP, 16f)
            if (choice.selected) typeface = Typeface.DEFAULT_BOLD
        }
        box.addView(text)
        val detail = choice.detail?.let {
            TextView(ctx).apply {
                this.text = it
                setTextSize(TypedValue.COMPLEX_UNIT_SP, 13f)
                setPadding(ctx.dp(22), ctx.dp(2), 0, 0)
            }
        }
        detail?.let { box.addView(it) }

        if (choice.onPick == null) {
            // A line of information, not a choice: it is read, not visited.
            text.setTextColor(Palette.MUTED)
            detail?.setTextColor(Palette.TEXT)
            box.isFocusable = false
            return box
        }
        val radius = ctx.dpf(10f)
        box.isFocusable = true
        box.isClickable = true
        box.background = StateListDrawable().apply {
            addState(intArrayOf(android.R.attr.state_focused), rounded(Palette.ACCENT, radius))
            addState(intArrayOf(), rounded(Color.TRANSPARENT, radius))
        }
        val colors = ColorStateList(
            arrayOf(intArrayOf(android.R.attr.state_focused), intArrayOf()),
            intArrayOf(Palette.INK, Palette.TEXT),
        )
        text.setTextColor(colors)
        detail?.setTextColor(
            ColorStateList(arrayOf(intArrayOf(android.R.attr.state_focused), intArrayOf()), intArrayOf(Palette.INK, Palette.MUTED)),
        )
        // The row's own state is what the children's colours follow.
        box.isDuplicateParentStateEnabled = false
        text.isDuplicateParentStateEnabled = true
        detail?.isDuplicateParentStateEnabled = true
        box.setOnClickListener {
            choice.onPick.invoke()
            if (choice.closes) close()
        }
        return box
    }
}
