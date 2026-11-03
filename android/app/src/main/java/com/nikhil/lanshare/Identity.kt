package com.nikhil.lanshare

import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import java.math.BigInteger
import java.net.Socket
import java.security.KeyPairGenerator
import java.security.KeyStore
import java.security.MessageDigest
import java.security.Principal
import java.security.PrivateKey
import java.security.cert.X509Certificate
import java.security.spec.ECGenParameterSpec
import java.util.Date
import javax.net.ssl.SSLEngine
import javax.net.ssl.X509ExtendedKeyManager
import javax.security.auth.x500.X500Principal

/**
 * This phone's key pair and certificate (PROTOCOL.md §1).
 *
 * The private key is created inside the Android Keystore and can never be read out,
 * not even by this app; the Keystore signs the TLS handshake for us.
 */
object Identity {
    private const val ALIAS = "lanshare_identity"

    private val keyStore: KeyStore by lazy { KeyStore.getInstance("AndroidKeyStore").apply { load(null) } }

    /** Creates the key on first use. */
    fun ensure(deviceName: String) {
        if (keyStore.containsAlias(ALIAS)) return
        val spec = KeyGenParameterSpec.Builder(ALIAS, KeyProperties.PURPOSE_SIGN)
            .setAlgorithmParameterSpec(ECGenParameterSpec("secp256r1"))
            .setDigests(KeyProperties.DIGEST_SHA256, KeyProperties.DIGEST_NONE)
            .setCertificateSubject(X500Principal("CN=" + deviceName.replace(Regex("[,=+<>#;\"\\\\]"), " ")))
            .setCertificateSerialNumber(BigInteger.valueOf(System.currentTimeMillis()))
            .setCertificateNotBefore(Date(System.currentTimeMillis() - 3_600_000L))
            .setCertificateNotAfter(Date(System.currentTimeMillis() + 20L * 365 * 24 * 3_600_000L))
            .build()
        KeyPairGenerator.getInstance(KeyProperties.KEY_ALGORITHM_EC, "AndroidKeyStore").apply {
            initialize(spec)
            generateKeyPair()
        }
    }

    val certificate: X509Certificate get() = keyStore.getCertificate(ALIAS) as X509Certificate

    private val privateKey: PrivateKey get() = keyStore.getKey(ALIAS, null) as PrivateKey

    val fingerprint: String get() = fingerprintOf(certificate)

    /** Lowercase hex SHA-256 of the SubjectPublicKeyInfo — same as the Go side. */
    fun fingerprintOf(cert: X509Certificate): String =
        MessageDigest.getInstance("SHA-256").digest(cert.publicKey.encoded)
            .joinToString("") { "%02x".format(it) }

    /** Always presents our one identity as the TLS client certificate. */
    val keyManager = object : X509ExtendedKeyManager() {
        override fun getClientAliases(keyType: String?, issuers: Array<out Principal>?) = arrayOf(ALIAS)
        override fun chooseClientAlias(keyType: Array<out String>?, issuers: Array<out Principal>?, socket: Socket?) = ALIAS
        override fun chooseEngineClientAlias(keyType: Array<out String>?, issuers: Array<out Principal>?, engine: SSLEngine?) = ALIAS
        override fun getServerAliases(keyType: String?, issuers: Array<out Principal>?): Array<String>? = null
        override fun chooseServerAlias(keyType: String?, issuers: Array<out Principal>?, socket: Socket?): String? = null
        override fun getCertificateChain(alias: String?): Array<X509Certificate> = arrayOf(certificate)
        override fun getPrivateKey(alias: String?): PrivateKey = privateKey
    }
}
