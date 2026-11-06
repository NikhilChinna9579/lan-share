package com.nikhil.lanshare

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.net.nsd.NsdManager
import android.net.nsd.NsdServiceInfo
import android.os.Build
import android.os.IBinder
import android.provider.Settings
import java.util.concurrent.atomic.AtomicInteger

/**
 * Runs while at least one laptop is paired, so laptops can send to this phone even
 * when the app isn't open. Android requires a visible notification for that; it's set to
 * the lowest importance so it stays out of the way. Idle cost: one open socket.
 */
class ReceiveService : Service() {
    private var server: PhoneServer? = null
    private var registration: NsdManager.RegistrationListener? = null

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        Hub.init(this)
        val nm = getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(NotificationChannel(CH_READY, "Ready to receive", NotificationManager.IMPORTANCE_MIN).apply {
            description = "Shown while LAN Share can receive files from your laptops"
            setShowBadge(false)
        })
        nm.createNotificationChannel(NotificationChannel(CH_RECEIVED, "Received files", NotificationManager.IMPORTANCE_DEFAULT))

        val ready = Notification.Builder(this, CH_READY)
            .setSmallIcon(R.drawable.ic_stat)
            .setContentTitle("Ready to receive files")
            .setContentText("From " + DeviceStore.all(this).joinToString { it.name }.ifEmpty { "your laptops" })
            .setContentIntent(openApp())
            .setOngoing(true)
            // Copied something? One tap here sends it to the laptop (it shows a Copy box there).
            .addAction(Notification.Action.Builder(null, "Send copied text", PendingIntent.getActivity(this, 4,
                Intent(this, ClipSendActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_CLEAR_TASK),
                PendingIntent.FLAG_IMMUTABLE)).build())
            .build()
        readyNote = ready
        foreground()
        // Android 14+ lets people swipe this away; it comes back at the next unlock (and
        // when LAN Share opens), so "Send copied text" is never lost for good.
        val flags = if (Build.VERSION.SDK_INT >= 33) Context.RECEIVER_NOT_EXPORTED else 0
        runCatching { registerReceiver(unlocked, android.content.IntentFilter(Intent.ACTION_USER_PRESENT), flags) }
        running = true
        Hub.changed() // the app's "Ready to receive" updates right away

        val name = deviceName(this)
        runCatching {
            Identity.ensure(name)
            val srv = PhoneServer(this, name, { notifyReceived(this, it) })
            val port = srv.start()
            android.util.Log.i("LanShare", "receiving on port $port")
            server = srv
            announce(name, port)
        }.onFailure { stopSelf() }
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (DeviceStore.all(this).isEmpty() && !PhonePairing.open()) stopSelf()
        showReady("opened")
        return START_STICKY
    }

    override fun onDestroy() {
        running = false
        runCatching { unregisterReceiver(unlocked) }
        registration?.let { runCatching { getSystemService(NsdManager::class.java).unregisterService(it) } }
        server?.stop()
        super.onDestroy()
    }

    private var readyNote: Notification? = null

    private val unlocked = object : android.content.BroadcastReceiver() {
        override fun onReceive(c: Context, i: Intent) = showReady("unlock")
    }

    /** Puts "Ready to receive · Send copied text" back if it was swiped away (no sound). */
    private fun showReady(why: String) {
        val n = readyNote ?: return
        val nm = getSystemService(NotificationManager::class.java)
        if (nm.activeNotifications.any { it.id == ID_READY }) return
        runCatching { nm.notify(ID_READY, n) }
        android.util.Log.i("LanShare", "ready note: shown again ($why)")
    }
    /** Foreground: Android keeps a receiving app running only with its notification showing. */
    private fun foreground(): Boolean {
        val n = readyNote ?: return false
        return runCatching {
            if (Build.VERSION.SDK_INT >= 34) startForeground(ID_READY, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE)
            else startForeground(ID_READY, n)
        }.onFailure { android.util.Log.w("LanShare", "foreground: $it") }.isSuccess
    }

    /** Tells laptops on the Wi-Fi where to find this phone (PROTOCOL.md §2). */
    private fun announce(name: String, port: Int) {
        val info = NsdServiceInfo().apply {
            serviceName = name
            serviceType = Protocol.SERVICE_TYPE
            setPort(port)
            setAttribute("v", Protocol.VERSION.toString())
            setAttribute("id", Identity.fingerprint)
            setAttribute("p", "android")
        }
        val listener = object : NsdManager.RegistrationListener {
            override fun onServiceRegistered(info: NsdServiceInfo) {}
            override fun onRegistrationFailed(info: NsdServiceInfo, errorCode: Int) {}
            override fun onServiceUnregistered(info: NsdServiceInfo) {}
            override fun onUnregistrationFailed(info: NsdServiceInfo, errorCode: Int) {}
        }
        registration = listener
        runCatching { getSystemService(NsdManager::class.java).registerService(info, NsdManager.PROTOCOL_DNS_SD, listener) }
    }

    private fun openApp() = PendingIntent.getActivity(this, 0,
        Intent(this, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP), PendingIntent.FLAG_IMMUTABLE)

    companion object {
        private const val CH_READY = "ready"
        private const val CH_RECEIVED = "received"
        private const val ID_READY = 1
        private val nextId = AtomicInteger(100)

        /** True while the service is up (for the "Ready to receive" line in the app). */
        @Volatile var running = false
            private set

        fun start(ctx: Context) {
            if (DeviceStore.all(ctx).isEmpty() && !PhonePairing.open()) return
            runCatching { ctx.startForegroundService(Intent(ctx, ReceiveService::class.java)) }
        }

        fun stop(ctx: Context) = ctx.stopService(Intent(ctx, ReceiveService::class.java))

        /** Re-reads settings, and puts the notification back if it was swiped away. */
        fun refresh(ctx: Context) = start(ctx)

        /** Starts over, e.g. to announce a new name on the Wi-Fi. */
        fun restart(ctx: Context) { stop(ctx); start(ctx) }

        /** "Received …" notification; tapping opens the file (or Downloads for several). */
        fun notifyReceived(ctx: Context, r: PhoneServer.Received) {
            if (r.files.isEmpty()) return
            val single = r.files.singleOrNull()
            val tap = if (single != null) {
                Intent(Intent.ACTION_VIEW).setDataAndType(single.uri, single.mime)
                    .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_ACTIVITY_NEW_TASK)
            } else {
                Intent(android.app.DownloadManager.ACTION_VIEW_DOWNLOADS).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
            }
            val id = nextId.incrementAndGet()
            val pi = PendingIntent.getActivity(ctx, id, tap,
                PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
            val total = r.files.sumOf { it.size }
            val n = Notification.Builder(ctx, CH_RECEIVED)
                .setSmallIcon(R.drawable.ic_stat)
                .setContentTitle(single?.let { "Received ${it.name}" } ?: "Received ${plural(r.files.size, "file")}")
                .setContentText("From ${r.from} · ${formatSize(total)} in ${formatDuration(r.tookMs)} · in ${r.where}")
                .setContentIntent(pi)
                .setAutoCancel(true)
                .build()
            runCatching { ctx.getSystemService(NotificationManager::class.java).notify(id, n) }
        }
    }
}

/** The phone's name as set in Settings, else its model. */
fun deviceName(ctx: Context): String = Prefs.name(ctx) ?:
    Settings.Global.getString(ctx.contentResolver, Settings.Global.DEVICE_NAME)?.takeIf { it.isNotBlank() } ?: Build.MODEL
