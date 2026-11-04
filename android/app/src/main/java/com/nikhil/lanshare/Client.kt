package com.nikhil.lanshare

import android.content.Context
import android.net.Uri
import android.provider.OpenableColumns
import org.json.JSONArray
import org.json.JSONObject
import java.io.File
import java.io.InputStream
import java.security.MessageDigest
import java.util.UUID

/** Talking to a paired laptop as a client: pair, ping, list folders, send files. */
object Client {

    /** A folder offered by the receiver: [key] goes on the wire, [label] is shown. */
    data class Folder(val key: String, val label: String)

    /** A file ready to send: display name, exact size, and how to read it. */
    class Outgoing(val name: String, val size: Long, val open: () -> InputStream)

    /**
     * Pairs using the scanned QR text (PROTOCOL.md §5, Pairing). Blocking.
     * Saves and returns the laptop, or throws with a message a person can read.
     */
    fun pair(ctx: Context, qrText: String, myName: String): Device {
        val qr = runCatching { JSONObject(qrText) }.getOrNull()
        if (qr == null || qr.optInt("v") != Protocol.VERSION || !qr.has("id") || !qr.has("code")) {
            throw IllegalArgumentException("That isn't a LAN Share pairing code.")
        }
        val device = Device(qr.getString("id"), qr.optString("name", "Laptop"), qr.getString("host"),
            qr.getInt("port"), System.currentTimeMillis())
        PeerConnection.open(device.host, device.port, device.id, myName).use { conn ->
            conn.send(JSONObject().put("type", "pair_request").put("code", qr.getString("code")).put("name", myName))
            val reply = conn.expect("pair_ok", "pair_fail")
            if (reply.getString("type") == "pair_fail") {
                throw IllegalStateException(
                    when (reply.optString("reason")) {
                        "expired" -> "That code expired. Press P in LAN Share on the laptop for a new one."
                        "not_pairing" -> "The laptop isn't pairing right now. Press P in LAN Share on the laptop."
                        else -> "The laptop rejected the code. Press P on the laptop for a new one."
                    }
                )
            }
            val saved = device.copy(name = reply.optString("name", device.name))
            DeviceStore.put(ctx, saved)
            return saved
        }
    }

    /** True if the device answers a ping at [host]:[port] with the right key. */
    fun ping(device: Device, host: String, port: Int, myName: String): Boolean = runCatching {
        PeerConnection.open(host, port, device.id, myName, connectTimeoutMs = 1_500).use { conn ->
            conn.send(JSONObject().put("type", "ping"))
            conn.expect("pong")
        }
    }.isSuccess

    /** Asks which folders the laptop offers. Returns (default key, folders). */
    fun folders(device: Device, myName: String): Pair<String, List<Folder>> =
        PeerConnection.open(device.host, device.port, device.id, myName).use { conn ->
            conn.send(JSONObject().put("type", "folders_request"))
            val resp = conn.expect("folders_response")
            val arr = resp.getJSONArray("folders")
            val list = (0 until arr.length()).map {
                val f = arr.getJSONObject(it)
                Folder(f.getString("key"), f.getString("label"))
            }
            resp.optString("default", "downloads") to list
        }

    /**
     * Sends [items] into [folderKey] (PROTOCOL.md §5, Transfer). Blocking.
     * [progress] gets (bytes sent so far, total). Returns the names the laptop saved.
     */
    fun send(device: Device, myName: String, folderKey: String, path: String, items: List<Outgoing>, progress: (Long, Long) -> Unit): List<String> =
        PeerConnection.open(device.host, device.port, device.id, myName).use { conn ->
            val files = JSONArray()
            items.forEach { files.put(JSONObject().put("name", it.name).put("size", it.size)) }
            conn.send(JSONObject().put("type", "file_offer").put("transfer_id", UUID.randomUUID().toString())
                .put("folder", folderKey).put("path", path).put("files", files))
            val answer = conn.expect("file_accept", "file_reject")
            if (answer.getString("type") == "file_reject") {
                throw IllegalStateException("${device.name} refused the files (${answer.optString("reason")}).")
            }
            val total = items.sumOf { it.size }
            var sent = 0L
            val buf = ByteArray(64 * 1024)
            items.forEachIndexed { index, item ->
                val digest = MessageDigest.getInstance("SHA-256")
                var remaining = item.size
                item.open().use { input ->
                    while (remaining > 0) {
                        val n = input.read(buf, 0, minOf(buf.size.toLong(), remaining).toInt())
                        if (n < 0) throw IllegalStateException("${item.name} got shorter while sending.")
                        digest.update(buf, 0, n)
                        conn.output.write(buf, 0, n)
                        remaining -= n
                        sent += n
                        progress(sent, total)
                    }
                }
                conn.send(JSONObject().put("type", "file_end").put("index", index).put("sha256", digest.digest().toHex()))
            }
            val saved = conn.expect("transfer_done").getJSONArray("saved")
            (0 until saved.length()).map { saved.getString(it) }
        }

    /** Turns picked or shared content URIs into files with known names and sizes. */
    fun resolveFiles(ctx: Context, uris: List<Uri>): List<Outgoing> = uris.map { uri ->
        var name: String? = null
        var size = -1L
        runCatching {
            ctx.contentResolver.query(uri, arrayOf(OpenableColumns.DISPLAY_NAME, OpenableColumns.SIZE), null, null, null)?.use { c ->
                if (c.moveToFirst()) {
                    val ni = c.getColumnIndex(OpenableColumns.DISPLAY_NAME)
                    val si = c.getColumnIndex(OpenableColumns.SIZE)
                    if (ni >= 0 && !c.isNull(ni)) name = c.getString(ni)
                    if (si >= 0 && !c.isNull(si)) size = c.getLong(si)
                }
            }
        }
        val fileName = name ?: uri.lastPathSegment?.substringAfterLast('/') ?: "file"
        if (size >= 0) {
            Outgoing(fileName, size) { ctx.contentResolver.openInputStream(uri) ?: error("Can't open $fileName") }
        } else {
            // Size unknown (some apps don't say): copy to the cache first so we know it exactly.
            val tmp = File.createTempFile("send", null, ctx.cacheDir)
            ctx.contentResolver.openInputStream(uri)?.use { input -> tmp.outputStream().use { input.copyTo(it) } }
                ?: error("Can't open $fileName")
            tmp.deleteOnExit()
            Outgoing(fileName, tmp.length()) { tmp.inputStream() }
        }
    }
}

fun ByteArray.toHex(): String = joinToString("") { "%02x".format(it) }

/** "2.4 MB" */
fun formatSize(n: Long): String = when {
    n >= 1L shl 30 -> String.format("%.2f GB", n / (1L shl 30).toDouble())
    n >= 1L shl 20 -> String.format("%.1f MB", n / (1L shl 20).toDouble())
    n >= 1L shl 10 -> String.format("%.0f KB", n / 1024.0)
    else -> "$n B"
}

fun plural(n: Int, one: String, many: String = one + "s") = if (n == 1) "1 $one" else "$n $many"
