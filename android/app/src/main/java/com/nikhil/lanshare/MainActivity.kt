package com.nikhil.lanshare

import android.Manifest
import android.app.Activity
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
    private val myName by lazy { deviceName(this) }
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

        val steps = findViewById<LinearLayout>(R.id.steps)
        listOf("Open LAN Share on your laptop", "Click “Show pairing code”", "Tap Scan QR code below and scan it").forEachIndexed { i, text ->
            val row = LayoutInflater.from(this).inflate(R.layout.item_step, steps, false)
            row.findViewById<TextView>(R.id.number).text = "${i + 1}"
            row.findViewById<TextView>(R.id.text).text = text
            steps.addView(row)
        }
        findViewById<View>(R.id.scanButton).setOnClickListener { scanQr() }
        pairAnother.setOnClickListener { scanQr() }
        findViewById<View>(R.id.settingsButton).setOnClickListener { showSharedFolders(firstTime = false) }

        Hub.init(this)
        presence = Presence(this, myName) { renderDevices() }
        worker.execute { runCatching { Identity.ensure(myName) } } // creating the key takes a moment
        askNotificationPermission()
        ReceiveService.start(this)
        render()
        handleShare(intent)
    }

    private var hooksRan = false

    override fun onResume() {
        super.onResume()
        foldersSheet?.let { if (it.isShowing) fillSharedFolders(it) }
        if (!hooksRan) {
            hooksRan = true
            ui { runTestHooks() } // after the screen is fully up
        }
    }

    override fun onStart() {
        super.onStart()
        Hub.listen(onHub)
        presence.start()
        render()
    }

    override fun onStop() {
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

    private fun render() {
        val any = DeviceStore.all(this).isNotEmpty()
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
            when (t.state) {
                Hub.State.RUNNING -> {
                    badge(icon, if (t.inbound) R.drawable.ic_download else R.drawable.ic_upload, R.color.primary_container, R.color.on_primary_container)
                    title.text = (if (t.inbound) "Receiving " else "Sending ") + t.name.ifEmpty { "…" }
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
                    }
                }
                Hub.State.DONE -> {
                    badge(icon, R.drawable.ic_check_circle, R.color.success_container, R.color.online)
                    val what = if (t.files > 1) "${t.files} files" else t.name
                    title.text = (if (t.inbound) "Received " else "Sent ") + what
                    sub.text = "$dir ${t.device} · in ${t.folder} · ${formatSize(t.total)} in ${formatDuration(t.elapsed)} · ${formatSize(t.speed)}/s"
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
        val tap = {
            when (s.state) {
                Presence.State.ONLINE -> pickFiles(d)
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

    private fun ago(t: Long): String =
        if (System.currentTimeMillis() - t < 60_000) "just now"
        else DateUtils.getRelativeTimeSpanString(t, System.currentTimeMillis(), DateUtils.MINUTE_IN_MILLIS).toString()

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
            .setMessage("It won't be able to send to this phone, and you'll need to scan its QR code again to pair.")
            .setPositiveButton("Forget") { _, _ ->
                DeviceStore.remove(this, d.id)
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
            if (firstTime) "Choose which folders your laptops can save into. You can change this any time with ⚙."
            else "Your laptops can save into these folders, and nothing else on this phone.")
        foldersSheet = sheet
        fillSharedFolders(sheet)
        sheet.primary("Done")
        sheet.show()
    }

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
        val devices = DeviceStore.all(this)
        when {
            uris.isEmpty() -> showCard(Kind.ERROR, "Nothing to send", "That app shared text, not a file.")
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
            ui {
                val sheet = Sheet(this, "Save on ${d.name}", "${plural(items.size, "file")} · ${formatSize(items.sumOf { it.size })}")
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
                else if (dirs.isEmpty()) sheet.message("No subfolders here.")
                dirs.forEach { name ->
                    sheet.add(Sheet.Row(R.drawable.ic_folder, name, trail = Sheet.Trail.CHEVRON, dismiss = false) {
                        browse(sheet, d, items, defaultKey, roots, folder, path + name)
                    })
                }
            }
        }
    }

    private fun send(d: Device, folder: Client.Folder, path: String, where: String, items: List<Client.Outgoing>) {
        val id = UUID.randomUUID().toString()
        val total = items.sumOf { it.size }
        val what = items.singleOrNull()?.name ?: plural(items.size, "file")
        Hub.transfer(id, false, d.name) {
            it.total = total; it.name = items.first().name; it.count = items.size; it.index = 1; it.folder = where
            it.cancel = { it.cancelled = true } // until connected; then it also cuts the connection
        }
        worker.execute {
            val result = runCatching {
                Client.send(DeviceStore.get(this, d.id) ?: d, myName, folder.key, path, items,
                    onConnected = { conn ->
                        Hub.transfer(id, false, d.name) { t -> t.cancel = { t.cancelled = true; conn.abort() } }
                        if (Hub.get(id)?.cancelled == true) conn.abort()
                    }) { sent, _, index ->
                    Hub.transfer(id, false, d.name) { t -> t.sent = sent; t.index = index + 1; t.name = items[index].name }
                }
            }
            val again = { send(d, folder, path, where, items) }
            Log.i(TAG, "send to ${d.name}: " + result.map { "ok" }.getOrElse { "failed: $it" })
            result.onSuccess {
                Hub.transfer(id, false, d.name) { t -> t.state = Hub.State.DONE; t.sent = total; t.files = items.size; t.cancel = null }
                val took = Hub.get(id)?.elapsed ?: 0
                Hub.add("sent", "Sent $what to ${d.name}",
                    "In $where · ${formatSize(total)} in ${formatDuration(took)} (${formatSize(total * 1000 / maxOf(took, 1))}/s)")
            }.onFailure { e ->
                if (Hub.get(id)?.cancelled == true) {
                    Hub.transfer(id, false, d.name) { t -> t.state = Hub.State.CANCELLED; t.message = "Cancelled. Nothing was kept on ${d.name}."; t.retry = again; t.cancel = null }
                    Hub.add("info", "Cancelled sending $what to ${d.name}")
                } else {
                    val msg = friendly(e, d)
                    Hub.transfer(id, false, d.name) { t -> t.state = Hub.State.ERROR; t.message = msg; t.retry = again; t.cancel = null }
                    Hub.add("error", "Couldn't send $what to ${d.name}", msg)
                }
            }
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
            when (mode) {
                "add_offline" -> { DeviceStore.put(this, Device("0".repeat(64), "Office PC", "10.0.2.99", Protocol.DEFAULT_PORT, 0)); render() }
                "sending" -> Hub.transfer("demo-out", false, d.name) { it.name = "holiday-photos.zip"; it.total = 64_000_000; it.sent = 40_300_000 }
                "receiving" -> Hub.transfer("demo-in", true, d.name) { it.name = "Quarterly report.pdf"; it.total = 9_000_000; it.sent = 2_700_000 }
                "error" -> showCard(Kind.ERROR, "Couldn't reach ${d.name}", friendly(ConnectException(), d), device = d.id)
                "folders" -> showSharedFolders(firstTime = true)
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
    }
}
