package com.nikhil.lanshare

import android.Manifest
import android.app.Activity
import android.content.ActivityNotFoundException
import android.content.Intent
import android.content.pm.ApplicationInfo
import android.content.pm.PackageManager
import android.content.res.ColorStateList
import android.graphics.drawable.GradientDrawable
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.util.Log
import android.text.format.DateUtils
import android.view.LayoutInflater
import android.view.View
import android.widget.Button
import android.widget.ImageView
import android.widget.LinearLayout
import android.widget.ProgressBar
import android.widget.TextView
import com.google.mlkit.vision.barcode.common.Barcode
import com.google.mlkit.vision.codescanner.GmsBarcodeScannerOptions
import com.google.mlkit.vision.codescanner.GmsBarcodeScanning
import java.io.File
import java.io.IOException
import java.net.ConnectException
import java.net.NoRouteToHostException
import java.net.SocketTimeoutException
import java.util.UUID
import java.util.concurrent.Executors
import javax.net.ssl.SSLException

/**
 * Home screen: paired laptops with live status, transfers in both directions as they
 * happen, and recent activity. Everything redraws from live state (Presence + Hub).
 */
class MainActivity : Activity() {
    private val worker = Executors.newCachedThreadPool()
    private val main = Handler(Looper.getMainLooper())
    private val myName get() = deviceName(this)
    private val myNameNow get() = deviceName(this)
    private lateinit var presence: Presence

    private lateinit var receiveDot: View
    private lateinit var receiveStatus: TextView
    private lateinit var card: View
    private lateinit var cardIcon: ImageView
    private lateinit var cardTitle: TextView
    private lateinit var cardSubtitle: TextView
    private lateinit var cardProgress: ProgressBar
    private lateinit var transferList: LinearLayout
    private lateinit var devicesLabel: View
    private lateinit var deviceList: LinearLayout
    private lateinit var pairAnother: View
    private lateinit var emptyState: View
    private lateinit var recentLabel: View
    private lateinit var recentList: LinearLayout

    private var pickTarget: Device? = null
    private var cardDevice: String? = null // the message card is about this laptop
    private val hideCard = Runnable { card.visibility = View.GONE; cardDevice = null }
    private val onHub: () -> Unit = { renderLive() }

    /** Runs on the main thread, but only if this screen is still alive. */
    private fun ui(block: () -> Unit) {
        main.post { if (!isFinishing && !isDestroyed) block() }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)
        edgeToEdge()
        worker.execute { RemoteCache.clean(this); DirectShare.update(this) }
        findViewById<TextView>(R.id.buildStamp).text = "LAN Share ${appVersion()}"
        Log.i(TAG, "version: ${appVersion()}")

        receiveDot = findViewById(R.id.receiveDot)
        receiveStatus = findViewById(R.id.receiveStatus)
        card = findViewById(R.id.transferCard)
        cardIcon = findViewById(R.id.transferIcon)
        cardTitle = findViewById(R.id.transferTitle)
        cardSubtitle = findViewById(R.id.transferSubtitle)
        cardProgress = findViewById(R.id.transferProgress)
        transferList = findViewById(R.id.transferList)
        devicesLabel = findViewById(R.id.devicesLabel)
        deviceList = findViewById(R.id.deviceList)
        pairAnother = findViewById(R.id.pairAnother)
        emptyState = findViewById(R.id.emptyState)
        recentLabel = findViewById(R.id.recentLabel)
        recentList = findViewById(R.id.recentList)
        findViewById<View>(R.id.recentClear).setOnClickListener { Hub.clearHistory() }

        val steps = findViewById<LinearLayout>(R.id.steps)
        listOf("Open LAN Share on your laptop", "Click “Show pairing code”", "Tap Scan QR code below and scan it").forEachIndexed { i, text ->
            val row = LayoutInflater.from(this).inflate(R.layout.item_step, steps, false)
            row.findViewById<TextView>(R.id.number).text = "${i + 1}"
            row.findViewById<TextView>(R.id.text).text = text
            steps.addView(row)
        }
        findViewById<View>(R.id.scanButton).setOnClickListener { scanQr() }
        pairAnother.setOnClickListener { pairMenu() }
        findViewById<View>(R.id.pairPhoneButton).setOnClickListener { pairMenu() }
        findViewById<View>(R.id.settingsButton).setOnClickListener { showSharedFolders(firstTime = false) }
        findViewById<View>(R.id.assistantButton).setOnClickListener { startActivity(Intent(this, AssistantActivity::class.java)) }

