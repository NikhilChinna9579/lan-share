package com.nikhil.lanshare

import android.app.Activity
import android.app.AlertDialog
import android.content.Intent
import android.graphics.Typeface
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.SystemClock
import android.provider.Settings
import android.view.Gravity
import android.view.View
import android.widget.Button
import android.widget.LinearLayout
import android.widget.ProgressBar
import android.widget.TextView
import com.google.mlkit.vision.barcode.common.Barcode
import com.google.mlkit.vision.codescanner.GmsBarcodeScannerOptions
import com.google.mlkit.vision.codescanner.GmsBarcodeScanning
import java.io.IOException
import java.net.ConnectException
import java.net.SocketTimeoutException
import java.util.concurrent.Executors
import javax.net.ssl.SSLException

/**
 * Spike screen: pair with the laptop by QR, then send files picked here or shared
 * from any app's Share menu. The floating bubble and voice come later.
 */
class MainActivity : Activity() {
    private val worker = Executors.newSingleThreadExecutor()
    private lateinit var status: TextView
    private lateinit var progressBar: ProgressBar
    private lateinit var pairButton: Button
    private lateinit var sendButton: Button
    private lateinit var forgetButton: Button
    private var busy = false

    private val myName: String by lazy {
        Settings.Global.getString(contentResolver, Settings.Global.DEVICE_NAME)?.takeIf { it.isNotBlank() } ?: Build.MODEL
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        buildUi()
        worker.execute { Identity.ensure(myName) } // creating the key takes a moment; do it early
        refresh()
        handleShare(intent)
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        handleShare(intent)
    }

    override fun onDestroy() {
        worker.shutdown()
        super.onDestroy()
    }

    // ---------- UI ----------

