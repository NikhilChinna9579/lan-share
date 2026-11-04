package com.nikhil.lanshare

import org.json.JSONObject
import java.io.IOException
import java.security.MessageDigest

/**
 * Moving the bytes of an accepted file_offer (PROTOCOL.md §5), in either direction.
 * Sending to a laptop and serving a laptop's fetch both use [stream]; receiving files
 * and fetching files both use [receive].
 */
object Transfer {

    /** Writes each file's bytes and its file_end. [progress] gets (sent, total, index). */
    fun stream(conn: PeerConnection, items: List<Client.Outgoing>, progress: (Long, Long, Int) -> Unit) {
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
                    progress(sent, total, index)
                }
            }
            conn.send(JSONObject().put("type", "file_end").put("index", index).put("sha256", digest.digest().toHex()))
        }
    }

    /**
     * Reads the files of an offer. [create] makes the destination for each name; a file
     * is kept only when its checksum matches, otherwise it is discarded and this throws.
     * [progress] gets (bytes so far, total, index), at most ~7 times a second.
     */
    fun receive(conn: PeerConnection, names: List<String>, sizes: List<Long>,
                create: (String) -> SharedFolders.Incoming,
                progress: (Long, Long, Int) -> Unit): List<PhoneServer.Saved> {
        val total = sizes.sum()
        var done = 0L
        val saved = mutableListOf<PhoneServer.Saved>()
        val buf = ByteArray(64 * 1024)
        for (i in names.indices) {
            progress(done, total, i)
            val inc = create(names[i])
            var ok = false
            try {
                val digest = MessageDigest.getInstance("SHA-256")
                var remaining = sizes[i]
                var lastUi = 0L
                while (remaining > 0) {
                    val n = conn.input.read(buf, 0, minOf(buf.size.toLong(), remaining).toInt())
                    if (n < 0) throw IOException("the other device disconnected")
                    digest.update(buf, 0, n)
                    inc.out.write(buf, 0, n)
                    remaining -= n
                    done += n
                    val now = System.currentTimeMillis()
                    if (now - lastUi > 150) {
                        lastUi = now
                        progress(done, total, i)
                    }
                }
                val end = conn.expect("file_end")
                if (end.optInt("index") != i || end.optString("sha256") != digest.digest().toHex()) {
                    conn.send(JSONObject().put("type", "error").put("code", "checksum"))
                    throw IOException("the file got damaged on the way (checksum mismatch)")
                }
                saved += PhoneServer.Saved(inc.finish(), inc.uri, inc.mime, sizes[i], inc.savedIn)
                ok = true
            } finally {
                if (!ok) inc.discard() // never leave half-written files behind
            }
        }
        progress(total, total, names.size - 1)
        return saved
    }
}
