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

/** The laptop this phone is paired with, saved on the phone. */
data class Laptop(val id: String, val name: String, val host: String, val port: Int) {

    companion object {
        private const val PREFS = "pairing"

        fun load(ctx: Context): Laptop? {
            val p = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
            val id = p.getString("id", null) ?: return null
            return Laptop(id, p.getString("name", "Laptop")!!, p.getString("host", "")!!, p.getInt("port", Protocol.DEFAULT_PORT))
        }

        fun forget(ctx: Context) = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit().clear().apply()

        /**
         * Pairs using the scanned QR text (PROTOCOL.md §5, Pairing). Blocking: call off the UI thread.
         * Returns the saved laptop, or throws with a message the user can read.
         */
        fun pair(ctx: Context, qrText: String, myName: String): Laptop {
            val qr = try {
                JSONObject(qrText)
            } catch (e: Exception) {
                throw IllegalArgumentException("That QR code isn't a LAN Share pairing code.")
            }
            if (qr.optInt("v") != Protocol.VERSION || !qr.has("id") || !qr.has("code")) {
                throw IllegalArgumentException("That QR code isn't a LAN Share pairing code.")
            }
            val laptop = Laptop(qr.getString("id"), qr.optString("name", "Laptop"), qr.getString("host"), qr.getInt("port"))
            PeerConnection.open(laptop.host, laptop.port, laptop.id, myName).use { conn ->
                conn.send(JSONObject().put("type", "pair_request").put("code", qr.getString("code")).put("name", myName))
                val reply = conn.expect("pair_ok", "pair_fail")
                if (reply.getString("type") == "pair_fail") {
                    throw IllegalStateException(
                        when (reply.optString("reason")) {
                            "expired" -> "The code expired. Press P on the laptop for a new one."
                            "not_pairing" -> "The laptop isn't in pairing mode. Press P on the laptop."
                            else -> "The laptop rejected the code. Press P on the laptop for a new one."
                        }
                    )
                }
                val saved = laptop.copy(name = reply.optString("name", laptop.name))
                ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit()
                    .putString("id", saved.id).putString("name", saved.name)
                    .putString("host", saved.host).putInt("port", saved.port).apply()
                return saved
            }
        }
    }

    /** A folder offered by the laptop: [key] goes on the wire, [label] is shown. */
    data class Folder(val key: String, val label: String)

    fun open(myName: String) = PeerConnection.open(host, port, id, myName)

    /** Asks which folders the laptop offers. Returns (default key, folders). */
    fun folders(myName: String): Pair<String, List<Folder>> = open(myName).use { conn ->
        conn.send(JSONObject().put("type", "folders_request"))
        val resp = conn.expect("folders_response")
        val arr = resp.getJSONArray("folders")
        val list = (0 until arr.length()).map {
            val f = arr.getJSONObject(it)
            Folder(f.getString("key"), f.getString("label"))
        }
        resp.optString("default", "downloads") to list
    }

    /** A file ready to send: its display name, exact size, and how to read it. */
    class Outgoing(val name: String, val size: Long, val open: () -> InputStream)

    /**
     * Sends [items] into [folderKey] (PROTOCOL.md §5, Transfer). Blocking.
     * [progress] gets (bytes sent so far, total bytes). Returns the names the laptop saved.
     */
    fun send(myName: String, folderKey: String, items: List<Outgoing>, progress: (Long, Long) -> Unit): List<String> =
        open(myName).use { conn ->
            val files = JSONArray()
            items.forEach { files.put(JSONObject().put("name", it.name).put("size", it.size)) }
            val transferId = UUID.randomUUID().toString()
            conn.send(JSONObject().put("type", "file_offer").put("transfer_id", transferId)
                .put("folder", folderKey).put("files", files))
            val answer = conn.expect("file_accept", "file_reject")
            if (answer.getString("type") == "file_reject") {
                throw IllegalStateException("The laptop refused the files (${answer.optString("reason")}).")
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
                val sha = digest.digest().joinToString("") { "%02x".format(it) }
                conn.send(JSONObject().put("type", "file_end").put("index", index).put("sha256", sha))
            }
            val done = conn.expect("transfer_done")
            val saved = done.getJSONArray("saved")
            (0 until saved.length()).map { saved.getString(it) }
        }
}

/** Turns picked or shared content URIs into [Laptop.Outgoing] files with known sizes. */
fun resolveFiles(ctx: Context, uris: List<Uri>): List<Laptop.Outgoing> = uris.map { uri ->
    var name: String? = null
    var size = -1L
    ctx.contentResolver.query(uri, arrayOf(OpenableColumns.DISPLAY_NAME, OpenableColumns.SIZE), null, null, null)?.use { c ->
        if (c.moveToFirst()) {
            val ni = c.getColumnIndex(OpenableColumns.DISPLAY_NAME)
            val si = c.getColumnIndex(OpenableColumns.SIZE)
            if (ni >= 0 && !c.isNull(ni)) name = c.getString(ni)
            if (si >= 0 && !c.isNull(si)) size = c.getLong(si)
        }
    }
    val fileName = name ?: uri.lastPathSegment?.substringAfterLast('/') ?: "file"
    if (size >= 0) {
        Laptop.Outgoing(fileName, size) { ctx.contentResolver.openInputStream(uri) ?: error("Can't open $fileName") }
    } else {
        // Size unknown (some apps don't say): copy to the cache first so we know it exactly.
        val tmp = File.createTempFile("send", null, ctx.cacheDir)
        ctx.contentResolver.openInputStream(uri)?.use { input -> tmp.outputStream().use { input.copyTo(it) } }
            ?: error("Can't open $fileName")
        tmp.deleteOnExit()
        Laptop.Outgoing(fileName, tmp.length()) { tmp.inputStream() }
    }
}
