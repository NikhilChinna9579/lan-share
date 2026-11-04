package com.nikhil.lanshare

import android.content.Context
import android.net.Uri
import android.util.Log
import org.json.JSONArray
import org.json.JSONObject
import java.io.IOException
import java.net.InetSocketAddress
import java.security.MessageDigest
import java.net.SocketException
import java.security.cert.CertificateException
import java.security.cert.X509Certificate
import java.util.concurrent.Executors
import javax.net.ssl.SSLServerSocket
import javax.net.ssl.SSLSocket
import javax.net.ssl.X509TrustManager

/**
 * The phone's receiving side: accepts connections from paired laptops only, answers
 * pings, lists shared folders, and saves incoming files (PROTOCOL.md §3–§5).
 */
class PhoneServer(private val ctx: Context, private val myName: String, private val onReceived: (Received) -> Unit) {

    class Saved(val name: String, val uri: Uri, val mime: String, val size: Long, val where: String = "")
    class Received(val from: String, val where: String, val files: List<Saved>, val tookMs: Long)

    private var server: SSLServerSocket? = null
    private val pool = Executors.newCachedThreadPool()

    /** Starts listening; returns the port. */
    fun start(): Int {
        val ss = PeerConnection.tlsContext(PairedOnly()).serverSocketFactory.createServerSocket() as SSLServerSocket
        ss.reuseAddress = true
        try {
            ss.bind(InetSocketAddress(Protocol.DEFAULT_PORT))
        } catch (e: IOException) {
            ss.bind(InetSocketAddress(0)) // default port busy: any port, announced over mDNS
        }
        ss.enabledProtocols = arrayOf("TLSv1.3")
        ss.needClientAuth = true
        server = ss
        pool.execute {
            while (!ss.isClosed) {
                val s = try {
                    ss.accept() as SSLSocket
                } catch (e: SocketException) {
                    break
                } catch (e: IOException) {
                    continue
                }
                pool.execute { runCatching { handle(s) }; runCatching { s.close() } }
            }
        }
        return ss.localPort
    }

    fun stop() {
        runCatching { server?.close() }
        pool.shutdownNow()
    }

