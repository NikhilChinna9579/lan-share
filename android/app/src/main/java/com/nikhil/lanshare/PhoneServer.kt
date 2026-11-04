package com.nikhil.lanshare

import android.content.Context
import android.net.Uri
import org.json.JSONArray
import org.json.JSONObject
import java.io.IOException
import java.net.InetSocketAddress
import java.net.SocketException
import java.security.MessageDigest
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

    class Saved(val name: String, val uri: Uri, val mime: String, val size: Long)
    class Received(val from: String, val where: String, val files: List<Saved>)

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
        val peer = DeviceStore.get(ctx, peerId) ?: return
        Hub.inboundContact(peerId)
        // A laptop that reaches us is at that IP right now: a free hint for its address.
        s.inetAddress?.hostAddress?.let { DeviceStore.updateAddress(ctx, peerId, it, peer.port) }
        val conn = PeerConnection(s)
        val hello = conn.expect("hello")
        if (hello.optInt("v") != Protocol.VERSION) {
            conn.send(JSONObject().put("type", "error").put("code", "version"))
            return
        }
        conn.send(conn.hello(myName))
        while (true) {
            val msg = try {
                conn.receive()
            } catch (e: IOException) {
                return // the laptop hung up between messages: normal end
            }
            when (msg.optString("type")) {
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
                "list_request" -> list(conn, msg)
                "file_offer" -> receive(conn, peerId, peer, msg)
                else -> conn.send(JSONObject().put("type", "error").put("code", "unknown_type"))
            }
        }
    }

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

    private fun receive(conn: PeerConnection, peerId: String, peer: Device, offer: JSONObject) {
        val tid = offer.optString("transfer_id").ifEmpty { System.nanoTime().toString() }
        fun reject(reason: String) = conn.send(JSONObject().put("type", "file_reject").put("transfer_id", tid).put("reason", reason))

        val folder = offer.optString("folder").ifEmpty { SharedFolders.DEFAULT }
        val path = offer.optString("path")
        if (SharedFolders.roots(ctx).none { it.key == folder } || SharedFolders.cleanPath(path) == null) return reject("bad_folder")
        val arr = offer.optJSONArray("files") ?: return reject("bad_name")
        val names = mutableListOf<String>()
        val sizes = mutableListOf<Long>()
        for (i in 0 until arr.length()) {
            val f = arr.getJSONObject(i)
            names += SharedFolders.sanitize(f.optString("name")) ?: return reject("bad_name")
            sizes += f.optLong("size", -1).takeIf { it >= 0 } ?: return reject("bad_name")
        }
        conn.send(JSONObject().put("type", "file_accept").put("transfer_id", tid))

        val total = sizes.sum()
        var done = 0L
        var where = ""
        val saved = mutableListOf<Saved>()
        val buf = ByteArray(64 * 1024)
        try {
            for (i in names.indices) {
                Hub.transfer(tid, true, peer.name) { it.name = names[i]; it.total = total }
                val inc = SharedFolders.create(ctx, folder, path, names[i])
                where = inc.savedIn
                var ok = false
                try {
                    val digest = MessageDigest.getInstance("SHA-256")
                    var remaining = sizes[i]
                    var lastUi = 0L
                    while (remaining > 0) {
                        val n = conn.input.read(buf, 0, minOf(buf.size.toLong(), remaining).toInt())
                        if (n < 0) throw IOException("the laptop disconnected")
                        digest.update(buf, 0, n)
                        inc.out.write(buf, 0, n)
                        remaining -= n
                        done += n
                        val now = System.currentTimeMillis()
                        if (now - lastUi > 150) {
                            lastUi = now
                            Hub.transfer(tid, true, peer.name) { it.sent = done }
                        }
                    }
                    val end = conn.expect("file_end")
                    if (end.optInt("index") != i || end.optString("sha256") != digest.digest().toHex()) {
                        conn.send(JSONObject().put("type", "error").put("code", "checksum"))
                        throw IOException("the file got damaged on the way (checksum mismatch)")
                    }
                    saved += Saved(inc.finish(), inc.uri, inc.mime, sizes[i])
                    ok = true
                } finally {
                    if (!ok) inc.discard() // never leave half-written files behind
                }
            }
        } catch (e: Exception) {
            Hub.transfer(tid, true, peer.name) { it.state = Hub.State.ERROR; it.message = e.message ?: "failed" }
            Hub.add("error", "Receiving from ${peer.name} failed", e.message ?: "")
            throw e
        }
        val out = JSONArray()
        saved.forEach { out.put(it.name) }
        conn.send(JSONObject().put("type", "transfer_done").put("transfer_id", tid).put("saved", out))
        Hub.transfer(tid, true, peer.name) { it.state = Hub.State.DONE; it.sent = total; it.files = saved.size; it.folder = where }
        val what = saved.singleOrNull()?.name ?: plural(saved.size, "file")
        Hub.add("received", "Received $what", "From ${peer.name} · ${formatSize(total)} · in $where")
        onReceived(Received(peer.name, where, saved))
    }

    /** Lets in only laptops we have paired with; everyone else fails the TLS handshake. */
    private inner class PairedOnly : X509TrustManager {
        override fun checkClientTrusted(chain: Array<out X509Certificate>?, authType: String?) {
            val cert = chain?.firstOrNull() ?: throw CertificateException("no certificate")
            if (DeviceStore.get(ctx, Identity.fingerprintOf(cert)) == null) throw CertificateException("not paired")
        }

        override fun checkServerTrusted(chain: Array<out X509Certificate>?, authType: String?) =
            throw CertificateException("server mode only")

        override fun getAcceptedIssuers(): Array<X509Certificate> = emptyArray()
    }
}
