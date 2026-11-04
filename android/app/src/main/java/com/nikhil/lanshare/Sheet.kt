package com.nikhil.lanshare

import android.app.Activity
import android.app.Dialog
import android.view.Gravity
import android.view.LayoutInflater
import android.view.View
import android.view.ViewGroup
import android.view.WindowManager
import android.widget.ImageView
import android.widget.LinearLayout
import android.widget.TextView

/**
 * A lightweight bottom sheet built on a plain Dialog (no extra libraries): a title,
 * a list of rows with icons, and Cancel. Used to pick a folder or a laptop.
 */
class Sheet(private val activity: Activity, title: String, subtitle: String?) {

    class Row(val icon: Int, val label: String, val chip: String? = null, val enabled: Boolean = true, val onPick: () -> Unit)

    private val dialog = Dialog(activity, R.style.Theme_LanShare_Sheet)
    private val view = LayoutInflater.from(activity).inflate(R.layout.sheet_picker, null)
    private val list = view.findViewById<LinearLayout>(R.id.sheetList)
    var onCancel: () -> Unit = {}

    init {
        view.findViewById<TextView>(R.id.sheetTitle).text = title
        view.findViewById<TextView>(R.id.sheetSubtitle).apply {
            text = subtitle
            visibility = if (subtitle.isNullOrEmpty()) View.GONE else View.VISIBLE
        }
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
    }

    fun add(row: Row): Sheet {
        val r = LayoutInflater.from(activity).inflate(R.layout.item_sheet_row, list, false)
        r.findViewById<ImageView>(R.id.rowIcon).setImageResource(row.icon)
        r.findViewById<TextView>(R.id.rowLabel).text = row.label
        r.findViewById<TextView>(R.id.rowChip).apply {
            text = row.chip
            visibility = if (row.chip == null) View.GONE else View.VISIBLE
        }
        r.isEnabled = row.enabled
        r.alpha = if (row.enabled) 1f else 0.45f
        r.setOnClickListener {
            dialog.dismiss()
            row.onPick()
        }
        list.addView(r)
        return this
    }

    fun show() = dialog.show()

    private fun dp(v: Int) = (v * activity.resources.displayMetrics.density).toInt()
}

/** Icon for a folder key offered by the other device. */
fun folderIcon(key: String): Int = when {
    key == "downloads" -> R.drawable.ic_download
    key == "documents" -> R.drawable.ic_document
    key == "pictures" -> R.drawable.ic_image
    key == "desktop" -> R.drawable.ic_desktop
    else -> R.drawable.ic_folder
}

@Suppress("DEPRECATION")
fun systemBottomInset(insets: android.view.WindowInsets): Int =
    if (android.os.Build.VERSION.SDK_INT >= 30) {
        insets.getInsets(android.view.WindowInsets.Type.systemBars()).bottom
    } else {
        insets.systemWindowInsetBottom
    }