    private fun buildUi() {
        val pad = (20 * resources.displayMetrics.density).toInt()
        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(pad, pad * 2, pad, pad)
            gravity = Gravity.CENTER_HORIZONTAL
        }
        root.addView(TextView(this).apply {
            text = "LAN Share"
            textSize = 26f
            setTypeface(typeface, Typeface.BOLD)
        })
        status = TextView(this).apply {
            textSize = 16f
            gravity = Gravity.CENTER
            setPadding(0, pad, 0, pad)
        }
        root.addView(status)
        progressBar = ProgressBar(this, null, android.R.attr.progressBarStyleHorizontal).apply {
            max = 1000
            visibility = View.GONE
        }
        root.addView(progressBar, LinearLayout.LayoutParams(LinearLayout.LayoutParams.MATCH_PARENT, LinearLayout.LayoutParams.WRAP_CONTENT))
        pairButton = button("Pair with laptop") { scanQr() }
        sendButton = button("Send files to laptop") { pickFiles() }
        forgetButton = button("Forget laptop") { confirmForget() }
        listOf(pairButton, sendButton, forgetButton).forEach {
            root.addView(it, LinearLayout.LayoutParams(LinearLayout.LayoutParams.MATCH_PARENT, LinearLayout.LayoutParams.WRAP_CONTENT))
        }
        root.addView(TextView(this).apply {
            text = "Tip: in any app, tap Share → LAN Share to send that file."
            textSize = 13f
            gravity = Gravity.CENTER
            setPadding(0, pad, 0, 0)
        })
        setContentView(root)
    }

    private fun button(label: String, onClick: () -> Unit) = Button(this).apply {
        text = label
        isAllCaps = false
        setOnClickListener { onClick() }
    }

    private fun refresh(message: String? = null) {
        val laptop = Laptop.load(this)
        status.text = message ?: if (laptop == null) {
            "Not paired yet.\nOpen LAN Share on the laptop, then tap Pair."
        } else {
            "Paired with ${laptop.name}.\nKeep LAN Share open on the laptop."
        }
        pairButton.visibility = if (laptop == null) View.VISIBLE else View.GONE
        sendButton.visibility = if (laptop != null) View.VISIBLE else View.GONE
        forgetButton.visibility = if (laptop != null) View.VISIBLE else View.GONE
        listOf(pairButton, sendButton, forgetButton).forEach { it.isEnabled = !busy }
    }

    private fun setBusy(on: Boolean, message: String? = null) {
        busy = on
        if (!on) progressBar.visibility = View.GONE
        refresh(message)
    }

    // ---------- Pairing ----------

    private fun scanQr() {
        val options = GmsBarcodeScannerOptions.Builder().setBarcodeFormats(Barcode.FORMAT_QR_CODE).build()
        GmsBarcodeScanning.getClient(this, options).startScan()
            .addOnSuccessListener { code -> code.rawValue?.let { pair(it) } }
            .addOnFailureListener { setBusy(false, "Couldn't open the scanner: ${it.message}\nCheck Google Play services is up to date and you're online once.") }
    }

    private fun pair(qrText: String) {
        setBusy(true, "Pairing…")
        worker.execute {
            val result = runCatching { Laptop.pair(this, qrText, myName) }
            runOnUiThread {
                result.onSuccess { setBusy(false, "Paired with ${it.name}! Now tap Send files.") }
                    .onFailure { setBusy(false, friendly(it)) }
            }
        }
    }

    private fun confirmForget() {
        AlertDialog.Builder(this)
            .setMessage("Forget this laptop? You'll need to scan its QR code again.")
            .setPositiveButton("Forget") { _, _ -> Laptop.forget(this); refresh() }
            .setNegativeButton("Cancel", null)
            .show()
    }

    // ---------- Sending ----------

    private fun pickFiles() {
        val intent = Intent(Intent.ACTION_OPEN_DOCUMENT).apply {
            addCategory(Intent.CATEGORY_OPENABLE)
            type = "*/*"
            putExtra(Intent.EXTRA_ALLOW_MULTIPLE, true)
        }
        @Suppress("DEPRECATION")
        startActivityForResult(intent, REQUEST_PICK)
    }

    @Deprecated("Activity result API needs AndroidX; plain framework keeps the app small")
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        @Suppress("DEPRECATION")
        super.onActivityResult(requestCode, resultCode, data)
        if (requestCode != REQUEST_PICK || resultCode != RESULT_OK || data == null) return
        val uris = mutableListOf<Uri>()
        data.clipData?.let { clip -> for (i in 0 until clip.itemCount) uris += clip.getItemAt(i).uri }
        if (uris.isEmpty()) data.data?.let { uris += it }
        if (uris.isNotEmpty()) chooseFolderThenSend(uris)
    }

    /** "This file" from the Share menu (requirements: voice assistant, "this file"). */
    private fun handleShare(intent: Intent?) {
        if (intent == null) return
        val uris = mutableListOf<Uri>()
        @Suppress("DEPRECATION")
        when (intent.action) {
            Intent.ACTION_SEND -> intent.getParcelableExtra<Uri>(Intent.EXTRA_STREAM)?.let { uris += it }
            Intent.ACTION_SEND_MULTIPLE -> intent.getParcelableArrayListExtra<Uri>(Intent.EXTRA_STREAM)?.let { uris += it }
            else -> return
        }
        setIntent(Intent()) // don't resend on rotation
        if (uris.isEmpty()) {
            refresh("Nothing to send: that app shared text, not a file.")
        } else if (Laptop.load(this) == null) {
            refresh("Pair with your laptop first, then share again.")
        } else {
            chooseFolderThenSend(uris)
        }
    }

    /** Requirement: if no folder is given, always ask. */
    private fun chooseFolderThenSend(uris: List<Uri>) {
        val laptop = Laptop.load(this) ?: return
        setBusy(true, "Connecting to ${laptop.name}…")
        worker.execute {
            val result = runCatching { laptop.folders(myName) }
            runOnUiThread {
                result.onFailure { setBusy(false, friendly(it)) }
                result.onSuccess { (defaultKey, folders) ->
                    setBusy(false, "Where should it go on ${laptop.name}?")
                    val labels = folders.map { if (it.key == defaultKey) "${it.label} (default)" else it.label }
                    AlertDialog.Builder(this)
                        .setTitle("Save on ${laptop.name} in…")
                        .setItems(labels.toTypedArray()) { _, which -> send(laptop, folders[which], uris) }
                        .setNegativeButton("Cancel") { _, _ -> refresh() }
                        .show()
                }
            }
        }
    }

    private fun send(laptop: Laptop, folder: Laptop.Folder, uris: List<Uri>) {
        setBusy(true, "Preparing ${uris.size} file(s)…")
        progressBar.progress = 0
        progressBar.visibility = View.VISIBLE
        worker.execute {
            val started = SystemClock.elapsedRealtime()
            var lastUi = 0L
            val result = runCatching {
                val items = resolveFiles(this, uris)
                laptop.send(myName, folder.key, items) { sent, total ->
                    val now = SystemClock.elapsedRealtime()
                    if (now - lastUi > 150 || sent == total) {
                        lastUi = now
                        val secs = maxOf(now - started, 1) / 1000.0
                        runOnUiThread {
                            progressBar.progress = if (total == 0L) 1000 else (sent * 1000 / total).toInt()
                            status.text = "Sending to ${laptop.name} → ${folder.label}\n" +
                                "${size(sent)} of ${size(total)} · ${size((sent / secs).toLong())}/s"
                        }
                    }
                }
            }
            runOnUiThread {
                result.onSuccess { saved ->
                    val secs = (SystemClock.elapsedRealtime() - started) / 1000.0
                    val took = "%.1f".format(secs)
                    setBusy(false, "Sent ${saved.size} file(s) to ${laptop.name} → ${folder.label} in $took s\n" +
                        saved.joinToString("\n"))
                }.onFailure { setBusy(false, friendly(it)) }
            }
        }
    }

    /** Turns exceptions into something a person can act on. */
    private fun friendly(e: Throwable): String {
        val name = Laptop.load(this)?.name ?: "the laptop"
        return when (e) {
            is ConnectException, is SocketTimeoutException ->
                "Can't reach $name.\nIs LAN Share open on it, on the same Wi-Fi, and allowed through the Windows firewall?"
            is SSLException ->
                "$name refused the connection. If you re-paired or reinstalled, tap Forget laptop and pair again."
            is IOException -> "Connection to $name dropped: ${e.message}"
            else -> e.message ?: e.toString()
        }
    }

    private fun size(n: Long): String = when {
        n >= 1L shl 30 -> "%.2f GB".format(n / (1L shl 30).toDouble())
        n >= 1L shl 20 -> "%.1f MB".format(n / (1L shl 20).toDouble())
        n >= 1L shl 10 -> "%.0f KB".format(n / 1024.0)
        else -> "$n B"
    }

    companion object {
        private const val REQUEST_PICK = 1
    }
}
