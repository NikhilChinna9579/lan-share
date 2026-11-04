package com.nikhil.lanshare

import android.os.Handler
import android.os.Looper
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.CopyOnWriteArrayList

/**
 * What's happening right now, shared by the receiving service and the screen (same
 * process): transfers in both directions, a short activity list, and who reached whom.
 * The screen listens and redraws as things change, so status is always live.
 */
object Hub {
    enum class State { RUNNING, DONE, ERROR }

    class Transfer(
        val id: String, val inbound: Boolean, val device: String,
        @Volatile var name: String = "", @Volatile var sent: Long = 0, @Volatile var total: Long = 0,
        @Volatile var files: Int = 0, @Volatile var state: State = State.RUNNING,
        @Volatile var message: String = "", @Volatile var folder: String = "",
        val started: Long = System.currentTimeMillis(), @Volatile var updated: Long = System.currentTimeMillis(),
    )

    data class Item(val time: Long, val kind: String, val text: String, val detail: String = "")

    private val main = Handler(Looper.getMainLooper())
    private val listeners = CopyOnWriteArrayList<() -> Unit>()
    private val transfers = LinkedHashMap<String, Transfer>()
    private val items = ArrayDeque<Item>()
    @Volatile private var posted = false

    // Reachability, both ways (for "can't connect" diagnosis).
    private val inbound = ConcurrentHashMap<String, Long>()   // laptop id → when it last connected to us
    private val reachOk = ConcurrentHashMap<String, Long>()   // when we last reached it
    private val reachFail = ConcurrentHashMap<String, Long>() // when we last failed to

    fun listen(l: () -> Unit) = listeners.add(l)
    fun unlisten(l: () -> Unit) = listeners.remove(l)

    /** Redraw soon (coalesced: at most ~7 times a second). */
    fun changed() {
        if (posted) return
        posted = true
        main.postDelayed({ posted = false; listeners.forEach { it() } }, 150)
    }

    @Synchronized
    fun transfer(id: String, inbound: Boolean, device: String, update: (Transfer) -> Unit) {
        val t = transfers.getOrPut(id) { Transfer(id, inbound, device) }
        update(t)
        t.updated = System.currentTimeMillis()
        while (transfers.size > 6) transfers.remove(transfers.keys.first())
        changed()
    }

    /** Running transfers, plus finished ones for a few seconds. Newest first. */
    @Synchronized
    fun visibleTransfers(): List<Transfer> {
        val now = System.currentTimeMillis()
        return transfers.values.filter { it.state == State.RUNNING || now - it.updated < 8_000 }.reversed()
    }

    @Synchronized
    fun add(kind: String, text: String, detail: String = "") {
        items.addFirst(Item(System.currentTimeMillis(), kind, text, detail))
        while (items.size > 30) items.removeLast()
        changed()
    }

    @Synchronized
    fun recent(): List<Item> = items.toList()

    fun inboundContact(id: String) {
        val was = inbound.put(id, System.currentTimeMillis())
        if (was == null || System.currentTimeMillis() - was > 15_000) changed()
    }

    fun lastInbound(id: String): Long = inbound[id] ?: 0

    fun reached(id: String, ok: Boolean) {
        (if (ok) reachOk else reachFail)[id] = System.currentTimeMillis()
    }

    /** Can this phone connect to that laptop? null if we don't know (yet). */
    fun canReach(id: String): Boolean? {
        val ok = reachOk[id] ?: 0
        val fail = reachFail[id] ?: 0
        return when {
            fail > ok && System.currentTimeMillis() - fail < 120_000 -> false
            ok > 0 -> true
            else -> null
        }
    }
}
