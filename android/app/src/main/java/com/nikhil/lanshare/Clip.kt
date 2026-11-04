package com.nikhil.lanshare

import android.app.Activity
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.content.ClipData
import android.content.ClipDescription
import android.content.ClipboardManager
import android.content.Context
import android.os.Handler
import android.os.Looper
import android.util.Log
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger

/**
 * Shared clipboard, phone side.
 *
 * Android (10+) lets an app read the clipboard only while it's on screen, but write it any
 * time. So: text from a laptop goes straight onto this phone's clipboard; going the other
 * way takes one tap in LAN Share (bubble/assistant, Send sheet, or Share → LAN Share).
 */
object Clip {
    private const val CH = "clipboard"
    private val main = Handler(Looper.getMainLooper())

    /** LAN Share screens currently on screen (only then may we read the clipboard). */
    private val onScreen = AtomicInteger(0)
    fun screenShown() { onScreen.incrementAndGet() }
    fun screenHidden() { onScreen.updateAndGet { maxOf(0, it - 1) } }

    fun enabled(ctx: Context) = ctx.getSharedPreferences("settings", Context.MODE_PRIVATE).getBoolean("clip", true)
    fun setEnabled(ctx: Context, on: Boolean) =
        ctx.getSharedPreferences("settings", Context.MODE_PRIVATE).edit().putBoolean("clip", on).apply()

    /** Puts text from [from] on this phone's clipboard; a quiet notification says so. */
    fun write(ctx: Context, text: String, from: String, notify: Boolean = true) {
        val app = ctx.applicationContext
        val done = CountDownLatch(1)
        main.post {
            runCatching {
                app.getSystemService(ClipboardManager::class.java).setPrimaryClip(ClipData.newPlainText("From $from", text))
            }.onFailure { Log.w("LanShare", "clipboard: $it") }
            done.countDown()
        }
        done.await(2, TimeUnit.SECONDS)
        Log.i("LanShare", "clipboard from $from: ${text.length} chars")
        if (notify) {
            val nm = app.getSystemService(NotificationManager::class.java)
            nm.createNotificationChannel(NotificationChannel(CH, "Copied from your laptops", NotificationManager.IMPORTANCE_LOW))
            val n = Notification.Builder(app, CH)
                .setSmallIcon(R.drawable.ic_stat)
                .setContentTitle("Copied from $from")
                .setContentText(oneLine(text))
                .setTimeoutAfter(60_000)
                .setAutoCancel(true)
                .build()
            runCatching { nm.notify(11, n) }
        }
    }

    /**
     * This phone's clipboard text, read from a LAN Share screen. Returns the text, or
     * null and why not (nothing copied, not text, or marked sensitive, e.g. a password).
     */
    fun read(activity: Activity): Pair<String?, String?> {
        val cm = activity.getSystemService(ClipboardManager::class.java)
        val clip = runCatching { cm.primaryClip }.getOrNull() ?: return null to "Nothing is copied on this phone."
        val desc = clip.description
        if (desc?.extras?.getBoolean(EXTRA_IS_SENSITIVE) == true) {
            return null to "What's copied is marked private (a password?), so it isn't sent."
        }
        val text = (0 until clip.itemCount).joinToString("\n") { clip.getItemAt(it).coerceToText(activity)?.toString() ?: "" }.trim()
        if (text.isEmpty()) return null to "What's copied isn't text."
        if (text.toByteArray().size > MAX) return null to "What's copied is too long to share."
        return text to null
    }

    /** For a laptop's "Paste from this phone": only possible while LAN Share is on screen. */
    fun readForLaptop(ctx: Context): Pair<String?, String> {
        if (!enabled(ctx)) return null to "clipboard sharing is turned off on the phone"
        // Android only lets the app on screen read the clipboard: check that right now.
        val state = android.app.ActivityManager.RunningAppProcessInfo()
        android.app.ActivityManager.getMyMemoryState(state)
        if (onScreen.get() == 0 || state.importance > android.app.ActivityManager.RunningAppProcessInfo.IMPORTANCE_FOREGROUND) {
            return null to "LAN Share isn't open on the phone (Android only lets the app on screen read the clipboard)"
        }
        val cm = ctx.applicationContext.getSystemService(ClipboardManager::class.java)
        var result: Pair<String?, String> = null to "nothing is copied on the phone"
        val done = CountDownLatch(1)
        main.post {
            runCatching {
                val clip = cm.primaryClip
                when {
                    clip == null -> {}
                    clip.description?.extras?.getBoolean(EXTRA_IS_SENSITIVE) == true ->
                        result = null to "what's copied on the phone is private (a password?)"
                    else -> {
                        val t = (0 until clip.itemCount).joinToString("\n") { clip.getItemAt(it).coerceToText(ctx)?.toString() ?: "" }.trim()
                        if (t.isNotEmpty() && t.toByteArray().size <= MAX) result = t to ""
                    }
                }
            }
            done.countDown()
        }
        done.await(2, TimeUnit.SECONDS)
        return result
    }

    fun oneLine(text: String): String = text.replace(Regex("\\s+"), " ").trim().let { if (it.length > 80) it.take(80) + "…" else it }

    const val MAX = 256 * 1024
    // ClipDescription.EXTRA_IS_SENSITIVE (Android 13); the same text works with older SDKs.
    private const val EXTRA_IS_SENSITIVE = "android.content.extra.IS_SENSITIVE"
}
