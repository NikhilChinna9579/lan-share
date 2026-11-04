package com.nikhil.lanshare

import android.content.Context
import android.net.Uri
import android.provider.DocumentsContract
import android.provider.OpenableColumns
import org.json.JSONArray
import org.json.JSONObject
import java.io.File
import java.io.InputStream
import java.util.UUID

/** Talking to a paired laptop as a client: pair, ping, list folders, send files. */
object Client {

    /** A folder offered by the receiver: [key] goes on the wire, [label] is shown. */
    data class Folder(val key: String, val label: String)

    /** A file ready to send: display name, exact size, and how to read it. */
    class Outgoing(val name: String, val size: Long, val dir: String = "", val open: () -> InputStream)

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

    /** Pairs with another phone found on the Wi-Fi, using the 6-digit code it shows. */
    fun pairPhone(ctx: Context, near: Presence.Nearby, code: String, myName: String): Device =
        PeerConnection.open(near.host, near.port, near.id, myName).use { conn ->
            conn.send(JSONObject().put("type", "pair_request").put("code", code.trim()).put("name", myName))
            val reply = conn.expect("pair_ok", "pair_fail")
            if (reply.getString("type") == "pair_fail") {
                throw IllegalStateException(when (reply.optString("reason")) {
                    "expired" -> "That code expired. On ${near.name}, start pairing again."
                    "not_pairing" -> "${near.name} isn't pairing right now. On it: Pair → Let another phone pair."
                    else -> "Wrong code. Check the 6 digits on ${near.name}."
                })
            }
            val d = Device(near.id, reply.optString("name", near.name), near.host, near.port, System.currentTimeMillis(),
                conn.peerPlatform.ifEmpty { near.platform })
            DeviceStore.put(ctx, d)
            d
        }

    /** True if the device answers a ping at [host]:[port] with the right key. */
    fun ping(device: Device, host: String, port: Int, myName: String): Boolean = pingName(device, host, port, myName) != null

    /** Like [ping]; returns the name the device gives now (it may have been renamed), or null. */
    fun pingName(device: Device, host: String, port: Int, myName: String): String? = runCatching {
        PeerConnection.open(host, port, device.id, myName, connectTimeoutMs = 1_500).use { conn ->
            conn.send(JSONObject().put("type", "ping"))
            conn.expect("pong")
            conn.peerName
        }
    }.getOrNull()

    /** Asks which folders the laptop offers. Returns (default key, folders). */
    fun list(device: Device, myName: String, folderKey: String, path: String): List<String> =
        PeerConnection.open(device.host, device.port, device.id, myName).use { conn ->
            conn.send(JSONObject().put("type", "list_request").put("folder", folderKey).put("path", path))
            val dirs = conn.expect("list_response").optJSONArray("dirs") ?: JSONArray()
            (0 until dirs.length()).map { dirs.getString(it) }
        }

    /** One file in a laptop's folder listing. */
    data class RemoteFile(val name: String, val size: Long, val modified: Long)

    /** Subfolders and files at [path] in one of the laptop's shared folders. */
    fun listing(device: Device, myName: String, folderKey: String, path: String): Pair<List<String>, List<RemoteFile>> =
        PeerConnection.open(device.host, device.port, device.id, myName).use { conn ->
            conn.send(JSONObject().put("type", "list_request").put("folder", folderKey).put("path", path))
            val r = conn.expect("list_response")
            val dirs = r.optJSONArray("dirs") ?: JSONArray()
            val files = r.optJSONArray("files") ?: JSONArray()
            (0 until dirs.length()).map { dirs.getString(it) } to (0 until files.length()).map {
                val f = files.getJSONObject(it)
                RemoteFile(f.getString("name"), f.optLong("size"), f.optLong("modified"))
            }
        }

