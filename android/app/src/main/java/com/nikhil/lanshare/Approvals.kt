package com.nikhil.lanshare

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.util.Log
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit

/**
 * "Ask before receiving": an incoming transfer waits until the person taps Accept or
 * Decline, in the app or on the notification. No answer within 50 s declines it
 * (the laptop gives up after a minute).
 */
object Approvals {
    private const val CH = "ask"
    private class Wait(val latch: CountDownLatch = CountDownLatch(1), @Volatile var ok: Boolean = false)
    private val waiting = ConcurrentHashMap<String, Wait>()

    fun enabled(ctx: Context) = prefs(ctx).getBoolean("ask", false)
    fun setEnabled(ctx: Context, on: Boolean) = prefs(ctx).edit().putBoolean("ask", on).apply()
    private fun prefs(ctx: Context) = ctx.getSharedPreferences("settings", Context.MODE_PRIVATE)

    /** Blocks until answered. [what] is "photo.jpg" or "3 files". */
    fun ask(ctx: Context, tid: String, from: String, what: String, total: Long, folder: String): Boolean {
        val w = Wait()
        waiting[tid] = w
        Hub.transfer(tid, true, from) {
            it.state = Hub.State.ASKING; it.name = what; it.total = total; it.folder = folder
            it.accept = { answer(tid, true) }
            it.cancel = { answer(tid, false) }
        }
        val id = tid.hashCode()
        notify(ctx, id, tid, from, what, total, folder)
        Log.i("LanShare", "approval: asking about $what from $from")
        val answered = w.latch.await(50, TimeUnit.SECONDS)
        waiting.remove(tid)
        runCatching { ctx.getSystemService(NotificationManager::class.java).cancel(id) }
        val ok = answered && w.ok
        Log.i("LanShare", "approval: ${if (ok) "accepted" else "declined"} $what from $from")
        if (ok) {
            Hub.transfer(tid, true, from) { it.state = Hub.State.RUNNING; it.accept = null; it.cancel = null; it.started = System.currentTimeMillis() }
        } else {
            Hub.transfer(tid, true, from) { it.state = Hub.State.CANCELLED; it.message = "Declined. Nothing was saved."; it.accept = null; it.cancel = null }
            Hub.add("info", "Declined $what from $from")
        }
        return ok
    }

    fun answer(tid: String, ok: Boolean) {
        val w = waiting[tid] ?: return
        w.ok = ok
        w.latch.countDown()
    }

    private fun notify(ctx: Context, id: Int, tid: String, from: String, what: String, total: Long, folder: String) {
        val nm = ctx.getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(NotificationChannel(CH, "Asks before receiving", NotificationManager.IMPORTANCE_HIGH))
        fun action(ok: Boolean) = PendingIntent.getBroadcast(ctx, id * 2 + if (ok) 1 else 0,
            Intent(ctx, Answer::class.java).putExtra("tid", tid).putExtra("ok", ok),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
        val open = PendingIntent.getActivity(ctx, id, Intent(ctx, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP),
            PendingIntent.FLAG_IMMUTABLE)
        val n = Notification.Builder(ctx, CH)
            .setSmallIcon(R.drawable.ic_stat)
            .setContentTitle("$from wants to send $what")
            .setContentText("${formatSize(total)} · into $folder")
            .setContentIntent(open)
            .setTimeoutAfter(50_000)
            .addAction(Notification.Action.Builder(null, "Decline", action(false)).build())
            .addAction(Notification.Action.Builder(null, "Accept", action(true)).build())
            .build()
        runCatching { nm.notify(id, n) }
    }

    /** The notification's Accept/Decline buttons. */
    class Answer : BroadcastReceiver() {
        override fun onReceive(context: Context, intent: Intent) {
            answer(intent.getStringExtra("tid") ?: return, intent.getBooleanExtra("ok", false))
        }
    }
}
