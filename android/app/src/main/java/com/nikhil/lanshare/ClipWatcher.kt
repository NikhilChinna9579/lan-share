package com.nikhil.lanshare

import android.app.Activity
import android.content.ClipboardManager
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.util.Log

/**
 * Copying on this phone, sent to the laptops with no tap: the "advanced" mode.
 *
 * Android 10+ lets only the app on screen read the clipboard, and doesn't even tell a
 * background app that something was copied. But it does write a line to the system log
 * ("Denying clipboard access to com.nikhil.lanshare…") when it holds back that news.
 * With the READ_LOGS permission (granted once from a computer:
 *   adb shell pm grant com.nikhil.lanshare android.permission.READ_LOGS)
 * we watch for that line, then flash an invisible screen for a moment (allowed because
 * the bubble's "display over other apps" is on) to read what was copied, and send it.
 * The same trick KDE Connect uses.
 */
object ClipWatcher {
    private const val TAG = "LanShare"
    private var process: Process? = null
    private var listener: ClipboardManager.OnPrimaryClipChangedListener? = null
    @Volatile var running = false
        private set
    @Volatile private var lastFire = 0L

    fun available(ctx: Context) =
        ctx.checkSelfPermission("android.permission.READ_LOGS") == PackageManager.PERMISSION_GRANTED

    fun wanted(ctx: Context) = Clip.enabled(ctx) && available(ctx)

    /** Why "notify my laptops when I copy here" isn't working yet, or null if it is (Settings). */
    fun problem(ctx: Context): String? = when {
        ClipAccessibility.isOn(ctx) -> null
        !available(ctx) -> "Off: turn it on once in Accessibility (tap for the steps)"
        !android.provider.Settings.canDrawOverlays(ctx) -> "Also needs “Display over other apps” (the bubble's permission)"
        !running -> "Open LAN Share once to start it (Android asks to allow log access)"
        else -> null
    }

    const val GRANT_COMMAND = "adb shell pm grant com.nikhil.lanshare android.permission.READ_LOGS"

    /** Starts watching (call while LAN Share is on screen: Android 13+ asks then). */
    @Synchronized
    fun start(ctx: Context) {
        if (running || !wanted(ctx)) return
        val app = ctx.applicationContext
        val cm = app.getSystemService(ClipboardManager::class.java)
        // Our listener is what makes Android log "Denying clipboard access" on each copy.
        val l = ClipboardManager.OnPrimaryClipChangedListener { /* in front: Clip.autoSend handles it */ }
        Handler(Looper.getMainLooper()).post { runCatching { cm.addPrimaryClipChangedListener(l) } }
        listener = l
        val p = runCatching {
            ProcessBuilder("logcat", "-T", "1", "-v", "brief", "ClipboardService:E", "*:S").redirectErrorStream(true).start()
        }.getOrElse { Log.w(TAG, "clipboard watcher: $it"); return }
        process = p
        running = true
        Log.i(TAG, "clipboard watcher: on")
        Thread {
            runCatching {
                p.inputStream.bufferedReader().forEachLine { line ->
                    if ("Denying clipboard access to ${app.packageName}" in line) copied(app, "system log")
                }
            }
            Log.i(TAG, "clipboard watcher: stopped")
            running = false
        }.apply { isDaemon = true; name = "clip-watch" }.start()
    }

    @Synchronized
    fun stop(ctx: Context) {
        process?.destroy(); process = null
        listener?.let { l -> runCatching { ctx.applicationContext.getSystemService(ClipboardManager::class.java).removePrimaryClipChangedListener(l) } }
        listener = null
        running = false
    }

    /** Something was copied in another app (seen in the system log, or by [ClipAccessibility]). */
    fun copied(ctx: Context, how: String) {
        if (!Clip.enabled(ctx)) return
        val now = System.currentTimeMillis()
        if (now - Clip.selfWriteAt < 1500 || now - lastFire < 700) return // our own write, or a burst
        lastFire = now
        Log.i(TAG, "clipboard watcher: copy seen ($how)")
        runCatching {
            ctx.startActivity(Intent(ctx, ClipReadActivity::class.java)
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_NO_ANIMATION or Intent.FLAG_ACTIVITY_EXCLUDE_FROM_RECENTS))
        }.onFailure { Log.w(TAG, "clipboard watcher: $it") }
    }
}

/** Invisible, for a split second: reads the clipboard (allowed while focused) and sends it. */
class ClipReadActivity : Activity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        window.setDimAmount(0f)
        Handler(Looper.getMainLooper()).postDelayed({ if (!isFinishing) finish() }, 1500) // never linger
    }

    override fun onWindowFocusChanged(hasFocus: Boolean) {
        super.onWindowFocusChanged(hasFocus)
        if (!hasFocus) return
        // Every paired laptop; one that's away just doesn't get it (no error note for that).
        Clip.autoSend(this, { DeviceStore.all(this).filter { !it.isPhone } }, force = true) { d, t ->
            Actions.sendClip(this, d, t, { e -> e.message ?: e.toString() }, quiet = true, auto = true)
        }
        finish()
        overridePendingTransition(0, 0)
    }
}
