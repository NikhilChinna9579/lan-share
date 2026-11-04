package com.nikhil.lanshare

import android.Manifest
import android.app.Activity
import android.app.AlertDialog
import android.content.Intent
import android.content.pm.ApplicationInfo
import android.content.pm.PackageManager
import android.graphics.drawable.GradientDrawable
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.os.SystemClock
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
import java.util.concurrent.Executors
import javax.net.ssl.SSLException

/**
 * Home screen: your paired laptops with live Online/Offline status. Tap one to send
 * files; anything shared to LAN Share from another app lands here too.
 */
class MainActivity : Activity() {
    private val worker = Executors.newSingleThreadExecutor()
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
    private lateinit var devicesLabel: View
    private lateinit var deviceList: LinearLayout
    private lateinit var pairAnother: View
    private lateinit var emptyState: View

    private var busy = false
    private var pickTarget: Device? = null
    private val hideCard = Runnable { card.visibility = View.GONE }

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
        devicesLabel = findViewById(R.id.devicesLabel)
        deviceList = findViewById(R.id.deviceList)
        pairAnother = findViewById(R.id.pairAnother)
        emptyState = findViewById(R.id.emptyState)

        val steps = findViewById<LinearLayout>(R.id.steps)
        listOf(
            "Open LAN Share on your laptop",
            "Tap Scan QR code below",
            "Scan the code on the laptop's screen",
        ).forEachIndexed { i, text ->
            val row = LayoutInflater.from(this).inflate(R.layout.item_step, steps, false)
            row.findViewById<TextView>(R.id.number).text = "${i + 1}"
            row.findViewById<TextView>(R.id.text).text = text
            steps.addView(row)
        }
        findViewById<View>(R.id.scanButton).setOnClickListener { scanQr() }
        pairAnother.setOnClickListener { scanQr() }

