package com.nikhil.lanshare

import android.accessibilityservice.AccessibilityService
import android.content.ComponentName
import android.content.Context
import android.provider.Settings
import android.util.Log
import android.view.accessibility.AccessibilityEvent
import android.view.accessibility.AccessibilityNodeInfo

/**
 * "Notify my laptops when I copy on this phone", switched on once in Accessibility.
 *
 * Android doesn't tell other apps that something was copied. But on Android 13+ every
 * copy makes the system show its "Copied" preview at the bottom of the screen, and
 * many apps show a "Copied" toast or have a Copy button. This service only watches
 * for those, then [ClipWatcher.copied] reads the clipboard for a split second and
 * sends it: the laptop shows "Copied on <phone>" with a Copy button.
 * It never reads or keeps anything else on screen.
 */
class ClipAccessibility : AccessibilityService() {

    override fun onServiceConnected() {
        Log.i("LanShare", "copy detector: on")
        running = true
    }

    override fun onDestroy() { running = false; super.onDestroy() }

    override fun onInterrupt() {}

    override fun onAccessibilityEvent(e: AccessibilityEvent?) {
        e ?: return
        val pkg = e.packageName?.toString() // none for "windows changed"
        if (pkg == packageName) return
        if (debug && (pkg == null || pkg == "com.android.systemui") &&
            (e.eventType != AccessibilityEvent.TYPE_WINDOW_CONTENT_CHANGED || e.source?.viewIdResourceName?.contains("clipboard", true) == true))
            Log.d("LanShare", "copy detector: event ${AccessibilityEvent.eventTypeToString(e.eventType)} pkg=$pkg class=${e.className} id=${e.source?.viewIdResourceName}")
        when (e.eventType) {
            // The system's "Copied" preview (Android 13+) appearing: a new window, or SystemUI's
            // clipboard views showing up / changing.
            AccessibilityEvent.TYPE_WINDOWS_CHANGED ->
                if (e.windowChanges and AccessibilityEvent.WINDOWS_CHANGE_ADDED != 0 && clipboardOverlayShown()) ClipWatcher.copied(this, "copied preview")
            AccessibilityEvent.TYPE_WINDOW_STATE_CHANGED, AccessibilityEvent.TYPE_WINDOW_CONTENT_CHANGED ->
                if (pkg == "com.android.systemui" &&
                    (e.source?.viewIdResourceName?.contains("clipboard", true) == true ||
                        e.className?.contains("clipboard", true) == true || clipboardOverlayShown()))
                    ClipWatcher.copied(this, "copied preview")
            // A "Copied" toast (older Android, and many apps).
            AccessibilityEvent.TYPE_NOTIFICATION_STATE_CHANGED ->
                if (e.parcelableData == null && e.text.any { it?.toString()?.contains("copied", ignoreCase = true) == true })
                    ClipWatcher.copied(this, "copied toast")
            // Tapping a button called Copy / Copy link / Copy text: the clipboard changes right after.
            AccessibilityEvent.TYPE_VIEW_CLICKED -> {
                val label = (e.text.joinToString(" ") + " " + (e.contentDescription ?: "")).trim().lowercase()
                if (label.startsWith("copy") && label.length < 30)
                    android.os.Handler(mainLooper).postDelayed({ ClipWatcher.copied(this, "copy button") }, 400)
            }
        }
    }

    /** Is SystemUI's clipboard preview on screen? (Its views are named clipboard_….) */
    private val debug by lazy { applicationInfo.flags and android.content.pm.ApplicationInfo.FLAG_DEBUGGABLE != 0 }
    @Volatile private var lastOverlayCheck = 0L

    private fun clipboardOverlayShown(): Boolean = runCatching {
        val now = System.currentTimeMillis()
        if (now - lastOverlayCheck < 300) return false // content changes come in bursts
        lastOverlayCheck = now
        windows.any { w ->
            if (debug) Log.d("LanShare", "copy detector: window '${w.title}' type=${w.type}")
            w.title?.toString()?.contains("clipboard", ignoreCase = true) == true ||
                w.root?.let { it.packageName == "com.android.systemui" && hasClipboardView(it, 0) } == true
        }
    }.getOrDefault(false)

    private fun hasClipboardView(n: AccessibilityNodeInfo, depth: Int): Boolean {
        if (n.viewIdResourceName?.contains("clipboard", ignoreCase = true) == true) return true
        if (depth > 5) return false
        for (i in 0 until n.childCount) n.getChild(i)?.let { if (hasClipboardView(it, depth + 1)) return true }
        return false
    }

    companion object {
        @Volatile var running = false
            private set

        /** Switched on in Settings → Accessibility (and Android has it running). */
        fun isOn(ctx: Context): Boolean {
            val me = ComponentName(ctx, ClipAccessibility::class.java)
            val list = Settings.Secure.getString(ctx.contentResolver, Settings.Secure.ENABLED_ACCESSIBILITY_SERVICES) ?: ""
            return list.split(':').any { ComponentName.unflattenFromString(it) == me }
        }
    }
}
