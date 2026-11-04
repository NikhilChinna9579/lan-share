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
 * pings and folder questions, and saves incoming files (PROTOCOL.md §3–§5).
 */
class PhoneServer(private val ctx: Context, private val myName: String, private val onReceived: (Received) -> Unit) {

    class Saved(val name: String, val uri: Uri, val mime: String, val size: Long)
    class Received(val from: String, val folderLabel: String, val files: List<Saved>)

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
        val conn = PeerConnection(s)
        val hello = conn.expect("hello")
        if (hello.optInt("v") != Protocol.VERSION) {
            conn.send(JSONObject().put("type", "error").put("code", "version"))
            return
        }
        conn.send(conn.hello(myName))
        // A laptop that reaches us is at that IP right now: a free hint for its address.
        s.inetAddress?.hostAddress?.let { DeviceStore.updateAddress(ctx, peerId, it, peer.port) }
        while (true) {
            val msg = try {
                conn.receive()
            } catch (e: IOException) {
                return // peer closed the connection: normal end
            }
            when (msg.optString("type")) {
                "ping" -> conn.send(JSONObject().put("type", "pong"))
                "folders_request" -> {
                    val arr = JSONArray()
                    Inbox.FOLDERS.forEach { (k, l) -> arr.put(JSONObject().put("key", k).put("label", l)) }
                    conn.send(JSONObject().put("type", "folders_response").put("default", Inbox.DEFAULT).put("folders", arr))
                }
                "file_offer" -> receive(conn, peer, msg)
                else -> conn.send(JSONObject().put("type", "error").put("code", "unknown_type"))
            }
        }
    }

    private fun receive(conn: PeerConnection, peer: Device, offer: JSONObject) {
        val tid = offer.optString("transfer_id")
        fun reject(reason: String) = conn.send(JSONObject().put("type", "file_reject").put("transfer_id", tid).put("reason", reason))

        val folder = offer.optString("folder").ifEmpty { Inbox.DEFAULT }
        val label = Inbox.FOLDERS.firstOrNull { it.first == folder }?.second ?: return reject("bad_folder")
        val arr = offer.optJSONArray("files") ?: return reject("bad_name")
        val names = mutableListOf<String>()
        val sizes = mutableListOf<Long>()
        for (i in 0 until arr.length()) {
            val f = arr.getJSONObject(i)
            val name = Inbox.sanitize(f.optString("name")) ?: return reject("bad_name")
            val size = f.optLong("size", -1)
            if (size < 0) return reject("bad_name")
            names += name
            sizes += size
        }
        conn.send(JSONObject().put("type", "file_accept").put("transfer_id", tid))

        val saved = mutableListOf<Saved>()
        val buf = ByteArray(64 * 1024)
        for (i in names.indices) {
            val pending = Inbox.create(ctx, folder, names[i])
            var ok = false
            try {
                val digest = MessageDigest.getInstance("SHA-256")
                (ctx.contentResolver.openOutputStream(pending.uri) ?: throw IOException("can't write")).use { out ->
                    var remaining = sizes[i]
                    while (remaining > 0) {
                        val n = conn.input.read(buf, 0, minOf(buf.size.toLong(), remaining).toInt())
                        if (n < 0) throw IOException("connection closed")
                        digest.update(buf, 0, n)
                        out.write(buf, 0, n)
                        remaining -= n
                    }
                }
                val end = conn.expect("file_end")
                if (end.optInt("index") != i || end.optString("sha256") != digest.digest().toHex()) {
                    conn.send(JSONObject().put("type", "error").put("code", "checksum"))
                    throw IOException("checksum mismatch")
                }
                val finalName = Inbox.publish(ctx, pending)
                saved += Saved(finalName, pending.uri, pending.mime, sizes[i])
                ok = true
            } finally {
                if (!ok) Inbox.discard(ctx, pending) // never leave half-written files behind
            }
        }
        val out = JSONArray()
        saved.forEach { out.put(it.name) }
        conn.send(JSONObject().put("type", "transfer_done").put("transfer_id", tid).put("saved", out))
        onReceived(Received(peer.name, label, saved))
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