    private fun handle(s: SSLSocket) {
        s.soTimeout = 15_000
        s.startHandshake() // fails here for anyone who isn't paired
        val peerId = Identity.fingerprintOf(s.session.peerCertificates[0] as X509Certificate)
        var peer = DeviceStore.get(ctx, peerId)
        if (peer == null && !PhonePairing.open()) return
        if (peer != null) Hub.inboundContact(peerId)
        // Where the laptop connected from is only a hint (it may be a VPN or virtual
        // adapter address we can't reach). Presence tries it, and saves it only if it works.
        if (peer != null) s.inetAddress?.hostAddress?.let { Hub.inboundFrom(peerId, it) }
        val conn = PeerConnection(s)
        val hello = conn.expect("hello")
        if (hello.optInt("v") != Protocol.VERSION) {
            conn.send(JSONObject().put("type", "error").put("code", "version"))
            return
        }
        conn.send(conn.hello(myName))
        val theirName = hello.optString("name")
        if (peer != null && theirName.isNotBlank() && theirName != peer.name) { // renamed on that device
            peer = peer.copy(name = theirName).also { DeviceStore.put(ctx, it) }
        }
        while (true) {
            val msg = try {
                conn.receive()
            } catch (e: IOException) {
                return // the laptop hung up between messages: normal end
            }
            val type = msg.optString("type")
            if (peer == null && type != "pair_request") {
                conn.send(JSONObject().put("type", "error").put("code", "not_trusted"))
                return
            }
            when (type) {
                "pair_request" -> {
                    val why = PhonePairing.check(msg.optString("code"))
                    if (why != null) {
                        conn.send(JSONObject().put("type", "pair_fail").put("reason", why))
                        Log.i("LanShare", "phone pairing refused: $why")
                    } else {
                        val name = msg.optString("name").ifBlank { "Phone" }
                        val d = Device(peerId, name, s.inetAddress?.hostAddress ?: "", Protocol.DEFAULT_PORT,
                            System.currentTimeMillis(), hello.optString("platform", "android"))
                        DeviceStore.put(ctx, d)
                        peer = d
                        conn.send(JSONObject().put("type", "pair_ok").put("name", myName))
                        PhonePairing.justPaired = name
                        Hub.add("paired", "Paired with $name", "It can send to this phone, and you to it")
                        Log.i("LanShare", "phone pairing: paired with $name")
                    }
                }
                "ping" -> {
                    // Tell the laptop whether we can connect back to it: if not, it can
                    // point the user at the Windows Firewall fix.
                    val pong = JSONObject().put("type", "pong")
                    Hub.canReach(peerId)?.let { pong.put("reach", it) }
                    conn.send(pong)
                }
                "folders_request" -> {
                    val arr = JSONArray()
                    SharedFolders.roots(ctx).forEach { arr.put(JSONObject().put("key", it.key).put("label", it.label)) }
                    conn.send(JSONObject().put("type", "folders_response").put("default", SharedFolders.DEFAULT).put("folders", arr))
                }
                "list_request" -> if (browsable()) list(conn, msg) else notAllowed(conn)
                "file_offer" -> receive(conn, peer!!, msg)
                "fetch_request" -> if (browsable()) serveFetch(conn, peer!!, msg) else notAllowed(conn)
                "search_request" -> if (browsable()) {
                    val arr = JSONArray()
                    SharedFolders.search(ctx, msg.optString("query")).forEach { h ->
                        arr.put(JSONObject().put("folder", h.folder).put("path", h.path).put("name", h.name)
                            .put("size", h.size).put("modified", h.modified))
                    }
                    conn.send(JSONObject().put("type", "search_response").put("results", arr))
                } else notAllowed(conn)
                "mkdir_request" -> {
                    val made = runCatching { SharedFolders.mkdir(ctx, msg.optString("folder"), msg.optString("path"), msg.optString("name")) }
                    made.onSuccess { conn.send(JSONObject().put("type", "mkdir_ok").put("name", it)) }
                        .onFailure { conn.send(JSONObject().put("type", "error").put("code", "bad_folder").put("message", it.message ?: "")) }
                }
                else -> conn.send(JSONObject().put("type", "error").put("code", "unknown_type"))
            }
        }
    }

    private fun browsable() = Prefs.allowBrowse(ctx)

    private fun notAllowed(conn: PeerConnection) =
        conn.send(JSONObject().put("type", "error").put("code", "not_allowed").put("message", "browsing is turned off on $myName"))

    private fun list(conn: PeerConnection, req: JSONObject) {
        val folder = req.optString("folder")
        val path = req.optString("path")
        val l = runCatching { SharedFolders.list(ctx, folder, path) }.getOrElse {
            conn.send(JSONObject().put("type", "error").put("code", "bad_folder"))
            return
        }
        val files = JSONArray()
        l.files.forEach { (n, size, mod) -> files.put(JSONObject().put("name", n).put("size", size).put("modified", mod)) }
        conn.send(JSONObject().put("type", "list_response").put("folder", folder).put("path", path)
            .put("dirs", JSONArray(l.dirs)).put("files", files))
    }

