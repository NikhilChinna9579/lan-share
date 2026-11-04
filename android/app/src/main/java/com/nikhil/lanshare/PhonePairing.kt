package com.nikhil.lanshare

import java.security.MessageDigest
import java.security.SecureRandom

/**
 * Lets another phone pair with this one: a 6-digit code, valid for 2 minutes, one use,
 * at most 5 wrong guesses (the same rules as the laptop's QR code).
 */
object PhonePairing {
    @Volatile private var code: String? = null
    @Volatile private var until = 0L
    @Volatile private var fails = 0
    /** Name of the phone that just paired (for the waiting screen), then null again. */
    @Volatile var justPaired: String? = null

    @Synchronized
    fun start(): String {
        val c = "%06d".format(SecureRandom().nextInt(1_000_000))
        code = c
        until = System.currentTimeMillis() + 120_000
        fails = 0
        justPaired = null
        return c
    }

    @Synchronized
    fun stop() { code = null }

    fun open() = code != null && System.currentTimeMillis() < until

    fun secondsLeft() = ((until - System.currentTimeMillis()) / 1000).coerceAtLeast(0)

    /** null if the code is right (and closes the window); otherwise the reason. */
    @Synchronized
    fun check(given: String): String? {
        val c = code ?: return "not_pairing"
        if (System.currentTimeMillis() > until) return "expired"
        if (!MessageDigest.isEqual(c.toByteArray(), given.trim().toByteArray())) {
            if (++fails >= 5) code = null
            return "bad_code"
        }
        code = null
        return null
    }
}