    /**
     * Gets files from the laptop's shared folder (PROTOCOL.md §5, Fetch) into this
     * phone's Downloads. Blocking. [progress] gets (bytes so far, total, index).
     */
    fun fetch(ctx: Context, device: Device, myName: String, folderKey: String, path: String, names: List<String>,
              onConnected: (PeerConnection) -> Unit = {}, progress: (Long, Long, Int) -> Unit): List<PhoneServer.Saved> =
        PeerConnection.open(device.host, device.port, device.id, myName).use { conn ->
            onConnected(conn)
            val tid = UUID.randomUUID().toString()
            conn.send(JSONObject().put("type", "fetch_request").put("transfer_id", tid)
                .put("folder", folderKey).put("path", path).put("files", JSONArray(names)))
            val offer = conn.receive()
            when (offer.optString("type")) {
                "file_offer" -> {}
                "error" -> throw IllegalStateException(
                    if (offer.optString("code") == "not_found") "That file is no longer on ${device.name}." else "${device.name} said: ${offer.optString("message", offer.optString("code"))}")
                else -> throw ProtocolException("unexpected reply: ${offer.optString("type")}")
            }
            val arr = offer.getJSONArray("files")
            val clean = (0 until arr.length()).map { SharedFolders.sanitize(arr.getJSONObject(it).optString("name")) ?: throw ProtocolException("bad file name") }
            val sizes = (0 until arr.length()).map { arr.getJSONObject(it).optLong("size", -1).also { s -> if (s < 0) throw ProtocolException("bad size") } }
            conn.send(JSONObject().put("type", "file_accept").put("transfer_id", tid))
            val saved = Transfer.receive(conn, clean, sizes, { SharedFolders.create(ctx, SharedFolders.DEFAULT, "", it) }, progress)
            val out = JSONArray()
            saved.forEach { out.put(it.name) }
            conn.send(JSONObject().put("type", "transfer_done").put("transfer_id", tid).put("saved", out))
            saved
        }

    /** Puts text on the laptop's clipboard. */
    fun pushClip(device: Device, myName: String, text: String) =
        PeerConnection.open(device.host, device.port, device.id, myName).use { conn ->
            conn.send(JSONObject().put("type", "clip_push").put("text", text))
            val r = conn.receive()
            if (r.optString("type") != "clip_ok") throw IllegalStateException(
                if (r.optString("code") == "not_allowed") "${device.name} has clipboard sharing turned off." else "${device.name} didn't take it.")
        }

    /** The laptop's clipboard text ("paste from laptop"). */
    fun requestClip(device: Device, myName: String): String =
        PeerConnection.open(device.host, device.port, device.id, myName).use { conn ->
            conn.send(JSONObject().put("type", "clip_request"))
            val r = conn.expect("clip_response")
            if (!r.optBoolean("available")) throw IllegalStateException(r.optString("reason").replaceFirstChar { it.uppercase() } + ".")
            r.optString("text")
        }

    class Hit(val folder: String, val path: String, val name: String, val size: Long, val modified: Long)

    /** Finds files by name in all of the laptop's shared folders (and subfolders). */
    fun search(device: Device, myName: String, query: String): List<Hit> =
        PeerConnection.open(device.host, device.port, device.id, myName).use { conn ->
            conn.send(JSONObject().put("type", "search_request").put("query", query))
            val r = conn.receive()
            if (r.optString("type") != "search_response") {
                throw IllegalStateException(if (r.optString("code") == "not_allowed") "${device.name} doesn't allow browsing its files." else "Search didn't work.")
            }
            val arr = r.optJSONArray("results") ?: JSONArray()
            (0 until arr.length()).map {
                val o = arr.getJSONObject(it)
                Hit(o.optString("folder"), o.optString("path"), o.optString("name"), o.optLong("size"), o.optLong("modified"))
            }
        }

    /** Makes a subfolder on the laptop (the "New folder" button); returns its name. */
    fun mkdir(device: Device, myName: String, folderKey: String, path: String, name: String): String =
        PeerConnection.open(device.host, device.port, device.id, myName).use { conn ->
            conn.send(JSONObject().put("type", "mkdir_request").put("folder", folderKey).put("path", path).put("name", name))
            val r = conn.receive()
            if (r.optString("type") != "mkdir_ok") throw IllegalStateException(r.optString("message").ifEmpty { "Couldn't make that folder." })
            r.optString("name", name)
        }