    private fun receive(conn: PeerConnection, peer: Device, offer: JSONObject) {
        val tid = offer.optString("transfer_id").ifEmpty { System.nanoTime().toString() }
        fun reject(reason: String) = conn.send(JSONObject().put("type", "file_reject").put("transfer_id", tid).put("reason", reason))

        val folder = offer.optString("folder").ifEmpty { SharedFolders.DEFAULT }
        val path = offer.optString("path")
        if (SharedFolders.roots(ctx).none { it.key == folder } || SharedFolders.cleanPath(path) == null) return reject("bad_folder")
        val arr = offer.optJSONArray("files") ?: return reject("bad_name")
        val names = mutableListOf<String>()
        val sizes = mutableListOf<Long>()
        val paths = mutableListOf<String>() // where each file goes: the offer's path plus its own subfolder
        for (i in 0 until arr.length()) {
            val f = arr.getJSONObject(i)
            names += SharedFolders.sanitize(f.optString("name")) ?: return reject("bad_name")
            sizes += f.optLong("size", -1).takeIf { it >= 0 } ?: return reject("bad_name")
            val rel = f.optString("path")
            val relParts = rel.replace('\\', '/').split('/').filter { it.isNotEmpty() && it != "." }
            if (relParts.any { SharedFolders.sanitize(it) != it }) return reject("bad_name") // no "..", no odd names
            paths += (SharedFolders.cleanPath(path)!! + relParts).joinToString("/")
        }
        if (Approvals.enabled(ctx)) {
            val label = SharedFolders.roots(ctx).first { it.key == folder }.label
            val tops = paths.map { it.removePrefix(path.trim('/')).trim('/').substringBefore('/') }.toSet()
            val what = when {
                names.size == 1 -> names[0]
                tops.size == 1 && tops.first().isNotEmpty() -> "folder ${tops.first()} (${plural(names.size, "file")})"
                else -> plural(names.size, "file")
            }
            if (!Approvals.ask(ctx, tid, peer.name, what, sizes.sum(), label)) return reject("declined")
        }
        // Big files can be continued after a dropped connection: say how much we already have.
        val keys = names.indices.map { i ->
            if (sizes[i] >= 4L shl 20) MessageDigest.getInstance("SHA-256")
                .digest("${peer.id}\u0000$folder\u0000${paths[i]}\u0000${names[i]}\u0000${sizes[i]}".toByteArray()).toHex()
            else null
        }
        val offsets = keys.mapIndexed { i, k -> k?.let { SharedFolders.partialSize(ctx, folder, it).takeIf { n -> n < sizes[i] } } ?: 0L }
        val accept = JSONObject().put("type", "file_accept").put("transfer_id", tid)
        if (offsets.any { it > 0 }) accept.put("offsets", JSONArray(offsets))
        conn.send(accept)

        val total = sizes.sum()
        val saved = try {
            var n = 0
            Transfer.receive(conn, names, sizes, {
                val i = n++
                val key = keys[i]
                if (key != null) SharedFolders.createResumable(ctx, folder, paths[i], it, key, offsets[i])
                else SharedFolders.create(ctx, folder, paths[i], it)
            }) { done, _, i ->
                Hub.transfer(tid, true, peer.name) {
                    it.name = names[i]; it.total = total; it.index = i + 1; it.count = names.size; it.sent = done
                    if (it.cancel == null) it.cancel = { it.cancelled = true; conn.abort() }
                }
            }
        } catch (e: Exception) {
            if (Hub.get(tid)?.cancelled == true) {
                keys.filterNotNull().forEach { SharedFolders.dropPartial(ctx, it) } // cancelled here: keep nothing
                Hub.transfer(tid, true, peer.name) { it.state = Hub.State.CANCELLED; it.message = "Cancelled. Nothing was kept." }
                Hub.add("info", "Cancelled receiving from ${peer.name}", "Nothing was kept")
            } else {
                Hub.transfer(tid, true, peer.name) { it.state = Hub.State.ERROR; it.message = e.message ?: "failed" }
                Hub.add("error", "Receiving from ${peer.name} failed", e.message ?: "")
            }
            throw e
        }
        val where = saved.lastOrNull()?.where ?: ""
        val out = JSONArray()
        saved.forEach { out.put(it.name) }
        conn.send(JSONObject().put("type", "transfer_done").put("transfer_id", tid).put("saved", out))
        Hub.transfer(tid, true, peer.name) { it.state = Hub.State.DONE; it.sent = total; it.files = saved.size; it.folder = where; it.cancel = null }
        val took = Hub.get(tid)?.elapsed ?: 0
        val what = saved.singleOrNull()?.name ?: plural(saved.size, "file")
        Hub.add("received", "Received $what", "From ${peer.name} · ${formatSize(total)} in ${formatDuration(took)} · in $where")
        onReceived(Received(peer.name, where, saved, took))
    }