        Hub.init(this)
        presence = Presence(this, myName) { renderDevices() }
        worker.execute { runCatching { Identity.ensure(myName) } } // creating the key takes a moment
        if (!Prefs.setupShown(this)) ui { showSetup() } else askNotificationPermission()
        ReceiveService.start(this)
        render()
        handleShare(intent)
    }

    private var hooksRan = false

    override fun onResume() {
        super.onResume()
        foldersSheet?.let { if (it.isShowing) fillSharedFolders(it) }
        setupSheet?.let { if (it.isShowing) fillSetup(it) } // back from a settings screen
        Bubble.bringBack(this) // opening LAN Share brings a dismissed bubble back (and covers "back from granting the permission")
        main.postDelayed({ Bubble.update(this); foldersSheet?.let { if (it.isShowing) fillSharedFolders(it) } }, 800) // some phones report the permission late
        if (!hooksRan) {
            hooksRan = true
            ui { runTestHooks() } // after the screen is fully up
        }
    }

    override fun onStart() {
        super.onStart()
        Hub.listen(onHub)
        presence.start()
        Clip.screenShown()
        render()
    }

    override fun onWindowFocusChanged(hasFocus: Boolean) {
        super.onWindowFocusChanged(hasFocus)
        if (hasFocus) Clip.autoSend(this, { DeviceStore.all(this).filter { presence.status(it.id).state == Presence.State.ONLINE } }) { d, t ->
            Actions.sendClip(this, d, t, { friendly(it, d) }, auto = true)
        }
    }

    override fun onStop() {
        Clip.screenHidden()
        presence.stop()
        Hub.unlisten(onHub)
        super.onStop()
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        handleShare(intent)
    }

    override fun onDestroy() {
        worker.shutdown()
        super.onDestroy()
    }

    // ------------------------------------------------------------------ layout

    @Suppress("DEPRECATION")
    private fun edgeToEdge() {
        if (Build.VERSION.SDK_INT >= 30) {
            window.setDecorFitsSystemWindows(false)
        } else {
            val d = window.decorView
            d.systemUiVisibility = d.systemUiVisibility or View.SYSTEM_UI_FLAG_LAYOUT_STABLE or
                View.SYSTEM_UI_FLAG_LAYOUT_FULLSCREEN or View.SYSTEM_UI_FLAG_LAYOUT_HIDE_NAVIGATION
        }
        findViewById<View>(R.id.root).setOnApplyWindowInsetsListener { v, insets ->
            val top = if (Build.VERSION.SDK_INT >= 30) {
                insets.getInsets(android.view.WindowInsets.Type.systemBars()).top
            } else {
                insets.systemWindowInsetTop
            }
            v.setPadding(0, top, 0, systemBottomInset(insets))
            insets
        }
    }

    private var lastScreen = ""
    private val reported = HashMap<String, String>()

    /**
     * Debug builds: logs where a button is on screen, so automated tests can tap it even
     * while a progress bar keeps the screen busy (Android's UI reader waits for idle).
     */
    private fun reportPos(v: View, label: String) {
        if (applicationInfo.flags and ApplicationInfo.FLAG_DEBUGGABLE == 0) return
        v.post {
            if (!v.isShown) return@post
            val xy = IntArray(2).also { v.getLocationOnScreen(it) }
            val at = "${xy[0] + v.width / 2},${xy[1] + v.height / 2}"
            if (reported[label] != at) { reported[label] = at; Log.d(TAG, "ui: $label at $at") }
        }
    }

    private fun render() {
        val any = DeviceStore.all(this).isNotEmpty()
        val screen = if (any) "home: " + DeviceStore.all(this).joinToString { it.name } else "unpaired"
        if (screen != lastScreen) { lastScreen = screen; Log.i(TAG, "screen: $screen") }
        emptyState.visibility = if (any) View.GONE else View.VISIBLE
        devicesLabel.visibility = if (any) View.VISIBLE else View.GONE
        pairAnother.visibility = if (any) View.VISIBLE else View.GONE
        renderDevices()
        renderLive()
    }

    /** Header, transfers and recent activity: redrawn whenever Hub changes. */
    private fun renderLive() {
        val any = DeviceStore.all(this).isNotEmpty()
        if (any && ReceiveService.running) {
            dot(receiveDot, R.color.online)
            receiveStatus.text = "Ready to receive"
        } else {
            dot(receiveDot, R.color.offline)
            receiveStatus.text = if (any) "Starting…" else "Not paired yet"
        }
        renderTransfers()
        renderRecent()
        renderDevices()
    }

    private fun renderTransfers() {
        val list = Hub.visibleTransfers()
        while (transferList.childCount > list.size) transferList.removeViewAt(transferList.childCount - 1)
        while (transferList.childCount < list.size) {
            transferList.addView(LayoutInflater.from(this).inflate(R.layout.item_transfer, transferList, false))
        }
        list.forEachIndexed { i, t ->
            val v = transferList.getChildAt(i)
            val icon = v.findViewById<ImageView>(R.id.tIcon)
            val title = v.findViewById<TextView>(R.id.tTitle)
            val sub = v.findViewById<TextView>(R.id.tSub)
            val bar = v.findViewById<ProgressBar>(R.id.tProgress)
            val action = v.findViewById<Button>(R.id.tAction)
            val dismiss = v.findViewById<View>(R.id.tDismiss)
            val dir = if (t.inbound) "From" else "To"
            action.visibility = View.GONE
            dismiss.visibility = View.GONE
            dismiss.contentDescription = "Dismiss"
            when (t.state) {
                Hub.State.ASKING -> {
                    badge(icon, R.drawable.ic_download, R.color.primary_container, R.color.on_primary_container)
                    title.text = "${t.device} wants to send ${t.name}"
                    sub.text = "${formatSize(t.total)} · into ${t.folder} · tap Accept, or ✕ to decline"
                    bar.visibility = View.GONE
                    action.visibility = View.VISIBLE
                    action.text = "Accept"
                    action.setOnClickListener { t.accept?.invoke() }
                    reportPos(action, "Accept")
                    dismiss.visibility = View.VISIBLE
                    dismiss.contentDescription = "Decline"
                    dismiss.setOnClickListener { t.cancel?.invoke() }
                }
                Hub.State.RUNNING -> {
                    badge(icon, if (t.inbound) R.drawable.ic_download else R.drawable.ic_upload, R.color.primary_container, R.color.on_primary_container)
                    title.text = (if (t.folder == "Opening") "Opening " else if (t.inbound) "Receiving " else "Sending ") + t.name.ifEmpty { "…" }
                    val elapsed = System.currentTimeMillis() - t.started
                    sub.text = "$dir ${t.device}" + (if (t.count > 1) " · file ${t.index} of ${t.count}" else "") +
                        " · ${formatSize(t.sent)} of ${formatSize(t.total)}" +
                        (if (t.sent > 0) " · ${formatSize(t.speed)}/s" else "") + " · ${formatDuration(elapsed)}"
                    bar.visibility = View.VISIBLE
                    bar.progress = if (t.total > 0) (t.sent * 1000 / t.total).toInt() else 0
                    t.cancel?.let { stop ->
                        action.visibility = View.VISIBLE
                        action.text = "Cancel"
                        action.setOnClickListener { worker.execute { stop() } }
                        reportPos(action, "Cancel")
                    }
                }
                Hub.State.DONE -> {
                    badge(icon, R.drawable.ic_check_circle, R.color.success_container, R.color.online)
                    val what = if (t.files > 1) "${t.files} files" else t.name
                    title.text = (if (t.folder == "Opened") "Opened " else if (t.inbound) "Received " else "Sent ") + what
                    sub.text = if (t.folder == "Opened") "$dir ${t.device} · ${formatSize(t.total)} · kept for a day, not in your folders · tick it to save"
                        else "$dir ${t.device} · in ${t.folder} · ${formatSize(t.total)} in ${formatDuration(t.elapsed)} · ${formatSize(t.speed)}/s"
                    bar.visibility = View.GONE
                }
                Hub.State.ERROR, Hub.State.CANCELLED -> {
                    badge(icon, if (t.state == Hub.State.CANCELLED) R.drawable.ic_close else R.drawable.ic_error, R.color.error_container, R.color.error)
                    title.text = when {
                        t.state == Hub.State.CANCELLED -> "Cancelled " + (if (t.inbound) "receiving from " else "sending to ") + t.device
                        t.inbound -> "Receiving from ${t.device} failed"
                        else -> "Couldn't send to ${t.device}"
                    }
                    sub.text = t.message
                    bar.visibility = View.GONE
                    t.retry?.let { again ->
                        action.visibility = View.VISIBLE
                        action.text = "Retry"
                        action.setOnClickListener { Hub.dismiss(t.id); again() }
                        reportPos(action, "Retry")
                    }
                    dismiss.visibility = View.VISIBLE
                    dismiss.setOnClickListener { Hub.dismiss(t.id) }
                }
            }
        }
        // While anything is running, tick once a second so the timer moves.
        if (list.any { it.state == Hub.State.RUNNING }) {
            main.removeCallbacks(tick)
            main.postDelayed(tick, 1_000)
        }
        // Finished cards disappear by themselves a few seconds later.
        if (list.any { it.state != Hub.State.RUNNING }) {
            main.removeCallbacks(refreshLater)
            main.postDelayed(refreshLater, 2_000)
        }
    }

    private val refreshLater = Runnable { renderTransfers() }
    private val tick = Runnable { if (!isFinishing) renderTransfers() }

    private fun renderRecent() {
        val items = Hub.recent().take(8)
        recentLabel.visibility = if (items.isEmpty()) View.GONE else View.VISIBLE
        recentList.visibility = recentLabel.visibility
        recentList.removeAllViews()
        for (it in items) {
            val v = LayoutInflater.from(this).inflate(R.layout.item_recent, recentList, false)
            val (ic, bg, fg) = when (it.kind) {
                "received" -> Triple(R.drawable.ic_download, R.color.success_container, R.color.online)
                "sent" -> Triple(R.drawable.ic_upload, R.color.success_container, R.color.online)
                "error" -> Triple(R.drawable.ic_error, R.color.error_container, R.color.error)
                else -> Triple(R.drawable.ic_laptop, R.color.primary_container, R.color.on_primary_container)
            }
            badge(v.findViewById(R.id.rIcon), ic, bg, fg)
            v.findViewById<TextView>(R.id.rText).text = it.text
            v.findViewById<TextView>(R.id.rDetail).apply { text = it.detail; visibility = if (it.detail.isEmpty()) View.GONE else View.VISIBLE }
            v.findViewById<TextView>(R.id.rTime).text = ago(it.time)
            recentList.addView(v)
        }
    }

    private fun renderDevices() {
        val devices = DeviceStore.all(this)
        val keep = devices.map { it.id }.toSet()
        for (i in deviceList.childCount - 1 downTo 0) {
            if (deviceList.getChildAt(i).tag !in keep) deviceList.removeViewAt(i)
        }
        devices.forEachIndexed { index, d ->
            var row = (0 until deviceList.childCount).map { deviceList.getChildAt(it) }.firstOrNull { it.tag == d.id }
            if (row == null) {
                row = LayoutInflater.from(this).inflate(R.layout.item_device, deviceList, false)
                row.tag = d.id
                deviceList.addView(row, index)
            }
            bindDevice(row!!, d)
        }
        // A problem message about a laptop goes away as soon as that laptop is fine.
        cardDevice?.let { id -> if (presence.status(id).state == Presence.State.ONLINE) hideCard.run() }
    }

    private fun bindDevice(row: View, d: Device) {
        val s = presence.status(d.id)
        row.findViewById<TextView>(R.id.name).text = d.name
        row.findViewById<ImageView>(R.id.icon).setImageResource(if (d.isPhone) R.drawable.ic_phone else R.drawable.ic_laptop)
        val status = row.findViewById<TextView>(R.id.status)
        val dotView = row.findViewById<View>(R.id.statusDot)
        val sendBtn = row.findViewById<Button>(R.id.send)
        when (s.state) {
            Presence.State.ONLINE -> {
                status.text = "Online · ${s.host}"
                dot(dotView, R.color.online)
            }
            Presence.State.ONE_WAY -> {
                status.text = "Can't connect to it · check its firewall"
                dot(dotView, R.color.checking)
            }
            Presence.State.CHECKING -> {
                status.text = "Looking for it on the Wi-Fi…"
                dot(dotView, R.color.checking)
            }
            Presence.State.OFFLINE -> {
                status.text = "Offline" + if (s.lastSeen > 0) " · seen ${ago(s.lastSeen)}" else ""
                dot(dotView, R.color.offline)
            }
        }
        sendBtn.isEnabled = s.state == Presence.State.ONLINE
        row.findViewById<View>(R.id.get).apply {
            isEnabled = s.state == Presence.State.ONLINE
            alpha = if (isEnabled) 1f else 0.4f
            setOnClickListener { getFiles(d) }
        }
        val tap = {
            when (s.state) {
                Presence.State.ONLINE -> sendWhat(d)
                Presence.State.ONE_WAY -> showOneWay(d)
                else -> showCard(Kind.ERROR, "${d.name} is offline",
                    "Open LAN Share on it, and check both are on the same Wi-Fi.", device = d.id)
            }
        }
        row.setOnClickListener { tap() }
        sendBtn.setOnClickListener { tap() }
        row.setOnLongClickListener { confirmForget(d); true }
    }

    private fun showOneWay(d: Device) = showCard(Kind.ERROR, "This phone can't connect to ${d.name}",
        "${d.name} can reach this phone, so Windows Firewall is blocking the other direction. " +
            "On the laptop: LAN Share → Settings → Allow through Windows Firewall.", device = d.id)

    private fun dot(v: View, color: Int) {
        (v.background.mutate() as GradientDrawable).setColor(getColor(color))
    }

    private fun badge(v: ImageView, icon: Int, bg: Int, fg: Int) {
        v.setImageResource(icon)
        (v.background.mutate() as GradientDrawable).setColor(getColor(bg))
        v.imageTintList = ColorStateList.valueOf(getColor(fg))
    }

    // ------------------------------------------------------------------ message card

    private enum class Kind { WORKING, DONE, ERROR }

    /** Pairing progress and problems. Errors hide by themselves, or when fixed. */
    private fun showCard(kind: Kind, title: String, subtitle: String, device: String? = null) {
        main.removeCallbacks(hideCard)
        cardDevice = device
        card.visibility = View.VISIBLE
        cardTitle.text = title
        cardSubtitle.text = subtitle
        cardSubtitle.visibility = if (subtitle.isEmpty()) View.GONE else View.VISIBLE
        when (kind) {
            Kind.WORKING -> badge(cardIcon, R.drawable.ic_upload, R.color.primary_container, R.color.on_primary_container)
            Kind.DONE -> badge(cardIcon, R.drawable.ic_check_circle, R.color.success_container, R.color.online)
            Kind.ERROR -> badge(cardIcon, R.drawable.ic_error, R.color.error_container, R.color.error)
        }
        cardProgress.visibility = if (kind == Kind.WORKING) View.VISIBLE else View.GONE
        cardProgress.isIndeterminate = true
        if (kind != Kind.WORKING) main.postDelayed(hideCard, if (kind == Kind.ERROR) 12_000 else 6_000)
    }

    // ------------------------------------------------------------------ pairing

    private fun scanQr() {
        val options = GmsBarcodeScannerOptions.Builder().setBarcodeFormats(Barcode.FORMAT_QR_CODE).build()
        GmsBarcodeScanning.getClient(this, options).startScan()
            .addOnSuccessListener { code -> code.rawValue?.let { pair(it) } }
            .addOnFailureListener {
                showCard(Kind.ERROR, "Couldn't open the scanner",
                    "Update Google Play services and connect to the internet once, then try again.")
            }
    }

    /** Laptop (scan its QR code) or another phone (6-digit code)? */
    private fun pairMenu() {
        val sheet = Sheet(this, "Pair", "You only do this once per device.")
        sheet.add(Sheet.Row(R.drawable.ic_qr, "A laptop", sub = "Scan the QR code LAN Share shows on it") { scanQr() })
        sheet.add(Sheet.Row(R.drawable.ic_phone, "A phone nearby", sub = "Pick it, then type the code it shows") { pairNearby() })
        sheet.add(Sheet.Row(R.drawable.ic_shield, "Let another phone pair with this one", sub = "Shows a code to type on the other phone") { showPairCode() })
        sheet.show()
    }

    private fun showPairCode() {
        worker.execute { runCatching { Identity.ensure(myName) } }
        val code = PhonePairing.start()
        ReceiveService.start(this) // listening is needed to be paired with
        Log.i(TAG, "phone pairing open: code $code")
        val big = TextView(this).apply {
            text = code.chunked(3).joinToString(" ")
            textSize = 40f
            letterSpacing = 0.1f
            setTextColor(getColor(R.color.ink))
            gravity = android.view.Gravity.CENTER
            setPadding(0, dp(12), 0, dp(4))
        }
        val note = TextView(this).apply {
            setTextColor(getColor(R.color.ink_muted))
            gravity = android.view.Gravity.CENTER
            setPadding(dp(20), 0, dp(20), dp(8))
        }
        val box = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL; addView(big); addView(note) }
        val dialog = android.app.AlertDialog.Builder(this)
            .setTitle("Pairing code")
            .setView(box)
            .setNegativeButton("Stop", null)
            .setOnDismissListener { PhonePairing.stop(); main.removeCallbacksAndMessages(PAIR_TICK) }
            .show()
        fun tick() {
            val who = PhonePairing.justPaired
            if (who != null) {
                PhonePairing.justPaired = null
                dialog.dismiss()
                showCard(Kind.DONE, "Paired with $who", "You can send files to each other now.")
                render(); presence.checkNow()
                return
            }
            if (!PhonePairing.open()) { note.text = "The code expired. Close and try again."; return }
            note.text = "On the other phone: LAN Share → Pair → A phone nearby → $myName, then type this code.\nValid for ${PhonePairing.secondsLeft()} s."
            main.postAtTime({ tick() }, PAIR_TICK, android.os.SystemClock.uptimeMillis() + 1000)
        }
        tick()
    }

    private fun pairNearby() {
        val sheet = Sheet(this, "Pair a phone nearby", "On the other phone, choose “Let another phone pair with this one”.")
        fun fill() {
            if (!sheet.isShowing && sheet.shownOnce) return
            sheet.redraw {
                val phones = presence.nearbyUnpaired().filter { it.platform == "android" }
                if (phones.isEmpty()) sheet.message("Looking for phones on this Wi-Fi…")
                phones.forEach { n ->
                    sheet.add(Sheet.Row(R.drawable.ic_phone, n.name, sub = n.host) { askCode(n) })
                }
            }
            main.postDelayed({ fill() }, 1500)
        }
        sheet.show()
        fill()
    }

    private fun askCode(n: Presence.Nearby) {
        val input = android.widget.EditText(this).apply {
            hint = "6-digit code"; setSingleLine(); inputType = android.text.InputType.TYPE_CLASS_NUMBER
        }
        val box = android.widget.FrameLayout(this).apply { setPadding(dp(20), dp(8), dp(20), 0); addView(input) }
        android.app.AlertDialog.Builder(this).setTitle("Code shown on ${n.name}").setView(box)
            .setPositiveButton("Pair") { _, _ -> pairPhone(n, input.text.toString()) }
            .setNegativeButton("Cancel", null).show()
        input.requestFocus()
    }

    private fun pairPhone(n: Presence.Nearby, code: String) {
        showCard(Kind.WORKING, "Pairing…", "Talking to ${n.name}")
        worker.execute {
            val r = runCatching { Identity.ensure(myName); Client.pairPhone(this, n, code, myName) }
            Log.i(TAG, "pair phone: " + r.map { "ok with ${it.name}" }.getOrElse { "failed: $it" })
            ui {
                r.onSuccess {
                    showCard(Kind.DONE, "Paired with ${it.name}", "You can send files to each other now.")
                    Hub.add("paired", "Paired with ${it.name}")
                    ReceiveService.start(this)
                    render(); presence.checkNow()
                }.onFailure { showCard(Kind.ERROR, "Pairing didn't work", friendly(it, null)) }
            }
        }
    }

    private fun pair(qrText: String) {
        showCard(Kind.WORKING, "Pairing…", "Talking to the laptop")
        worker.execute {
            val result = runCatching { Identity.ensure(myName); Client.pair(this, qrText, myName) }
            Log.i(TAG, "pair: " + result.map { "ok with ${it.name}" }.getOrElse { "failed: $it" })
            ui {
                result.onSuccess {
                    showCard(Kind.DONE, "Paired with ${it.name}", "You won't need to scan again.")
                    Hub.add("paired", "Paired with ${it.name}")
                    ReceiveService.start(this)
                    render()
                    presence.checkNow()
                    if (!isFinishing) showSharedFolders(firstTime = true)
                }.onFailure { showCard(Kind.ERROR, "Pairing didn't work", friendly(it, null)) }
            }
        }
    }

    private fun confirmForget(d: Device) {
        android.app.AlertDialog.Builder(this)
            .setTitle("Forget ${d.name}?")
            .setMessage("It won't be able to send to this phone, and you'll need to pair again " +
                if (d.isPhone) "with a code." else "by scanning its QR code.")
            .setPositiveButton("Forget") { _, _ ->
                DeviceStore.remove(this, d.id)
                Log.i(TAG, "forgot ${d.name}")
                if (DeviceStore.all(this).isEmpty()) ReceiveService.stop(this)
                render()
            }
            .setNegativeButton("Cancel", null)
            .show()
    }

    // ------------------------------------------------------------------ shared folders on this phone

    private var foldersSheet: Sheet? = null

    private fun showSharedFolders(firstTime: Boolean) {
        val sheet = Sheet(this, if (firstTime) "Shared folders" else "Settings",
            if (firstTime) "Choose which folders your laptops can use. You can change this any time with ⚙."
            else "Your laptops can save into and get files from these folders, and nothing else on this phone.")
        foldersSheet = sheet
        fillSharedFolders(sheet)
        sheet.note("LAN Share ${appVersion()}")
        sheet.primary("Done")
        sheet.show()
    }

    private fun appVersion(): String =
        runCatching { packageManager.getPackageInfo(packageName, 0).versionName }.getOrNull() ?: "?"

    private fun fillSharedFolders(sheet: Sheet) {
        sheet.clear()
        for (r in SharedFolders.STANDARD) {
            val on = SharedFolders.isOn(this, r.key)
            sheet.add(Sheet.Row(folderIcon(r.key), r.label, sub = r.dir!!.trimEnd('/').replace("/", " / "),
                trail = if (on) Sheet.Trail.SWITCH_ON else Sheet.Trail.SWITCH_OFF, dismiss = false) {
                SharedFolders.setStandard(this, r.key, !on)
                fillSharedFolders(sheet)
            })
        }
        for (r in SharedFolders.trees(this)) {
            sheet.add(Sheet.Row(R.drawable.ic_folder, r.label, sub = "Added by you · subfolders included",
                trail = Sheet.Trail.REMOVE, dismiss = false, onRemove = {
                    SharedFolders.removeTree(this, r.key)
                    fillSharedFolders(sheet)
                }) {})
        }
        sheet.add(Sheet.Row(R.drawable.ic_add, "Add a folder…", sub = "Any folder you choose, e.g. on your SD card", dismiss = false) {
            @Suppress("DEPRECATION")
            startActivityForResult(Intent(Intent.ACTION_OPEN_DOCUMENT_TREE), REQUEST_TREE)
        })
        // This phone.
        sheet.add(Sheet.Row(R.drawable.ic_phone, "Name: $myNameNow", sub = "What your laptops call this phone · tap to change", dismiss = false) {
            val input = android.widget.EditText(this).apply { setText(myNameNow); setSingleLine(); selectAll() }
            val box = android.widget.FrameLayout(this).apply { setPadding(dp(20), dp(8), dp(20), 0); addView(input) }
            android.app.AlertDialog.Builder(this).setTitle("Rename this phone").setView(box)
                .setPositiveButton("Save") { _, _ ->
                    val n = input.text.toString().trim()
                    if (n.isNotEmpty()) {
                        Prefs.setName(this, n)
                        Hub.add("info", "Renamed this phone to $n", "Laptops see the new name from now on")
                        ReceiveService.restart(this) // announce the new name on the Wi-Fi
                        fillSharedFolders(sheet)
                    }
                }
                .setNegativeButton("Cancel", null).show()
        })
        val ask = Approvals.enabled(this)
        sheet.add(Sheet.Row(R.drawable.ic_download, "Ask before receiving files",
            sub = if (ask) "You accept or decline each time" else "Paired laptops' files are saved straight away",
            trail = if (ask) Sheet.Trail.SWITCH_ON else Sheet.Trail.SWITCH_OFF, dismiss = false) {
            Approvals.setEnabled(this, !ask); fillSharedFolders(sheet)
        })
        val browse = Prefs.allowBrowse(this)
        sheet.add(Sheet.Row(R.drawable.ic_folder, "Let laptops browse and get files",
            sub = if (browse) "Paired laptops can see and get files in these folders" else "Laptops can only send to this phone",
            trail = if (browse) Sheet.Trail.SWITCH_ON else Sheet.Trail.SWITCH_OFF, dismiss = false) {
            Prefs.setAllowBrowse(this, !browse); fillSharedFolders(sheet)
        })
        // Shared clipboard.
        val clip = Clip.enabled(this)
        sheet.add(Sheet.Row(R.drawable.ic_document, "Share the clipboard with laptops",
            sub = if (clip) "Copy on a laptop, paste here; send what you copy here in one tap" else "Off: nothing copied is shared",
            trail = if (clip) Sheet.Trail.SWITCH_ON else Sheet.Trail.SWITCH_OFF, dismiss = false) {
            Clip.setEnabled(this, !clip); fillSharedFolders(sheet)
        })
        if (clip) {
            val ask = Clip.askFirst(this)
            sheet.add(Sheet.Row(R.drawable.ic_document, "Ask before replacing my clipboard",
                sub = if (ask) "Copied on a laptop: a notification with a Copy button" else "Off: what's copied on a laptop is ready to paste right away",
                trail = if (ask) Sheet.Trail.SWITCH_ON else Sheet.Trail.SWITCH_OFF, dismiss = false) {
                Clip.setAskFirst(this, !ask); fillSharedFolders(sheet)
            })
            val auto = Clip.autoEnabled(this)
            sheet.add(Sheet.Row(R.drawable.ic_document, "Send what I copy here automatically",
                sub = if (auto) "When LAN Share or the bubble opens (Android allows it only then)" else "Off: send it from the bubble or the Send sheet",
                trail = if (auto) Sheet.Trail.SWITCH_ON else Sheet.Trail.SWITCH_OFF, dismiss = false) {
                Clip.setAutoEnabled(this, !auto); fillSharedFolders(sheet)
            })
        }
        // The floating bubble opens the assistant from any app.
        val bubbleWanted = Bubble.enabled(this)
        val bubbleBlocked = bubbleWanted && (!android.provider.Settings.canDrawOverlays(this) || Bubble.problem == Bubble.BLOCKED)
        sheet.add(Sheet.Row(R.drawable.ic_mic, "Floating assistant bubble", sub = Bubble.status(this),
            chip = if (bubbleBlocked) "Fix" else null,
            trail = if (bubbleWanted) Sheet.Trail.SWITCH_ON else Sheet.Trail.SWITCH_OFF, dismiss = false) {
            when {
                bubbleWanted && !android.provider.Settings.canDrawOverlays(this) ->
                    runCatching { startActivity(Intent(android.provider.Settings.ACTION_MANAGE_OVERLAY_PERMISSION, Uri.parse("package:$packageName"))) }
                bubbleWanted && Bubble.problem == Bubble.BLOCKED -> {
                    // The phone maker's own permission lives in the app's info page.
                    Bubble.problem = null
                    runCatching { startActivity(Intent(android.provider.Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.parse("package:$packageName"))) }
                }
                bubbleWanted -> Bubble.setEnabled(this, false)
                else -> {
                    Bubble.setEnabled(this, true)
                    if (!android.provider.Settings.canDrawOverlays(this)) {
                        showCard(Kind.WORKING, "Allow “Display over other apps”", "Turn it on for LAN Share, then come back.")
                        runCatching { startActivity(Intent(android.provider.Settings.ACTION_MANAGE_OVERLAY_PERMISSION, Uri.parse("package:$packageName"))) }
                    }
                    ReceiveService.start(this)
                }
            }
            main.postDelayed({ if (sheet.isShowing) fillSharedFolders(sheet) }, 900) // shows whether it really appeared
        })
        // "Hey Jarvis" opens the assistant without touching the phone.
        val wake = WakeWord.canListen(this)
        sheet.add(Sheet.Row(R.drawable.ic_mic, "Say “Hey Jarvis” to open the assistant",
            sub = if (wake) "On: listening on this phone only; uses some battery" else "Hands-free; works offline, nothing leaves the phone",
            trail = if (wake) Sheet.Trail.SWITCH_ON else Sheet.Trail.SWITCH_OFF, dismiss = false) {
            if (wake) {
                WakeWord.setEnabled(this, false)
            } else {
                WakeWord.setEnabled(this, true)
                if (checkSelfPermission(Manifest.permission.RECORD_AUDIO) != PackageManager.PERMISSION_GRANTED) {
                    requestPermissions(arrayOf(Manifest.permission.RECORD_AUDIO), REQUEST_MIC)
                }
            }
            ReceiveService.refresh(this)
            fillSharedFolders(sheet)
        })
        // Photos, videos and music in the standard folders are only visible with permission.
        val media = SharedFolders.canReadMedia(this)
        sheet.add(Sheet.Row(R.drawable.ic_image, "Let laptops see photos, videos and music",
            sub = if (media) "On: laptops can browse and get them" else "Tap to allow, so laptops can get them from this phone",
            chip = if (media) "On" else null, enabled = !media, dismiss = false) {
            requestPermissions(SharedFolders.mediaPermissions(), REQUEST_MEDIA)
        })
        // When something misbehaves: LAN Share's own log (nothing else from the phone).
        sheet.add(Sheet.Row(R.drawable.ic_document, "Share a diagnostic log", sub = "LAN Share's own log, to help find a problem") {
            worker.execute {
                val log = runCatching {
                    val p = ProcessBuilder("logcat", "-d", "-v", "time", "-t", "3000", "-s", "LanShare:V", "AndroidRuntime:E", "WindowManager:W")
                        .redirectErrorStream(true).start()
                    p.inputStream.bufferedReader().readText().also { p.waitFor() }
                }.getOrElse { "Couldn't read the log: $it" }
                val head = "LAN Share ${packageManager.getPackageInfo(packageName, 0).versionName} · Android ${Build.VERSION.RELEASE} (API ${Build.VERSION.SDK_INT}) · ${Build.MANUFACTURER} ${Build.MODEL}\n" +
                    "bubble: on=${Bubble.enabled(this)} allowed=${android.provider.Settings.canDrawOverlays(this)} problem=${Bubble.problem} service=${ReceiveService.running}\n\n"
                ui {
                    startActivity(Intent.createChooser(Intent(Intent.ACTION_SEND).setType("text/plain")
                        .putExtra(Intent.EXTRA_SUBJECT, "LAN Share diagnostic log").putExtra(Intent.EXTRA_TEXT, head + log.takeLast(200_000)), "Share the log"))
                }
            }
        })
        // Battery saving can pause the app, so laptops see the phone as offline.
        val pm = getSystemService(android.os.PowerManager::class.java)
        val unrestricted = pm.isIgnoringBatteryOptimizations(packageName)
        sheet.add(Sheet.Row(R.drawable.ic_shield, "Stay reachable in the background",
            sub = if (unrestricted) "On: laptops can always send to this phone" else "Tap to let laptops reach this phone while it's idle",
            chip = if (unrestricted) "On" else null, enabled = !unrestricted, dismiss = false) {
            runCatching {
                @android.annotation.SuppressLint("BatteryLife")
                val i = Intent(android.provider.Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, Uri.parse("package:$packageName"))
                startActivity(i)
            }.onFailure { startActivity(Intent(android.provider.Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS)) }
        })
    }

    // ------------------------------------------------------------------ sending

    /** Text onto a laptop's clipboard. */
    private fun sendText(d: Device, text: String) {
        showCard(Kind.WORKING, "Copying to ${d.name}…", Clip.oneLine(text))
        Actions.sendClip(this, d, text, { friendly(it, d) }) { err ->
            ui {
                if (err == null) showCard(Kind.DONE, "Copied to ${d.name}", "Paste it there with Ctrl+V.")
                else showCard(Kind.ERROR, "Couldn't copy to ${d.name}", err, device = d.id)
            }
        }
    }

    /** This phone's clipboard → a laptop's (reads it now: this screen is on top). */
    private fun sendClipboard(d: Device) {
        val (text, why) = Clip.read(this)
        if (text == null) showCard(Kind.ERROR, "Nothing to send", why ?: "") else sendText(d, text)
    }

    private fun pasteFrom(d: Device) {
        showCard(Kind.WORKING, "Getting ${d.name}'s clipboard…", "")
        Actions.pasteFrom(this, d, { friendly(it, d) }) { err ->
            ui {
                if (err == null) showCard(Kind.DONE, "Copied from ${d.name}", "Paste it anywhere on this phone.")
                else showCard(Kind.ERROR, "Couldn't get ${d.name}'s clipboard", err, device = d.id)
            }
        }
    }

    /** Files, or a whole folder? */
    private fun sendWhat(d: Device) {
        val sheet = Sheet(this, "Send to ${d.name}", null)
        if (Clip.enabled(this)) sheet.add(Sheet.Row(R.drawable.ic_document, "Send what's copied", sub = "Onto its clipboard, ready to paste") { sendClipboard(d) })
        sheet.add(Sheet.Row(R.drawable.ic_file, "Choose files", sub = "One or more") { pickFiles(d) })
        sheet.add(Sheet.Row(R.drawable.ic_folder, "Send a whole folder", sub = "With its subfolders") {
            pickTarget = d
            @Suppress("DEPRECATION")
            startActivityForResult(Intent(Intent.ACTION_OPEN_DOCUMENT_TREE), REQUEST_SEND_TREE)
        })
        sheet.show()
    }

    private fun pickFiles(target: Device) {
        pickTarget = target
        val intent = Intent(Intent.ACTION_OPEN_DOCUMENT).apply {
            addCategory(Intent.CATEGORY_OPENABLE)
            type = "*/*"
            putExtra(Intent.EXTRA_ALLOW_MULTIPLE, true)
        }
        @Suppress("DEPRECATION")
        startActivityForResult(intent, REQUEST_PICK)
    }

    @Deprecated("Activity result API needs AndroidX; the plain framework keeps the app small")
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        @Suppress("DEPRECATION")
        super.onActivityResult(requestCode, resultCode, data)
        if (resultCode != RESULT_OK || data == null) return
        if (requestCode == REQUEST_TREE) {
            data.data?.let { uri ->
                runCatching { SharedFolders.addTree(this, uri) }
                    .onSuccess { r -> Hub.add("info", "Sharing ${r.label}", "Laptops can save into it and its subfolders") }
                    .onFailure { showCard(Kind.ERROR, "Couldn't share that folder", it.message ?: "") }
                foldersSheet?.let { if (it.isShowing) fillSharedFolders(it) }
            }
            return
        }
        val target = pickTarget ?: return
        if (requestCode == REQUEST_SEND_TREE) {
            val tree = data.data ?: return
            showCard(Kind.WORKING, "Reading the folder…", "")
            worker.execute {
                val items = runCatching { Client.resolveTree(this, tree) }.getOrElse { emptyList() }
                ui {
                    hideCard.run()
                    if (items.isEmpty()) showCard(Kind.ERROR, "That folder is empty", "Or Android didn't let LAN Share read it.")
                    else chooseDest(target, items)
                }
            }
            return
        }
        if (requestCode != REQUEST_PICK) return
        val uris = mutableListOf<Uri>()
        data.clipData?.let { clip -> for (i in 0 until clip.itemCount) uris += clip.getItemAt(i).uri }
        if (uris.isEmpty()) data.data?.let { uris += it }
        if (uris.isNotEmpty()) chooseFolder(target, uris)
    }

    /** Files shared from another app ("this file" in the requirements). */
    private fun handleShare(intent: Intent?) {
        if (intent == null) return
        val uris = mutableListOf<Uri>()
        @Suppress("DEPRECATION")
        when (intent.action) {
            Intent.ACTION_SEND -> intent.getParcelableExtra<Uri>(Intent.EXTRA_STREAM)?.let { uris += it }
            Intent.ACTION_SEND_MULTIPLE -> intent.getParcelableArrayListExtra<Uri>(Intent.EXTRA_STREAM)?.let { uris += it }
            else -> return
        }
        setIntent(Intent()) // don't resend after rotation
        Hub.shared(uris) // "this file" for the assistant, for a few minutes
        // "Share → Nikhil PC" (a Direct Share target): that device, no need to ask which.
        val direct = DirectShare.deviceFor(this, intent)
        if (direct != null) Log.i(TAG, "direct share to ${direct.name}: ${uris.size} file(s)")
        val devices = direct?.let { listOf(it) } ?: DeviceStore.all(this)
        // Shared text (a link, a message): onto a laptop's clipboard, ready to paste.
        val text = intent.getStringExtra(Intent.EXTRA_TEXT)?.trim()
        if (uris.isEmpty() && !text.isNullOrEmpty()) {
            when {
                devices.isEmpty() -> showCard(Kind.ERROR, "Pair a laptop first", "Then share the text again.")
                devices.size == 1 -> sendText(devices[0], text)
                else -> {
                    val sheet = Sheet(this, "Copy to…", Clip.oneLine(text))
                    devices.forEach { d -> sheet.add(Sheet.Row(if (d.isPhone) R.drawable.ic_phone else R.drawable.ic_laptop, d.name) { sendText(d, text) }) }
                    sheet.show()
                }
            }
            return
        }
        when {
            uris.isEmpty() -> showCard(Kind.ERROR, "Nothing to send", "That app shared nothing LAN Share can send.")
            devices.isEmpty() -> showCard(Kind.ERROR, "Pair a laptop first", "Then share the file again.")
            devices.size == 1 -> chooseFolder(devices[0], uris)
            else -> {
                val sheet = Sheet(this, "Send to…", plural(uris.size, "file"))
                devices.forEach { d ->
                    val online = presence.status(d.id).state == Presence.State.ONLINE
                    sheet.add(Sheet.Row(R.drawable.ic_laptop, d.name, chip = if (online) "Online" else null) { chooseFolder(d, uris) })
                }
                sheet.show()
            }
        }
    }

    /**
     * Where to save on the laptop: its shared folders, and their subfolders.
     * Requirement: when no folder was named, always ask.
     */
    private fun chooseFolder(d: Device, uris: List<Uri>) {
        Log.i(TAG, "chooseFolder: ${uris.size} file(s) for ${d.name} at ${d.host}:${d.port}")
        worker.execute {
            val items = runCatching { Client.resolveFiles(this, uris) }.getOrElse {
                Log.w(TAG, "resolveFiles failed", it)
                ui { showCard(Kind.ERROR, "Couldn't read that file", it.message ?: "") }
                return@execute
            }
            ui { chooseDest(d, items) }
        }
    }

    /** Where to save the files on the laptop: its shared folders and their subfolders. */
    private fun chooseDest(d: Device, items: List<Client.Outgoing>) {
        run {
            run { // (kept as a block: this was inside chooseFolder before)
                val top = items.map { it.dir.substringBefore('/') }.toSet().singleOrNull()?.takeIf { it.isNotEmpty() }
                val sheet = Sheet(this, "Save on ${d.name}", (if (top != null) "Folder $top · " else "") +
                    "${plural(items.size, "file")} · ${formatSize(items.sumOf { it.size })}")
                sheet.message("Asking ${d.name} for its folders…")
                sheet.show()
                worker.execute {
                    val r = runCatching { Client.folders(DeviceStore.get(this, d.id) ?: d, myName) }
                    Log.i(TAG, "folders from ${d.name}: " + r.map { it.second.joinToString { f -> f.label } }.getOrElse { "failed: $it" })
                    ui {
                        r.onFailure { sheet.dismiss(); showCard(Kind.ERROR, "Couldn't reach ${d.name}", friendly(it, d), device = d.id) }
                        r.onSuccess { (defaultKey, roots) -> showRoots(sheet, d, items, defaultKey, roots) }
                    }
                }
            }
        }
    }

    private fun showRoots(sheet: Sheet, d: Device, items: List<Client.Outgoing>, defaultKey: String, roots: List<Client.Folder>) {
        sheet.clear()
        sheet.crumb(null, null)
        sheet.primary(null)
        if (roots.isEmpty()) sheet.message("${d.name} isn't sharing any folders. Add one in LAN Share → Settings on the laptop.")
        roots.forEach { f ->
            sheet.add(Sheet.Row(folderIcon(f.key), f.label, chip = if (f.key == defaultKey) "Default" else null,
                trail = Sheet.Trail.CHEVRON, dismiss = false) {
                browse(sheet, d, items, defaultKey, roots, f, emptyList())
            })
        }
    }

    private fun browse(sheet: Sheet, d: Device, items: List<Client.Outgoing>, defaultKey: String,
                       roots: List<Client.Folder>, folder: Client.Folder, path: List<String>) {
        sheet.clear()
        sheet.crumb((listOf(folder.label) + path).joinToString("  ›  ")) {
            if (path.isEmpty()) showRoots(sheet, d, items, defaultKey, roots)
            else browse(sheet, d, items, defaultKey, roots, folder, path.dropLast(1))
        }
        val where = (listOf(folder.label) + path).joinToString("/")
        sheet.primary("Save here · $where") { send(d, folder, path.joinToString("/"), where, items) }
        sheet.message("Loading…")
        worker.execute {
            val r = runCatching { Client.list(DeviceStore.get(this, d.id) ?: d, myName, folder.key, path.joinToString("/")) }
            Log.i(TAG, "list ${folder.label}/${path.joinToString("/")}: " + r.map { it.size.toString() + " subfolders" }.getOrElse { "failed: $it" })
            ui {
                if (!sheet.isShowing) return@ui
                sheet.clear()
                val dirs = r.getOrElse { emptyList() }
                if (r.isFailure) sheet.message("Couldn't list this folder: ${friendly(r.exceptionOrNull()!!, d)}")
                else sheet.add(Sheet.Row(R.drawable.ic_add, "New folder…", dismiss = false) {
                    newFolder(d) { name ->
                        worker.execute {
                            val made = runCatching { Client.mkdir(DeviceStore.get(this, d.id) ?: d, myName, folder.key, path.joinToString("/"), name) }
                            Log.i(TAG, "mkdir $name on ${d.name}: " + made.getOrElse { "failed: $it" })
                            ui {
                                made.onSuccess { browse(sheet, d, items, defaultKey, roots, folder, path + it) }
                                    .onFailure { showCard(Kind.ERROR, "Couldn't make that folder", friendly(it, d)) }
                            }
                        }
                    }
                })
                if (r.isSuccess && dirs.isEmpty()) sheet.message("No subfolders here.")
                dirs.forEach { name ->
                    sheet.add(Sheet.Row(R.drawable.ic_folder, name, trail = Sheet.Trail.CHEVRON, dismiss = false) {
                        browse(sheet, d, items, defaultKey, roots, folder, path + name)
                    })
                }
            }
        }
    }

    private fun send(d: Device, folder: Client.Folder, path: String, where: String, items: List<Client.Outgoing>) =
        Actions.send(this, d, folder, path, where, items) { friendly(it, d) }

    private fun newFolder(d: Device, then: (String) -> Unit) {
        val input = android.widget.EditText(this).apply { hint = "Folder name"; setSingleLine() }
        val box = android.widget.FrameLayout(this).apply { setPadding(dp(20), dp(8), dp(20), 0); addView(input) }
        android.app.AlertDialog.Builder(this)
            .setTitle("New folder on ${d.name}")
            .setView(box)
            .setPositiveButton("Create") { _, _ -> input.text.toString().trim().takeIf { it.isNotEmpty() }?.let(then) }
            .setNegativeButton("Cancel", null)
            .show()
        input.requestFocus()
    }

    private fun dp(v: Int) = (v * resources.displayMetrics.density).toInt()

    // ------------------------------------------------------------------ getting files from a laptop

    /** Browse a laptop's shared folders, tick files, and copy them to this phone's Downloads. */
    private fun getFiles(d: Device) {
        val sheet = Sheet(this, "Get from ${d.name}", "Copies to this phone's Downloads")
        if (Clip.enabled(this)) sheet.add(Sheet.Row(R.drawable.ic_document, "Paste from ${d.name}", sub = "What's copied there, onto this phone's clipboard") { pasteFrom(d) })
        sheet.message("Asking ${d.name} for its folders…")
        sheet.show()
        worker.execute {
            val r = runCatching { Client.folders(DeviceStore.get(this, d.id) ?: d, myName) }
            Log.i(TAG, "get: folders from ${d.name}: " + r.map { it.second.joinToString { f -> f.label } }.getOrElse { "failed: $it" })
            ui {
                r.onFailure { sheet.dismiss(); showCard(Kind.ERROR, "Couldn't reach ${d.name}", friendly(it, d), device = d.id) }
                r.onSuccess { (_, roots) -> getRoots(sheet, d, roots) }
            }
        }
    }

    private fun getRoots(sheet: Sheet, d: Device, roots: List<Client.Folder>) {
        sheet.clear()
        sheet.crumb(null, null)
        sheet.primary(null)
        if (Clip.enabled(this)) sheet.add(Sheet.Row(R.drawable.ic_document, "Paste from ${d.name}", sub = "What's copied there, onto this phone's clipboard") { pasteFrom(d) })
        if (roots.isEmpty()) sheet.message("${d.name} isn't sharing any folders. Add one in LAN Share → Settings on the laptop.")
        roots.forEach { f ->
            sheet.add(Sheet.Row(folderIcon(f.key), f.label, trail = Sheet.Trail.CHEVRON, dismiss = false) {
                getBrowse(sheet, d, roots, f, emptyList())
            })
        }
    }

    private fun getBrowse(sheet: Sheet, d: Device, roots: List<Client.Folder>, folder: Client.Folder, path: List<String>) {
        sheet.clear()
        sheet.crumb((listOf(folder.label) + path).joinToString("  ›  ")) {
            if (path.isEmpty()) getRoots(sheet, d, roots) else getBrowse(sheet, d, roots, folder, path.dropLast(1))
        }
        sheet.primary(null)
        sheet.message("Loading…")
        worker.execute {
            val r = runCatching { Client.listing(DeviceStore.get(this, d.id) ?: d, myName, folder.key, path.joinToString("/")) }
            Log.i(TAG, "get: list ${folder.label}/${path.joinToString("/")}: " +
                r.map { "${it.first.size} folders, ${it.second.size} files" }.getOrElse { "failed: $it" })
            ui {
                if (!sheet.isShowing) return@ui
                val (dirs, files) = r.getOrElse { emptyList<String>() to emptyList() }
                val sorted = files.sortedByDescending { it.modified } // newest first
                val picked = linkedSetOf<String>()
                fun draw() {
                    sheet.redraw {
                        if (r.isFailure) sheet.message("Couldn't list this folder: ${friendly(r.exceptionOrNull()!!, d)}")
                        else if (dirs.isEmpty() && files.isEmpty()) sheet.message("Nothing here.")
                        dirs.forEach { name ->
                            sheet.add(Sheet.Row(R.drawable.ic_folder, name, trail = Sheet.Trail.CHEVRON, dismiss = false) {
                                getBrowse(sheet, d, roots, folder, path + name)
                            })
                        }
                        sorted.forEach { f ->
                            val on = f.name in picked
                            val cached = RemoteCache.fresh(RemoteCache.fileFor(this, d, folder.key, path.joinToString("/"), f.name), f.size, f.modified)
                            sheet.add(Sheet.Row(fileIcon(f.name), f.name, sub = "${formatSize(f.size)} · ${ago(f.modified)} · tap to open",
                                chip = if (cached) "Opened" else null,
                                trail = if (on) Sheet.Trail.CHECK_ON else Sheet.Trail.CHECK_OFF, dismiss = false,
                                onTrail = { if (!picked.remove(f.name)) picked += f.name; draw() }) {
                                openRemote(d, folder, path.joinToString("/"), f) { if (sheet.isShowing) draw() }
                            })
                        }
                    }
                    val chosen = sorted.filter { it.name in picked }
                    if (chosen.isEmpty()) sheet.primary(if (files.isEmpty()) null else "Tick the files to get", enabled = false)
                    else sheet.primary("Get ${chosen.singleOrNull()?.name ?: plural(chosen.size, "file")} · ${formatSize(chosen.sumOf { it.size })}") {
                        fetch(d, folder, path.joinToString("/"), chosen.map { it.name }, chosen.sumOf { it.size }, chosen)
                    }
                }
                draw()
            }
        }
    }

    /** Ticked files: saved here. Any already opened (cached, unchanged) are copied, not downloaded again. */
    private fun fetch(d: Device, folder: Client.Folder, path: String, names: List<String>, total: Long, files: List<Client.RemoteFile> = emptyList()) {
        val cached = files.filter { RemoteCache.fresh(RemoteCache.fileFor(this, d, folder.key, path, it.name), it.size, it.modified) }
        cached.forEach { Actions.saveFromCache(this, d, RemoteCache.fileFor(this, d, folder.key, path, it.name), it.name) }
        val rest = names - cached.map { it.name }.toSet()
        if (rest.isNotEmpty()) Actions.fetch(this, d, folder, path, rest, total - cached.sumOf { it.size }) { friendly(it, d) }
    }

    /** Tap on a laptop's file: shown straight away (fetched into a temporary cache first if needed). */
    private fun openRemote(d: Device, folder: Client.Folder, path: String, f: Client.RemoteFile, after: () -> Unit = {}) {
        Actions.openRemote(this, d, folder, path, f, { friendly(it, d) }) { file ->
            after()
            if (file != null) view(file, f.name)
        }
    }

    /** Opens a cached file in whatever app shows that kind of file. */
    private fun view(file: java.io.File, name: String) {
        val i = Intent(Intent.ACTION_VIEW).setDataAndType(RemoteCache.uriFor(this, file), SharedFolders.mimeOf(name))
            .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_ACTIVITY_NEW_TASK)
        try {
            startActivity(i)
            Log.i(TAG, "open: showing $name")
        } catch (e: ActivityNotFoundException) {
            Log.i(TAG, "open: no app for $name")
            Hub.add("error", "No app here opens $name", "Tick it and choose Get to save it in Downloads instead.")
        }
    }

    /** Turns exceptions into something a person can act on. */
    private fun friendly(e: Throwable, d: Device?): String {
        val name = d?.name ?: "the laptop"
        if (d != null && presence.status(d.id).state == Presence.State.ONE_WAY) {
            return "$name can reach this phone, but not the other way: on the laptop open LAN Share → Settings → Allow through Windows Firewall."
        }
        return when (e) {
            is ConnectException, is SocketTimeoutException, is NoRouteToHostException ->
                "Is LAN Share running on $name, on the same Wi-Fi? If Windows asked about the firewall, choose Allow."
            is SSLException -> "$name didn't recognise this phone. Long-press it, choose Forget, and pair again."
            is IOException -> "The connection dropped (${e.message}). Try again."
            else -> e.message ?: e.toString()
        }
    }

    // ------------------------------------------------------------------ first start

    private var setupSheet: Sheet? = null

    /**
     * First start: everything LAN Share can use, in one place, so nobody has to find
     * Settings. Each can be skipped; all of them stay in Settings ⚙ too.
     */
    private fun showSetup() {
        Prefs.setSetupShown(this) // once, even if closed straight away
        val sheet = Sheet(this, "Set up LAN Share", "Allow what you'd like to use. You can change these any time in Settings ⚙.")
        setupSheet = sheet
        fillSetup(sheet)
        sheet.primary("Allow all") {
            val ask = runtimePermissions().filter { checkSelfPermission(it) != PackageManager.PERMISSION_GRANTED }
            Log.i(TAG, "setup: asking for ${ask.size} permission(s)")
            if (ask.isNotEmpty()) requestPermissions(ask.toTypedArray(), REQUEST_SETUP)
        }
        sheet.show()
        Log.i(TAG, "setup: shown")
    }

    /** Android's own permission questions (the rest are settings screens). */
    private fun runtimePermissions(): List<String> =
        (if (Build.VERSION.SDK_INT >= 33) listOf(Manifest.permission.POST_NOTIFICATIONS) else emptyList()) +
            SharedFolders.mediaPermissions().toList() + Manifest.permission.RECORD_AUDIO

    private fun fillSetup(sheet: Sheet) {
        sheet.clear()
        fun granted(vararg p: String) = p.all { checkSelfPermission(it) == PackageManager.PERMISSION_GRANTED }
        fun chip(on: Boolean) = if (on) "Allowed" else null
        val notify = Build.VERSION.SDK_INT < 33 || granted(Manifest.permission.POST_NOTIFICATIONS)
        sheet.add(Sheet.Row(R.drawable.ic_shield, "Notifications", sub = "Files arriving, and the Copy button for text copied on your laptop",
            chip = chip(notify), dismiss = false) { if (!notify) requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), REQUEST_SETUP) })
        val media = SharedFolders.canReadMedia(this)
        sheet.add(Sheet.Row(R.drawable.ic_image, "Photos, videos and music", sub = "So your laptop can browse and get them",
            chip = chip(media), dismiss = false) { if (!media) requestPermissions(SharedFolders.mediaPermissions(), REQUEST_SETUP) })
        val mic = granted(Manifest.permission.RECORD_AUDIO)
        sheet.add(Sheet.Row(R.drawable.ic_mic, "Microphone", sub = "To talk to the assistant (“send this to my laptop”)",
            chip = chip(mic), dismiss = false) { if (!mic) requestPermissions(arrayOf(Manifest.permission.RECORD_AUDIO), REQUEST_SETUP) })
        val overlay = android.provider.Settings.canDrawOverlays(this)
        sheet.add(Sheet.Row(R.drawable.ic_mic, "Floating bubble", sub = if (overlay) "On: tap it from any app" else "Tap, then switch on “Allow display over other apps”",
            chip = chip(overlay), dismiss = false) {
            if (!overlay) runCatching { startActivity(Intent(android.provider.Settings.ACTION_MANAGE_OVERLAY_PERMISSION, Uri.parse("package:$packageName"))) }
        })
        val battery = getSystemService(android.os.PowerManager::class.java).isIgnoringBatteryOptimizations(packageName)
        sheet.add(Sheet.Row(R.drawable.ic_shield, "Stay reachable in the background", sub = "So your laptop can send files any time",
            chip = chip(battery), dismiss = false) {
            if (!battery) runCatching {
                startActivity(Intent(android.provider.Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, Uri.parse("package:$packageName")))
            }
        })
        if (Build.VERSION.SDK_INT >= 33) {
            sheet.add(Sheet.Row(R.drawable.ic_document, "Add the “Send copied text” tile", sub = "Copy anything, pull down, tap: it's on your laptop",
                dismiss = false) { addClipTile() })
        }
    }

    /** Android 13+: one tap adds our Quick Settings tile. */
    private fun addClipTile() {
        if (Build.VERSION.SDK_INT < 33) return
        val sbm = getSystemService(android.app.StatusBarManager::class.java)
        sbm.requestAddTileService(android.content.ComponentName(this, ClipTile::class.java), "Send copied text",
            android.graphics.drawable.Icon.createWithResource(this, R.drawable.ic_stat), mainExecutor) { result ->
            Log.i(TAG, "setup: add tile → $result")
        }
    }

    override fun onRequestPermissionsResult(requestCode: Int, permissions: Array<out String>, grantResults: IntArray) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults)
        if (requestCode == REQUEST_SETUP) {
            // Back to the list: what's left (the bubble, background) is a tap each.
            setupSheet?.let { fillSetup(it); if (!it.isShowing) it.show() }
            ReceiveService.refresh(this)
        }
        if (requestCode == REQUEST_MEDIA) foldersSheet?.let { if (it.isShowing) fillSharedFolders(it) }
        if (requestCode == REQUEST_MIC) { ReceiveService.refresh(this); foldersSheet?.let { if (it.isShowing) fillSharedFolders(it) } }
    }

    private fun askNotificationPermission() {
        if (Build.VERSION.SDK_INT >= 33 && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), REQUEST_NOTIFY)
        }
    }

    // ------------------------------------------------------------------ test hooks

    /**
     * Debug builds only, for automated tests and screenshots on an emulator. These files
     * can only be written by this app itself or over USB debugging ("run-as").
     */
    private fun runTestHooks() {
        if (applicationInfo.flags and ApplicationInfo.FLAG_DEBUGGABLE == 0) return
        File(filesDir, "test_pair.txt").takeIf { it.exists() }?.let { f ->
            val text = f.readText()
            f.delete()
            pair(text.trim())
        }
        File(filesDir, "demo.txt").takeIf { it.exists() }?.let { f ->
            val mode = f.readText().trim()
            f.delete()
            Log.i(TAG, "test hook: $mode")
            val d = DeviceStore.all(this).firstOrNull() ?: Device("demo", "DESKTOP-NIKHIL", "", 0, 0)
            if (mode == "ask_clip_off" || mode == "ask_clip_on") { Clip.setAskFirst(this, mode.endsWith("on")); return@let }
            if (mode.startsWith("copy_only:")) { // as if copied in another app, then LAN Share left
                Clip.holdUntil = System.currentTimeMillis() + 3000
                getSystemService(android.content.ClipboardManager::class.java)
                    .setPrimaryClip(android.content.ClipData.newPlainText("test", mode.removePrefix("copy_only:")))
                moveTaskToBack(true)
                return@let
            }
            if (mode.startsWith("ask:")) { // a sentence with nothing shared first
                Hub.shared(emptyList())
                startActivity(Intent(this, AssistantActivity::class.java).putExtra("say", mode.removePrefix("ask:")))
                return@let
            }
            if (mode.startsWith("say:")) { // a file just shared + a sentence for the assistant
                val f = File(cacheDir, "Shared note.txt").apply { writeBytes(ByteArray(200_000) { (it % 13).toByte() }) }
                Hub.shared(listOf(Uri.fromFile(f)))
                startActivity(Intent(this, AssistantActivity::class.java).putExtra("say", mode.removePrefix("say:")))
                return@let
            }
            if (mode.startsWith("rename:")) {
                Prefs.setName(this, mode.removePrefix("rename:"))
                ReceiveService.restart(this)
                Log.i(TAG, "renamed this phone to ${mode.removePrefix("rename:")}")
                return@let
            }
            if (mode == "get_sheet") { getFiles(d); return@let }
            if (mode.startsWith("pair_phone:")) { // pair_phone:host:port:id:code
                val (_, host, port, id, code) = mode.split(":")
                pairPhone(Presence.Nearby(id, "Test phone", host, port.toInt(), "android"), code)
                return@let
            }
            when (mode) {
                "add_offline" -> { DeviceStore.put(this, Device("0".repeat(64), "Office PC", "10.0.2.99", Protocol.DEFAULT_PORT, 0)); render() }
                "sending" -> Hub.transfer("demo-out", false, d.name) { it.name = "holiday-photos.zip"; it.total = 64_000_000; it.sent = 40_300_000 }
                "receiving" -> Hub.transfer("demo-in", true, d.name) { it.name = "Quarterly report.pdf"; it.total = 9_000_000; it.sent = 2_700_000 }
                "error" -> showCard(Kind.ERROR, "Couldn't reach ${d.name}", friendly(ConnectException(), d), device = d.id)
                "folders" -> showSharedFolders(firstTime = true)
                "allow_pair" -> {
                    Identity.ensure(myName)
                    val code = PhonePairing.start()
                    ReceiveService.start(this)
                    Log.i(TAG, "phone pairing open: code $code id ${Identity.fingerprint}")
                }
                "wake_on" -> { WakeWord.setEnabled(this, true); ReceiveService.refresh(this) }
                "wake_off" -> { WakeWord.setEnabled(this, false); ReceiveService.refresh(this) }
                "wake_files" -> worker.execute {
                    // Recordings pushed by the test: each one's best "hey jarvis" score.
                    File(filesDir, "wav").listFiles()?.sortedBy { it.name }?.forEach { f ->
                        val score = runCatching { WakeWord.scoreWav(this, f.readBytes()) }.getOrElse { -1f }
                        Log.i(TAG, "wake test: ${f.name} score ${"%.3f".format(score)}")
                    }
                }
                "send_clip" -> { // copies a text here, then sends what's copied (the real read path)
                    getSystemService(android.content.ClipboardManager::class.java)
                        .setPrimaryClip(android.content.ClipData.newPlainText("test", "Hello from the phone 📱"))
                    main.postDelayed({ sendClipboard(d) }, 1500)
                }
                "paste_from" -> pasteFrom(d)
                "ask_on" -> Approvals.setEnabled(this, true)
                "ask_off" -> Approvals.setEnabled(this, false)
                "browse_off" -> Prefs.setAllowBrowse(this, false)
                "browse_on" -> Prefs.setAllowBrowse(this, true)
                "send_folder" -> {
                    // A folder "Trip" with a subfolder, as if picked with "Send a whole folder".
                    val items = listOf("Trip" to "a.jpg", "Trip" to "b.jpg", "Trip/Day 2" to "c.jpg").map { (dir, name) ->
                        val f = File(cacheDir, name).apply { writeBytes(ByteArray(100_000) { (it % 7).toByte() }) }
                        Client.Outgoing(name, f.length(), dir) { f.inputStream() }
                    }
                    chooseDest(d, items)
                }
                "bubble" -> { Bubble.setEnabled(this, true); ReceiveService.start(this) }
                "assistant" -> {
                    // "this file" = a file just shared into LAN Share; then the sentence a person would say.
                    val f = File(cacheDir, "Voice note.m4a")
                    f.outputStream().use { out -> val b = ByteArray(1 shl 16); repeat(8) { java.util.Random(it.toLong()).nextBytes(b); out.write(b) } }
                    Hub.shared(listOf(Uri.fromFile(f)))
                    startActivity(Intent(this, AssistantActivity::class.java).putExtra("say", "send this to my laptop in downloads"))
                }
                "assistant_ask" -> {
                    val f = File(cacheDir, "Voice memo 2.m4a")
                    f.outputStream().use { out -> out.write(ByteArray(300_000) { (it % 251).toByte() }) }
                    Hub.shared(listOf(Uri.fromFile(f)))
                    startActivity(Intent(this, AssistantActivity::class.java).putExtra("say", "send this to my laptop"))
                }
                "assistant_get" -> startActivity(Intent(this, AssistantActivity::class.java).putExtra("say", "get meeting notes from my laptop"))
                "send_big" -> {
                    val f = File(cacheDir, "Big video.mp4")
                    f.outputStream().use { out -> val b = ByteArray(1 shl 20); repeat(200) { out.write(b) } } // 200 MB
                    chooseFolder(d, listOf(Uri.fromFile(f)))
                }
                "send_test" -> {
                    val f = File(cacheDir, "IMG_2041.jpg")
                    f.outputStream().use { out -> val b = ByteArray(1 shl 16); repeat(32) { java.util.Random(it.toLong()).nextBytes(b); out.write(b) } }
                    chooseFolder(d, listOf(Uri.fromFile(f)))
                }
                "sheet" -> {
                    val sheet = Sheet(this, "Save on ${d.name}", "2 files · 8.4 MB")
                    sheet.crumb("Documents  ›  Work", null)
                    listOf("2025", "2026", "Invoices", "Reports").forEach { sheet.add(Sheet.Row(R.drawable.ic_folder, it, trail = Sheet.Trail.CHEVRON) {}) }
                    sheet.primary("Save here · Documents/Work")
                    sheet.show()
                }
            }
        }
    }

    companion object {
        const val TAG = "LanShare"
        private const val REQUEST_PICK = 1
        private const val REQUEST_NOTIFY = 2
        private const val REQUEST_TREE = 3
        private const val REQUEST_MEDIA = 4
        private const val REQUEST_SEND_TREE = 6
        private const val REQUEST_MIC = 7
        private const val REQUEST_SETUP = 8
        private val PAIR_TICK = Any()
    }
}
