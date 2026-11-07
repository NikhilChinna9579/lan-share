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
 * way takes one tap (Send copied text: tile or notification; Send sheet; Share → LAN Share).
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

    /** "Ask with a Copy button" (default) instead of putting arriving text straight on the clipboard. */
    fun askFirst(ctx: Context) = ctx.getSharedPreferences("settings", Context.MODE_PRIVATE).getBoolean("clip_ask", true)
    fun setAskFirst(ctx: Context, on: Boolean) =
        ctx.getSharedPreferences("settings", Context.MODE_PRIVATE).edit().putBoolean("clip_ask", on).apply()

    /** Text copied on a laptop arrived: onto the clipboard, or offered with a Copy button. */
    fun incoming(ctx: Context, text: String, from: String, auto: Boolean) {
        if (!auto || !askFirst(ctx)) return write(ctx, text, from) // a deliberate send: straight in
        val app = ctx.applicationContext
        val dir = java.io.File(app.filesDir, "clip_in").apply { mkdirs() }
        dir.listFiles()?.sortedByDescending { it.lastModified() }?.drop(4)?.forEach { it.delete() } // keep a few
        val id = (System.currentTimeMillis() % 1_000_000).toInt()
        java.io.File(dir, "$id.txt").writeText(text)
        java.io.File(dir, "$id.from").writeText(from)
        Log.i("LanShare", "clipboard from $from: ${text.length} chars, offered (Copy button)")
        val nm = app.getSystemService(NotificationManager::class.java)
        // High: it pops up for a moment with its Copy button showing (silently).
        nm.createNotificationChannel(NotificationChannel(CH_ASK, "Copied on your other devices", NotificationManager.IMPORTANCE_HIGH).apply {
            setSound(null, null); enableVibration(false)
        })
        val copy = android.app.PendingIntent.getBroadcast(app, id,
            android.content.Intent(app, CopyReceiver::class.java).putExtra("id", id),
            android.app.PendingIntent.FLAG_IMMUTABLE or android.app.PendingIntent.FLAG_UPDATE_CURRENT)
        val b = Notification.Builder(app, CH_ASK)
            .setSmallIcon(R.drawable.ic_stat)
            .setContentTitle("Copied on $from")
            .setContentText(oneLine(text))
            .setStyle(Notification.BigTextStyle().bigText(text.take(400)))
            .setContentIntent(copy) // tapping it copies too
        linkIn(text)?.let { b.addAction(openAction(app, it, NOTE_ASK, id)) } // a link: Open, before Copy
        val n = b.addAction(Notification.Action.Builder(null, "Copy", copy).build())
            .setAutoCancel(true)
            .setGroup("clipboard") // its own: never folded in with "Ready to receive", hiding Copy
            .setCategory(Notification.CATEGORY_STATUS)
            .setTimeoutAfter(10 * 60_000)
            .build()
        runCatching { nm.notify(NOTE_ASK, n) }
    }

    /** The notification's Copy button. */
    class CopyReceiver : android.content.BroadcastReceiver() {
        override fun onReceive(ctx: Context, intent: android.content.Intent) {
            val id = intent.getIntExtra("id", -1)
            val dir = java.io.File(ctx.filesDir, "clip_in")
            val text = runCatching { java.io.File(dir, "$id.txt").readText() }.getOrNull() ?: return
            val from = runCatching { java.io.File(dir, "$id.from").readText() }.getOrDefault("your laptop")
            ctx.getSystemService(NotificationManager::class.java).cancel(NOTE_ASK) // gone at once
            val t0 = System.currentTimeMillis()
            write(ctx, text, from, notify = false)
            Log.i("LanShare", "clipboard: Copy took ${System.currentTimeMillis() - t0} ms")
            Log.i("LanShare", "clipboard: copied what came from $from")
            android.widget.Toast.makeText(ctx, "Copied. Paste anywhere.", android.widget.Toast.LENGTH_SHORT).show()
        }
    }

    /** Puts text from [from] on this phone's clipboard; a quiet notification says so. */
    fun write(ctx: Context, text: String, from: String, notify: Boolean = true) {
        val app = ctx.applicationContext
        remember(app, text) // came from there: never sent back
        selfWriteAt = System.currentTimeMillis() // the watcher ignores our own change
        val put = {
            runCatching {
                app.getSystemService(ClipboardManager::class.java).setPrimaryClip(ClipData.newPlainText("From $from", text))
            }.onFailure { Log.w("LanShare", "clipboard: $it") }
        }
        if (Looper.myLooper() == Looper.getMainLooper()) {
            put() // already on the main thread (the notification's Copy button): no waiting on ourselves
        } else {
            val done = CountDownLatch(1)
            main.post { put(); done.countDown() }
            done.await(2, TimeUnit.SECONDS)
        }
        Log.i("LanShare", "clipboard from $from: ${text.length} chars (on the clipboard)")
        if (notify) {
            val nm = app.getSystemService(NotificationManager::class.java)
            nm.createNotificationChannel(NotificationChannel(CH, "Copied from your laptops", NotificationManager.IMPORTANCE_LOW))
            val link = linkIn(text)
            val b = Notification.Builder(app, CH)
                .setSmallIcon(R.drawable.ic_stat)
                .setContentTitle(if (link != null) "Link from $from · copied" else "Copied from $from")
                .setContentText(oneLine(text))
                .setTimeoutAfter(if (link != null) 10 * 60_000L else 60_000L)
                .setAutoCancel(true)
            if (link != null) b.addAction(openAction(app, link, NOTE_COPIED, 0)).setContentIntent(openIntent(app, link, NOTE_COPIED, 0))
            runCatching { nm.notify(NOTE_COPIED, b.build()) }
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

    // ------------------------------------------------------------- sent on its own

    /** "Send what I copy here when LAN Share opens" (on unless turned off). */
    fun autoEnabled(ctx: Context) = ctx.getSharedPreferences("settings", Context.MODE_PRIVATE).getBoolean("clip_auto", true)
    fun setAutoEnabled(ctx: Context, on: Boolean) =
        ctx.getSharedPreferences("settings", Context.MODE_PRIVATE).edit().putBoolean("clip_auto", on).apply()

    /** Tests: not while a test hook is putting something on the clipboard. */
    @Volatile var holdUntil = 0L

    private fun remember(ctx: Context, text: String) =
        ctx.getSharedPreferences("settings", Context.MODE_PRIVATE).edit().putInt("clip_last", text.hashCode()).apply()
    private fun known(ctx: Context, text: String) =
        ctx.getSharedPreferences("settings", Context.MODE_PRIVATE).getInt("clip_last", 0) == text.hashCode()

    /**
     * Android lets an app read the clipboard only while it's on screen. So the moment
     * LAN Share comes up, something newly copied on this phone goes to the laptops
     * that are online.
     * Only text copied in the last 10 minutes, not marked private, and not what just
     * came from a laptop.
     */
    fun autoSend(activity: Activity, targets: () -> List<Device>, force: Boolean = false, send: (Device, String) -> Unit) {
        if (!enabled(activity) || (!force && !autoEnabled(activity)) || System.currentTimeMillis() < holdUntil) return
        val cm = activity.getSystemService(ClipboardManager::class.java)
        val clip = runCatching { cm.primaryClip }.getOrNull() ?: return
        val age = System.currentTimeMillis() - (clip.description?.timestamp ?: 0L)
        if (age > 10 * 60_000) return
        val (text, _) = read(activity)
        if (text == null || known(activity, text)) return
        remember(activity, text)
        val to = targets()
        Log.i("LanShare", "clipboard auto: ${text.length} chars to ${to.joinToString { it.name }.ifEmpty { "nobody online" }}")
        to.forEach { send(it, text) }
    }

    /**
     * The web link in copied text: the first http(s) address ("https://youtu.be/…", or
     * "Look at this: https://amzn.in/d/…"), or null. Same rules as the laptop's linkIn.
     */
    fun linkIn(text: String): String? {
        if (text.length > 4096) return null
        for (raw in text.split(Regex("\\s+"))) {
            var f = raw.trimStart('(', '<', '[', '"', '\'').trimEnd('.', ',', ';', ':', '!', '?', '>', ']', '"', '\'')
            // A ")" ends the link only when it doesn't close a "(" inside it (Wikipedia links).
            while (f.endsWith(")") && f.count { it == '(' } < f.count { it == ')' }) f = f.dropLast(1)
            if (safeLink(f)) return f
        }
        return null
    }

    /** Only http and https with a host: a laptop can never make this phone open an app's screen. */
    fun safeLink(s: String): Boolean {
        val low = s.lowercase()
        if (!low.startsWith("http://") && !low.startsWith("https://")) return false
        if (s.any { it <= ' ' || it == '\u007f' || it == '"' }) return false
        val u = android.net.Uri.parse(s)
        return u.scheme?.lowercase() in setOf("http", "https") && !u.host.isNullOrEmpty()
    }

    private fun openIntent(app: Context, link: String, note: Int, req: Int) = android.app.PendingIntent.getActivity(app, 7000 + req,
        android.content.Intent(app, OpenLinkActivity::class.java).putExtra("link", link).putExtra("note", note)
            .addFlags(android.content.Intent.FLAG_ACTIVITY_NEW_TASK),
        android.app.PendingIntent.FLAG_IMMUTABLE or android.app.PendingIntent.FLAG_UPDATE_CURRENT)

    private fun openAction(app: Context, link: String, note: Int, req: Int) =
        Notification.Action.Builder(null, "Open", openIntent(app, link, note, req)).build()

    fun oneLine(text: String): String = text.replace(Regex("\\s+"), " ").trim().let { if (it.length > 80) it.take(80) + "…" else it }

    const val MAX = 256 * 1024
    private const val CH_ASK = "clipboard_ask"
    const val NOTE_COPIED = 11
    private const val NOTE_ASK = 12
    @Volatile var selfWriteAt = 0L
    // ClipDescription.EXTRA_IS_SENSITIVE (Android 13); the same text works with older SDKs.
    private const val EXTRA_IS_SENSITIVE = "android.content.extra.IS_SENSITIVE"
}