    /**
     * A paired laptop asks for files from one of our shared folders (PROTOCOL.md §5,
     * Fetch). Only plain, visible files directly in that folder are served.
     */
    private fun serveFetch(conn: PeerConnection, peer: Device, req: JSONObject) {
        val tid = req.optString("transfer_id").ifEmpty { System.nanoTime().toString() }
        val folder = req.optString("folder")
        val path = req.optString("path")
        fun fail(code: String, msg: String) = conn.send(JSONObject().put("type", "error").put("code", code).put("message", msg))
        if (SharedFolders.roots(ctx).none { it.key == folder } || SharedFolders.cleanPath(path) == null) return fail("bad_folder", "not a shared folder")
        val arr = req.optJSONArray("files") ?: return fail("not_found", "no files asked for")
        val items = mutableListOf<Client.Outgoing>()
        for (i in 0 until arr.length()) {
            val name = arr.optString(i)
            if (SharedFolders.sanitize(name) != name || name.startsWith(".")) return fail("not_found", "bad name")
            items += SharedFolders.open(ctx, folder, path, name) ?: return fail("not_found", "$name isn't there")
        }
        if (items.isEmpty()) return fail("not_found", "no files asked for")
        val offer = JSONArray()
        items.forEach { offer.put(JSONObject().put("name", it.name).put("size", it.size)) }
        conn.send(JSONObject().put("type", "file_offer").put("transfer_id", tid).put("folder", folder).put("path", path).put("files", offer))
        if (conn.expect("file_accept", "file_reject").optString("type") != "file_accept") return
        val total = items.sumOf { it.size }
        val label = (listOf(SharedFolders.roots(ctx).first { it.key == folder }.label) + SharedFolders.cleanPath(path)!!).joinToString("/")
        Hub.transfer(tid, false, peer.name) {
            it.name = items[0].name; it.total = total; it.count = items.size; it.folder = label
            it.cancel = { it.cancelled = true; conn.abort() }
        }
        try {
            var lastUi = 0L
            Transfer.stream(conn, items) { sent, _, i ->
                val now = System.currentTimeMillis()
                if (now - lastUi > 150 || sent == total) {
                    lastUi = now
                    Hub.transfer(tid, false, peer.name) { it.sent = sent; it.name = items[i].name; it.index = i + 1 }
                }
            }
            conn.expect("transfer_done")
        } catch (e: Exception) {
            if (Hub.get(tid)?.cancelled == true) {
                Hub.transfer(tid, false, peer.name) { it.state = Hub.State.CANCELLED; it.message = "Cancelled." }
            } else {
                Hub.transfer(tid, false, peer.name) { it.state = Hub.State.ERROR; it.message = e.message ?: "failed" }
                Hub.add("error", "${peer.name} couldn't get files", e.message ?: "")
            }
            throw e
        }
        Hub.transfer(tid, false, peer.name) { it.state = Hub.State.DONE; it.sent = total; it.files = items.size; it.cancel = null }
        val took = Hub.get(tid)?.elapsed ?: 0
        val what = items.singleOrNull()?.name ?: plural(items.size, "file")
        Hub.add("sent", "${peer.name} got $what", "From $label · ${formatSize(total)} in ${formatDuration(took)}")
        android.util.Log.i("LanShare", "served ${items.size} files to ${peer.name}")
    }

    /** Lets in only laptops we have paired with; everyone else fails the TLS handshake. */
    private inner class PairedOnly : X509TrustManager {
        override fun checkClientTrusted(chain: Array<out X509Certificate>?, authType: String?) {
            val cert = chain?.firstOrNull() ?: throw CertificateException("no certificate")
            // Strangers get in only while this phone shows a pairing code (and then may only pair).
            if (DeviceStore.get(ctx, Identity.fingerprintOf(cert)) == null && !PhonePairing.open()) throw CertificateException("not paired")
        }

        override fun checkServerTrusted(chain: Array<out X509Certificate>?, authType: String?) =
            throw CertificateException("server mode only")

        override fun getAcceptedIssuers(): Array<X509Certificate> = emptyArray()
    }
}