        presence = Presence(this, myName) { renderDevices() }
        worker.execute { runCatching { Identity.ensure(myName) } } // creating the key takes a moment
        askNotificationPermission()
        ReceiveService.start(this)
        render()
        handleShare(intent)
        runTestHooks()
    }

    override fun onStart() {
        super.onStart()
        presence.start()
        render()
    }

    override fun onStop() {
        presence.stop()
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
        val devices = DeviceStore.all(this)
        val any = devices.isNotEmpty()
        emptyState.visibility = if (any) View.GONE else View.VISIBLE
        devicesLabel.visibility = if (any) View.VISIBLE else View.GONE
        pairAnother.visibility = if (any) View.VISIBLE else View.GONE
        if (any) {
            dot(receiveDot, R.color.online)
            receiveStatus.text = "Ready to receive"
        } else {
            dot(receiveDot, R.color.offline)
            receiveStatus.text = "Not paired yet"
        }
        renderDevices()
    }

    private fun renderDevices() {
        val devices = DeviceStore.all(this)
        // Reuse rows by device id so status changes don't flicker.
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
    }

    private fun bindDevice(row: View, d: Device) {
        val s = presence.status(d.id)
        row.findViewById<TextView>(R.id.name).text = d.name
        val status = row.findViewById<TextView>(R.id.status)
        val dotView = row.findViewById<View>(R.id.statusDot)
        when (s.state) {
            Presence.State.ONLINE -> {
                status.text = "Online · ${s.host}"
                dot(dotView, R.color.online)
            }
            Presence.State.CHECKING -> {
                status.text = "Looking for it on the Wi-Fi…"
                dot(dotView, R.color.checking)
            }
            Presence.State.OFFLINE -> {
                val seen = if (s.lastSeen > 0) " · seen " +
                    DateUtils.getRelativeTimeSpanString(s.lastSeen, System.currentTimeMillis(), DateUtils.MINUTE_IN_MILLIS) else ""
                status.text = "Offline$seen"
                dot(dotView, R.color.offline)
            }
        }
        val send = { if (!busy) pickFiles(d) }
        row.setOnClickListener { send() }
        row.findViewById<Button>(R.id.send).setOnClickListener { send() }
        row.setOnLongClickListener { confirmForget(d); true }
    }

    private fun dot(v: View, color: Int) {
        (v.background.mutate() as GradientDrawable).setColor(getColor(color))
    }

    // ------------------------------------------------------------------ status card

    private enum class Kind { WORKING, DONE, ERROR }

    /** Shows the card at the top. [progress] 0–1000, or null for none. */
    private fun showCard(kind: Kind, title: String, subtitle: String, progress: Int? = null, autoHide: Boolean = false) {
        main.removeCallbacks(hideCard)
        card.visibility = View.VISIBLE
        cardTitle.text = title
        cardSubtitle.text = subtitle
        cardSubtitle.visibility = if (subtitle.isEmpty()) View.GONE else View.VISIBLE
        val (icon, bg, fg) = when (kind) {
            Kind.WORKING -> Triple(R.drawable.ic_upload, R.color.primary_container, R.color.on_primary_container)
            Kind.DONE -> Triple(R.drawable.ic_check_circle, R.color.success_container, R.color.online)
            Kind.ERROR -> Triple(R.drawable.ic_error, R.color.error_container, R.color.error)
        }
        cardIcon.setImageResource(icon)
        (cardIcon.background.mutate() as GradientDrawable).setColor(getColor(bg))
        cardIcon.imageTintList = android.content.res.ColorStateList.valueOf(getColor(fg))
        if (progress == null) {
            cardProgress.visibility = View.GONE
        } else {
            cardProgress.visibility = View.VISIBLE
            cardProgress.isIndeterminate = progress < 0
            if (progress >= 0) cardProgress.progress = progress
        }
        if (autoHide) main.postDelayed(hideCard, 6_000)
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
        busy = true
        showCard(Kind.WORKING, "Pairing…", "Talking to the laptop", progress = -1)
        worker.execute {
            val result = runCatching { Identity.ensure(myName); Client.pair(this, qrText, myName) }
            main.post {
                busy = false
                result.onSuccess {
                    showCard(Kind.DONE, "Paired with ${it.name}", "You won't need to scan again.", autoHide = true)
                    ReceiveService.start(this)
                    render()
                    presence.checkNow()
                }.onFailure { showCard(Kind.ERROR, "Pairing didn't work", friendly(it, null)) }
            }
        }
    }

    private fun confirmForget(d: Device) {
        AlertDialog.Builder(this)
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
        val target = pickTarget ?: return
        if (requestCode != REQUEST_PICK || resultCode != RESULT_OK || data == null) return
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
                    sheet.add(Sheet.Row(R.drawable.ic_laptop, d.name, if (online) "Online" else null) { chooseFolder(d, uris) })
                }
                sheet.show()
            }
        }
    }

    /** Requirement: when no folder was named, always ask. */
    private fun chooseFolder(d: Device, uris: List<Uri>) {
        busy = true
        showCard(Kind.WORKING, "Connecting to ${d.name}…", "", progress = -1)
        worker.execute {
            val result = runCatching {
                val items = Client.resolveFiles(this, uris)
                items to Client.folders(DeviceStore.get(this, d.id) ?: d, myName)
            }
            main.post {
                busy = false
                result.onFailure { showCard(Kind.ERROR, "Couldn't reach ${d.name}", friendly(it, d)) }
                result.onSuccess { (items, folders) ->
                    card.visibility = View.GONE
                    val (defaultKey, list) = folders
                    val sheet = Sheet(this, "Save on ${d.name}", "${plural(items.size, "file")} · ${formatSize(items.sumOf { it.size })}")
                    list.forEach { f ->
                        sheet.add(Sheet.Row(folderIcon(f.key), f.label, if (f.key == defaultKey) "Default" else null) {
                            send(d, f, items)
                        })
                    }
                    sheet.show()
                }
            }
        }
    }

    private fun send(d: Device, folder: Client.Folder, items: List<Client.Outgoing>) {
        busy = true
        val total = items.sumOf { it.size }
        val what = items.singleOrNull()?.name ?: plural(items.size, "file")
        showCard(Kind.WORKING, "Sending $what", "To ${d.name} · ${folder.label}", progress = 0)
        worker.execute {
            val started = SystemClock.elapsedRealtime()
            var lastUi = 0L
            val result = runCatching {
                Client.send(DeviceStore.get(this, d.id) ?: d, myName, folder.key, items) { sent, all ->
                    val now = SystemClock.elapsedRealtime()
                    if (now - lastUi > 120 || sent == all) {
                        lastUi = now
                        val speed = sent * 1000 / maxOf(now - started, 1)
                        main.post {
                            cardProgress.progress = if (all == 0L) 1000 else (sent * 1000 / all).toInt()
                            cardSubtitle.text = "${formatSize(sent)} of ${formatSize(all)} · ${formatSize(speed)}/s"
                        }
                    }
                }
            }
            main.post {
                busy = false
                val secs = (SystemClock.elapsedRealtime() - started) / 1000.0
                result.onSuccess {
                    showCard(Kind.DONE, "Sent $what to ${d.name}",
                        "In ${folder.label} · ${formatSize(total)} in ${String.format("%.1f", secs)} s", autoHide = true)
                }.onFailure { showCard(Kind.ERROR, "Couldn't send to ${d.name}", friendly(it, d)) }
            }
        }
    }

    /** Turns exceptions into something a person can act on. */
    private fun friendly(e: Throwable, d: Device?): String {
        val name = d?.name ?: "the laptop"
        return when (e) {
            is ConnectException, is SocketTimeoutException, is NoRouteToHostException ->
                "Is LAN Share open on $name, on the same Wi-Fi? If Windows asked about the firewall, choose Allow."
            is SSLException ->
                "$name didn't recognise this phone. Long-press it, choose Forget, and pair again."
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
     * Debug builds only, for automated screenshots and tests on an emulator. The files can
     * only be written by this app itself or over a USB-debugging connection ("run-as").
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
            val d = DeviceStore.all(this).firstOrNull() ?: Device("demo", "DESKTOP-NIKHIL", "", 0, 0)
            when (mode) {
                "add_offline" -> {
                    DeviceStore.put(this, Device("0".repeat(64), "Office PC", "10.0.2.99", Protocol.DEFAULT_PORT, 0))
                    render()
                }
                "sending" -> showCard(Kind.WORKING, "Sending holiday-photos.zip", "38.4 MB of 61.0 MB · 21.7 MB/s", progress = 630)
                "done" -> showCard(Kind.DONE, "Sent 3 files to ${d.name}", "In Documents · 4.2 MB in 0.4 s")
                "error" -> showCard(Kind.ERROR, "Couldn't reach ${d.name}", friendly(ConnectException(), d))
                "sheet" -> {
                    val sheet = Sheet(this, "Save on ${d.name}", "2 files · 8.4 MB")
                    listOf("downloads" to "Downloads", "documents" to "Documents", "pictures" to "Pictures", "desktop" to "Desktop")
                        .forEach { (k, l) -> sheet.add(Sheet.Row(folderIcon(k), l, if (k == "downloads") "Default" else null) {}) }
                    sheet.show()
                }
            }
        }
    }

    companion object {
        private const val REQUEST_PICK = 1
        private const val REQUEST_NOTIFY = 2
    }
}
