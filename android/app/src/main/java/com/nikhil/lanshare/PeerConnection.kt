package com.nikhil.lanshare

import org.json.JSONObject
import java.io.BufferedInputStream
import java.io.BufferedOutputStream
import java.io.Closeable
import java.io.DataInputStream
import java.io.DataOutputStream
import java.io.IOException
import java.net.InetSocketAddress
import java.net.Socket
import java.security.cert.CertificateException
import java.security.cert.X509Certificate
import javax.net.ssl.SSLContext
import javax.net.ssl.SSLSocket
import javax.net.ssl.X509TrustManager

/** Thrown when the peer replies with something other than what we expected. */
class ProtocolException(message: String) : IOException(message)

/**
 * One mutual-TLS connection to a peer (PROTOCOL.md §3–§4): framed JSON messages,
 * plus raw byte streams for file contents.
 */
class PeerConnection(private val socket: SSLSocket) : Closeable {
    val input = DataInputStream(BufferedInputStream(socket.inputStream, 64 * 1024))
    val output = DataOutputStream(BufferedOutputStream(socket.outputStream, 64 * 1024))

    /** The peer's friendly name from its hello. */
    var peerName: String = ""

    fun send(msg: JSONObject) {
        val body = msg.toString().toByteArray(Charsets.UTF_8)
        if (body.size > Protocol.MAX_FRAME_SIZE) throw ProtocolException("frame too large")
        output.writeInt(body.size) // DataOutputStream writes big-endian, as the protocol requires
        output.write(body)
        output.flush()
    }

    fun receive(): JSONObject {
        val size = input.readInt()
        if (size < 0 || size > Protocol.MAX_FRAME_SIZE) throw ProtocolException("frame too large")
        val body = ByteArray(size)
        input.readFully(body)
        return JSONObject(String(body, Charsets.UTF_8))
    }

    /** Reads a frame and fails unless its type is one of [expected]. */
    fun expect(vararg expected: String): JSONObject {
        val msg = receive()
        val type = msg.optString("type")
        if (type !in expected) {
            val detail = if (type == "error") msg.optString("code") else type
            throw ProtocolException("unexpected reply: $detail")
        }
        return msg
    }

    fun hello(myName: String) = JSONObject()
        .put("type", "hello").put("v", Protocol.VERSION)
        .put("name", myName).put("platform", "android")

    override fun close() = socket.close()

    companion object {
        private const val IDLE_TIMEOUT_MS = 15_000

        /** Connects, checks the peer's key against [pinnedId], and swaps hellos. */
        fun open(host: String, port: Int, pinnedId: String, myName: String, connectTimeoutMs: Int = 5_000): PeerConnection {
            val socket = tlsContext(PinnedTrust(pinnedId)).socketFactory.createSocket() as SSLSocket
            try {
                socket.enabledProtocols = arrayOf("TLSv1.3")
                socket.connect(InetSocketAddress(host, port), connectTimeoutMs)
                socket.soTimeout = IDLE_TIMEOUT_MS
                socket.startHandshake()
                val conn = PeerConnection(socket)
                conn.send(conn.hello(myName))
                conn.peerName = conn.expect("hello").optString("name")
                return conn
            } catch (e: Exception) {
                socket.close()
                throw e
            }
        }

        /**
         * Connects to [host]:[port] and returns the key fingerprint it presents, without
         * trusting it or sending anything. Used when scanning the network for a device.
         */
        fun probe(host: String, port: Int): String? {
            var seen: String? = null
            val recorder = object : X509TrustManager {
                override fun checkServerTrusted(chain: Array<out X509Certificate>?, authType: String?) {
                    seen = chain?.firstOrNull()?.let { Identity.fingerprintOf(it) }
                    throw CertificateException("probe only") // never continue past the handshake
                }
                override fun checkClientTrusted(chain: Array<out X509Certificate>?, authType: String?) =
                    throw CertificateException("client mode only")
                override fun getAcceptedIssuers(): Array<X509Certificate> = emptyArray()
            }
            val plain = Socket()
            return try {
                plain.connect(InetSocketAddress(host, port), 350)
                val s = tlsContext(recorder).socketFactory.createSocket(plain, host, port, true) as SSLSocket
                s.enabledProtocols = arrayOf("TLSv1.3")
                s.soTimeout = 1_200
                runCatching { s.startHandshake() }
                s.close()
                seen
            } catch (e: IOException) {
                null
            } finally {
                plain.close()
            }
        }

        fun tlsContext(trust: X509TrustManager): SSLContext =
            SSLContext.getInstance("TLSv1.3").apply { init(arrayOf(Identity.keyManager), arrayOf(trust), null) }
    }

    /** Accepts exactly one server key: the one we paired with. No CAs involved. */
    private class PinnedTrust(private val pinnedId: String) : X509TrustManager {
        override fun checkServerTrusted(chain: Array<out X509Certificate>?, authType: String?) {
            val cert = chain?.firstOrNull() ?: throw CertificateException("no certificate")
            if (Identity.fingerprintOf(cert) != pinnedId) {
                throw CertificateException("This is not your paired device (key mismatch)")
            }
        }

        override fun checkClientTrusted(chain: Array<out X509Certificate>?, authType: String?) =
            throw CertificateException("client mode only")

        override fun getAcceptedIssuers(): Array<X509Certificate> = emptyArray()
    }
}