    /** Every file inside a folder picked with the folder picker, keeping subfolders. */
    fun resolveTree(ctx: Context, tree: Uri): List<Outgoing> {
        val out = mutableListOf<Outgoing>()
        val cr = ctx.contentResolver
        val cols = arrayOf(DocumentsContract.Document.COLUMN_DOCUMENT_ID, DocumentsContract.Document.COLUMN_DISPLAY_NAME,
            DocumentsContract.Document.COLUMN_MIME_TYPE, DocumentsContract.Document.COLUMN_SIZE)
        fun walk(docId: String, rel: String) {
            cr.query(DocumentsContract.buildChildDocumentsUriUsingTree(tree, docId), cols, null, null, null)?.use { c ->
                while (c.moveToNext() && out.size < 10_000) {
                    val id = c.getString(0)
                    val name = c.getString(1) ?: continue
                    if (name.startsWith(".")) continue
                    if (c.getString(2) == DocumentsContract.Document.MIME_TYPE_DIR) walk(id, "$rel/$name")
                    else {
                        val uri = DocumentsContract.buildDocumentUriUsingTree(tree, id)
                        out += Outgoing(name, if (c.isNull(3)) 0 else c.getLong(3), rel) {
                            cr.openInputStream(uri) ?: error("Can't open $name")
                        }
                    }
                }
            }
        }
        val rootId = DocumentsContract.getTreeDocumentId(tree)
        val top = cr.query(DocumentsContract.buildDocumentUriUsingTree(tree, rootId), arrayOf(DocumentsContract.Document.COLUMN_DISPLAY_NAME), null, null, null)
            ?.use { if (it.moveToFirst()) it.getString(0) else null } ?: "Folder"
        walk(rootId, top)
        return out
    }

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
    fun send(device: Device, myName: String, folderKey: String, path: String, items: List<Outgoing>,
             onConnected: (PeerConnection) -> Unit = {}, progress: (Long, Long, Int) -> Unit): List<String> =
        PeerConnection.open(device.host, device.port, device.id, myName).use { conn ->
            onConnected(conn)
            val files = JSONArray()
            items.forEach { o ->
                val f = JSONObject().put("name", o.name).put("size", o.size)
                if (o.dir.isNotEmpty()) f.put("path", o.dir) // part of a whole folder
                files.put(f)
            }
            conn.send(JSONObject().put("type", "file_offer").put("transfer_id", UUID.randomUUID().toString())
                .put("folder", folderKey).put("path", path).put("files", files))
            conn.readTimeout(65_000) // the laptop may be asking its user first
            val answer = conn.expect("file_accept", "file_reject")
            conn.readTimeout(15_000)
            if (answer.getString("type") == "file_reject") {
                throw IllegalStateException(if (answer.optString("reason") == "declined") "${device.name} declined the files."
                    else "${device.name} refused the files (${answer.optString("reason")}).")
            }
            // The laptop may already have part of a big file from an interrupted try.
            val off = answer.optJSONArray("offsets")
            val offsets = LongArray(items.size) { off?.optLong(it, 0) ?: 0 }
            items.forEachIndexed { i, it -> if (offsets[i] > 0) android.util.Log.i("LanShare", "continuing ${it.name} from ${formatSize(offsets[i])}") }
            Transfer.stream(conn, items, offsets, progress)
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

/** "under 0.1 s", "2.4 s", "38 s", "2 min 5 s" */
fun formatDuration(ms: Long): String = when {
    ms < 100 -> "under 0.1 s"
    ms < 10_000 -> String.format("%.1f s", ms / 1000.0)
    ms < 60_000 -> "${ms / 1000} s"
    ms < 3_600_000 -> "${ms / 60_000} min ${(ms / 1000) % 60} s"
    else -> "${ms / 3_600_000} h ${(ms / 60_000) % 60} min"
}

/** "2.4 MB" */
fun formatSize(n: Long): String = when {
    n >= 1L shl 30 -> String.format("%.2f GB", n / (1L shl 30).toDouble())
    n >= 1L shl 20 -> String.format("%.1f MB", n / (1L shl 20).toDouble())
    n >= 1L shl 10 -> String.format("%.0f KB", n / 1024.0)
    else -> "$n B"
}

fun plural(n: Int, one: String, many: String = one + "s") = if (n == 1) "1 $one" else "$n $many"
