package com.nikhil.lanshare

import android.app.Activity
import android.app.Dialog
import android.view.Gravity
import android.view.LayoutInflater
import android.view.View
import android.view.ViewGroup
import android.view.WindowManager
import android.widget.Button
import android.widget.ImageView
import android.widget.LinearLayout
import android.widget.Switch
import android.widget.TextView

/**
 * A lightweight bottom sheet built on a plain Dialog (no extra libraries): title, an
 * optional breadcrumb with Back, rows, an optional note and primary button, and Cancel.
 * Rows can be replaced while it's open, so it can browse into folders.
 */
class Sheet(private val activity: Activity, private val title: String, subtitle: String?) {

    enum class Trail { NONE, CHEVRON, SWITCH_ON, SWITCH_OFF, REMOVE, CHECK_ON, CHECK_OFF }

    class Row(
        val icon: Int, val label: String, val sub: String? = null, val chip: String? = null,
        val trail: Trail = Trail.NONE, val enabled: Boolean = true, val dismiss: Boolean = true,
        val onRemove: (() -> Unit)? = null,
        val onTrail: (() -> Unit)? = null, // its own action for the tick (the row itself does something else)
        val onPick: () -> Unit,
    )

    private val dialog = Dialog(activity, R.style.Theme_LanShare_Sheet)
    private val view = LayoutInflater.from(activity).inflate(R.layout.sheet_picker, null)
    private val list = view.findViewById<LinearLayout>(R.id.sheetList)
    private val subtitleView = view.findViewById<TextView>(R.id.sheetSubtitle)
    var onCancel: () -> Unit = {}

    init {
        view.findViewById<TextView>(R.id.sheetTitle).text = title
        setSubtitle(subtitle)
        view.findViewById<View>(R.id.sheetCancel).setOnClickListener { dialog.cancel() }
        dialog.setOnCancelListener { onCancel() }
        dialog.setContentView(view)
        dialog.window?.apply {
            setLayout(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT)
            setGravity(Gravity.BOTTOM)
            addFlags(WindowManager.LayoutParams.FLAG_DIM_BEHIND)
        }
        // Keep the sheet's content above the gesture/navigation bar.
        view.setOnApplyWindowInsetsListener { v, insets ->
            v.setPadding(0, 0, 0, systemBottomInset(insets) + dp(12))
            insets
        }
        // Long lists scroll instead of pushing the buttons off screen.
        view.findViewById<View>(R.id.sheetScroll).layoutParams.height = ViewGroup.LayoutParams.WRAP_CONTENT
    }

    fun setSubtitle(s: String?) {
        subtitleView.text = s
        subtitleView.visibility = if (s.isNullOrEmpty()) View.GONE else View.VISIBLE
    }

    private fun dpx(v: Int) = (v * activity.resources.displayMetrics.density).toInt()

    fun clear(): Sheet { list.removeAllViews(); return this }

    /** Rebuilds the rows without jumping back to the top (e.g. after ticking a file). */
    fun redraw(build: () -> Unit) {
        val scroll = view.findViewById<View>(R.id.sheetScroll)
        val y = scroll.scrollY
        clear()
        build()
        scroll.post { scroll.scrollTo(0, y) }
    }

    fun add(row: Row): Sheet {
        val r = LayoutInflater.from(activity).inflate(R.layout.item_sheet_row, list, false)
        r.findViewById<ImageView>(R.id.rowIcon).setImageResource(row.icon)
        r.findViewById<TextView>(R.id.rowLabel).text = row.label
        r.findViewById<TextView>(R.id.rowSub).apply {
            text = row.sub
            visibility = if (row.sub.isNullOrEmpty()) View.GONE else View.VISIBLE
        }
        r.findViewById<TextView>(R.id.rowChip).apply {
            text = row.chip
            visibility = if (row.chip == null) View.GONE else View.VISIBLE
        }
        r.findViewById<Switch>(R.id.rowSwitch).apply {
            visibility = if (row.trail == Trail.SWITCH_ON || row.trail == Trail.SWITCH_OFF) View.VISIBLE else View.GONE
            isChecked = row.trail == Trail.SWITCH_ON
        }
        r.findViewById<ImageView>(R.id.rowChevron).apply {
            visibility = if (row.trail in setOf(Trail.CHEVRON, Trail.CHECK_ON, Trail.CHECK_OFF)) View.VISIBLE else View.GONE
            if (row.trail == Trail.CHECK_ON || row.trail == Trail.CHECK_OFF) { // a tick box, for choosing files
                val on = row.trail == Trail.CHECK_ON
                setImageResource(if (on) R.drawable.ic_check_circle else R.drawable.ic_circle_outline)
                imageTintList = android.content.res.ColorStateList.valueOf(activity.getColor(if (on) R.color.primary else R.color.ink_muted))
                contentDescription = if (on) "Chosen" else "Not chosen"
            }
            row.onTrail?.let { act ->
                setOnClickListener { act() }
                setPadding(dpx(10), dpx(10), dpx(10), dpx(10))
                layoutParams = layoutParams.apply { width = dpx(44); height = dpx(44) }
                if (activity.applicationInfo.flags and android.content.pm.ApplicationInfo.FLAG_DEBUGGABLE != 0) post {
                    val xy = IntArray(2).also { getLocationOnScreen(it) } // tests tap it here
                    android.util.Log.d("LanShare", "ui: tick ${row.label} at ${xy[0] + width / 2},${xy[1] + height / 2}")
                }
            }
        }
        r.findViewById<View>(R.id.rowRemove).apply {
            visibility = if (row.trail == Trail.REMOVE) View.VISIBLE else View.GONE
            setOnClickListener { row.onRemove?.invoke() }
        }
        r.isEnabled = row.enabled
        r.alpha = if (row.enabled) 1f else 0.45f
        r.setOnClickListener {
            if (row.dismiss) dialog.dismiss()
            row.onPick()
        }
        list.addView(r)
        fit()
        android.util.Log.d("LanShare", "row: ${row.label}")
        return this
    }

    /** A line of text in place of rows, e.g. "Loading…" or "No subfolders here". */
    fun message(text: String): Sheet {
        val t = TextView(activity).apply {
            this.text = text
            setTextColor(activity.getColor(R.color.ink_muted))
            textSize = 14f
            setPadding(dp(16), dp(14), dp(16), dp(14))
        }
        list.addView(t)
        return this
    }

    fun crumb(text: String?, onBack: (() -> Unit)?) {
        val row = view.findViewById<View>(R.id.sheetCrumbRow)
        row.visibility = if (text == null) View.GONE else View.VISIBLE
        view.findViewById<TextView>(R.id.sheetCrumb).text = text
        view.findViewById<View>(R.id.sheetBack).setOnClickListener { onBack?.invoke() }
    }

    fun note(text: String?) {
        view.findViewById<TextView>(R.id.sheetNote).apply {
            this.text = text
            visibility = if (text.isNullOrEmpty()) View.GONE else View.VISIBLE
        }
    }

    fun primary(text: String?, enabled: Boolean = true, onClick: () -> Unit = {}) {
        view.findViewById<Button>(R.id.sheetPrimary).apply {
            visibility = if (text == null) View.GONE else View.VISIBLE
            this.text = text
            isEnabled = enabled
            setOnClickListener { dialog.dismiss(); onClick() }
        }
    }

    /** Caps the list at ~55% of the screen so Cancel and the main button stay visible. */
    private fun fit() = list.post {
        val max = (activity.resources.displayMetrics.heightPixels * 0.55).toInt()
        val scroll = view.findViewById<View>(R.id.sheetScroll)
        val want = if (list.height > max) max else ViewGroup.LayoutParams.WRAP_CONTENT
        if (scroll.layoutParams.height != want) {
            scroll.layoutParams.height = want
            scroll.requestLayout()
        }
    }

    /** True once shown (so a refresh loop knows a hidden sheet was closed, not unopened). */
    var shownOnce = false
        private set

    fun show() { dialog.show(); shownOnce = true; fit(); android.util.Log.i("LanShare", "sheet: $title") }
    fun dismiss() = dialog.dismiss()
    val isShowing get() = dialog.isShowing

    private fun dp(v: Int) = (v * activity.resources.displayMetrics.density).toInt()
}

/** Icon for a folder key. */
fun fileIcon(name: String): Int {
    val mime = SharedFolders.mimeOf(name)
    return when {
        mime.startsWith("image/") -> R.drawable.ic_image
        mime.startsWith("video/") -> R.drawable.ic_video
        mime.startsWith("audio/") -> R.drawable.ic_music
        mime == "application/pdf" || mime.startsWith("text/") || "document" in mime -> R.drawable.ic_document
        else -> R.drawable.ic_file
    }
}

fun folderIcon(key: String): Int = when (key) {
    "downloads" -> R.drawable.ic_download
    "documents" -> R.drawable.ic_document
    "pictures" -> R.drawable.ic_image
    "camera" -> R.drawable.ic_camera
    "movies", "videos" -> R.drawable.ic_video
    "music" -> R.drawable.ic_music
    "desktop" -> R.drawable.ic_desktop
    else -> R.drawable.ic_folder
}

@Suppress("DEPRECATION")
fun systemBottomInset(insets: android.view.WindowInsets): Int =
    if (android.os.Build.VERSION.SDK_INT >= 30) {
        insets.getInsets(android.view.WindowInsets.Type.systemBars()).bottom
    } else {
        insets.systemWindowInsetBottom
    }
